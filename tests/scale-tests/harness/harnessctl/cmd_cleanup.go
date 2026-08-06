/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

// Self-contained cleanup. Prior runs leave two kinds of debris that used to be
// cleared by hand: simulated KWOK nodes, and orphaned janitor CRs (GPUReset /
// RebootNode) that reference now-deleted nodes and get wedged in Terminating by
// the janitor's validating webhook. `harnessctl cleanup` clears both (and,
// optionally, the connector pool) with no manual kubectl patching, so every
// phase command starts from a clean slate.

import (
	"context"
	"flag"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

func runCleanup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stack cleanup", flag.ExitOnError)
	cfg := defaultConfig()
	bindNvsNamespaceFlag(fs, &cfg)
	bindKwokNamespaceFlag(fs, &cfg)
	bindJanitorNamespaceFlag(fs, &cfg)
	nodes := fs.Bool("nodes", true, "delete all simulated KWOK nodes")
	crs := fs.Bool("crs", true, "garbage-collect orphaned janitor CRs (GPUReset/RebootNode referencing deleted nodes)")
	pool := fs.Bool("pool", true, "also tear down the connector pool + resident injectors (use --pool=false to keep it)")
	_ = fs.Parse(args)

	c, err := newClients(cfg)
	if err != nil {
		return err
	}

	if *pool {
		stepf("cleanup: connector pool")
		_ = c.teardownConnectorPool(ctx, cfg)
	}
	if *nodes {
		stepf("cleanup: KWOK nodes")
		if err := c.deleteAllKwokNodes(ctx, 20*time.Minute); err != nil {
			return err
		}
		stepf("cleanup: restart kwok-controller (clear stale lease cache)")
		if err := c.restartKwokController(ctx, cfg); err != nil {
			warnf("kwok-controller restart failed (non-fatal): %v", err)
		}
	}
	if *crs {
		stepf("cleanup: orphaned janitor CRs")
		removed, err := c.gcOrphanedJanitorCRs(ctx, cfg)
		if err != nil {
			warnf("CR GC incomplete: %v", err)
		}
		infof("garbage-collected %d orphaned janitor CR(s)", removed)
	}
	infof("cleanup complete")
	return nil
}

// deleteAllKwokNodes deletes every simulated node and blocks until none remain
// (or timeout). A single DeleteCollection only clears the batch it processes
// before the apiserver request timeout (tens of thousands of nodes never fit in
// one request), so this re-issues the collection delete until the fleet is
// actually gone — each pass returns a server timeout after clearing a batch,
// which is expected and non-fatal.
func (c *clients) deleteAllKwokNodes(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		n := c.countKwokNodes(ctx)
		if n == 0 {
			infof("all kwok nodes deleted")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out deleting kwok nodes: %d remain", n)
		}
		infof("deleting %d kwok nodes (delete-collection; cascades fake pods)", n)
		if err := c.teardownNodes(ctx); err != nil {
			// Expected mid-delete conditions at scale, all safe to retry with a
			// fresh list: request timeout (batch cleared, ran out of time),
			// service unavailable, or 410 Gone (paginated list's continue token
			// expired due to concurrent node deletion).
			if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
				apierrors.IsServiceUnavailable(err) || apierrors.IsResourceExpired(err) {
				warnf("delete-collection cleared a batch but did not complete (%s) — retrying remaining",
					apierrors.ReasonForError(err))
			} else {
				return fmt.Errorf("teardown nodes: %w", err)
			}
		}
	}
}

// restartKwokController rollout-restarts the kwok-controller so the next run
// starts with a clean informer cache. After a large node churn (e.g. deleting a
// 50k fleet) the controller's in-memory lease cache goes stale: it spins on
// UID-precondition failures syncing leases for now-deleted nodes and starves
// heartbeat renewal for freshly created ones, capping how many nodes it can keep
// Ready (observed: only ~1.7k of 5k Ready until restarted). Best-effort — a
// failed restart or wait must not fail cleanup.
func (c *clients) restartKwokController(ctx context.Context, cfg Config) error {
	patch := []byte(fmt.Sprintf(
		`{"spec":{"template":{"metadata":{"annotations":{"harnessctl/restartedAt":%q}}}}}`,
		time.Now().Format(time.RFC3339)))
	if _, err := c.kube.AppsV1().Deployments(cfg.KWOKNamespace).Patch(
		ctx, "kwok-controller", types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patch kwok-controller: %w", err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		d, err := c.kube.AppsV1().Deployments(cfg.KWOKNamespace).Get(ctx, "kwok-controller", metav1.GetOptions{})
		if err == nil && d.Generation == d.Status.ObservedGeneration &&
			d.Status.UpdatedReplicas == d.Status.Replicas &&
			d.Status.AvailableReplicas == d.Status.Replicas && d.Status.Replicas > 0 {
			infof("kwok-controller restarted (clean cache)")
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	infof("kwok-controller restart issued (rollout still settling)")
	return nil
}

// gcOrphanedJanitorCRs deletes every GPUReset/RebootNode whose spec.nodeName no
// longer resolves to a live node. Each is wedged the same way: the janitor
// finalizer can't be removed while the referenced node is absent (the validating
// webhook denies it). Rather than the per-CR node-recreate dance (seconds each →
// hours for thousands), it recreates all referenced nodes ONCE in bulk, then
// clears finalizers + deletes every CR in bounded parallel, then removes the
// placeholders. Returns the number cleared; real-node remediations are untouched.
func (c *clients) gcOrphanedJanitorCRs(ctx context.Context, cfg Config) (int, error) {
	nodeList, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("list nodes: %w", err)
	}
	exist := make(map[string]struct{}, len(nodeList.Items))
	for i := range nodeList.Items {
		exist[nodeList.Items[i].Name] = struct{}{}
	}

	type gvrOrphans struct {
		gvr     schema.GroupVersionResource
		orphans []orphanCR
	}
	var batches []gvrOrphans
	nodeSet := map[string]struct{}{}
	total := 0
	for _, gvr := range []schema.GroupVersionResource{gpuresetGVR, rebootGVR} {
		orphans := c.listOrphanCRs(ctx, gvr, exist)
		if len(orphans) == 0 {
			continue
		}
		infof("%s: %d orphaned CR(s) to remove", gvr.Resource, len(orphans))
		batches = append(batches, gvrOrphans{gvr, orphans})
		total += len(orphans)
		for _, o := range orphans {
			if o.node != "" {
				nodeSet[o.node] = struct{}{}
			}
		}
	}
	if total == 0 {
		return 0, nil
	}

	created := c.ensurePlaceholderNodesBulk(ctx, cfg, nodeSet)
	defer c.deleteNodesBulk(ctx, created)
	for _, b := range batches {
		c.deleteCRsBulk(ctx, b.gvr, b.orphans)
	}
	return total, nil
}

// orphanCR is an orphaned janitor CR and the (now-deleted) node it references.
type orphanCR struct{ name, node string }

// cleanupConcurrency bounds the parallel API sweeps during orphan GC; the client
// QPS/Burst (see newClients) is set high enough that this is the real limit.
const cleanupConcurrency = 50

// ensurePlaceholderNodesBulk creates a placeholder KWOK node for each referenced
// name that doesn't already exist, in bounded parallel, so the webhook admits the
// subsequent finalizer removal. Returns the names it actually created (to delete).
func (c *clients) ensurePlaceholderNodesBulk(ctx context.Context, cfg Config, names map[string]struct{}) []string {
	type res struct {
		name    string
		created bool
	}
	ch := make(chan res, len(names))
	sem := make(chan struct{}, cleanupConcurrency)
	var wg sync.WaitGroup
	for name := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ch <- res{name, c.ensurePlaceholderNode(ctx, cfg, name)}
		}(name)
	}
	wg.Wait()
	close(ch)
	var created []string
	for r := range ch {
		if r.created {
			created = append(created, r.name)
		}
	}
	infof("created %d placeholder node(s) to unwedge finalizers", len(created))
	return created
}

// deleteCRsBulk clears finalizers (admitted now the nodes exist) and deletes every
// CR in bounded parallel. A CR already carrying a deletionTimestamp is removed the
// instant its finalizer clears; the explicit Delete covers any that don't.
func (c *clients) deleteCRsBulk(ctx context.Context, gvr schema.GroupVersionResource, orphans []orphanCR) {
	nullFinalizers := []byte(`{"metadata":{"finalizers":null}}`)
	sem := make(chan struct{}, cleanupConcurrency)
	var wg sync.WaitGroup
	var done int64
	for _, o := range orphans {
		wg.Add(1)
		go func(o orphanCR) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_, _ = c.dynamic.Resource(gvr).Patch(ctx, o.name, types.MergePatchType, nullFinalizers, metav1.PatchOptions{})
			_ = c.dynamic.Resource(gvr).Delete(ctx, o.name, metav1.DeleteOptions{})
			atomic.AddInt64(&done, 1)
		}(o)
	}
	wg.Wait()
	infof("%s: cleared finalizers + deleted %d CR(s)", gvr.Resource, done)
}

// deleteNodesBulk deletes the given nodes in bounded parallel.
func (c *clients) deleteNodesBulk(ctx context.Context, names []string) {
	if len(names) == 0 {
		return
	}
	sem := make(chan struct{}, cleanupConcurrency)
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_ = c.kube.CoreV1().Nodes().Delete(ctx, n, metav1.DeleteOptions{})
		}(n)
	}
	wg.Wait()
	infof("removed %d placeholder node(s)", len(names))
}

// ensurePlaceholderNode creates a minimal KWOK node with the given name if it's
// absent, returning whether it created one (so the caller can delete it after).
func (c *clients) ensurePlaceholderNode(ctx context.Context, cfg Config, name string) bool {
	if _, err := c.kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{}); err == nil {
		return false
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{"kwok.x-k8s.io/node": "fake"},
			Labels:      map[string]string{"type": "kwok", "kubernetes.io/hostname": name},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{Key: "kwok.x-k8s.io/node", Value: "fake", Effect: corev1.TaintEffectNoSchedule}},
		},
	}
	if cfg.ProviderIDScheme != "" {
		node.Spec.ProviderID = cfg.ProviderIDScheme + "://" + name
	}
	if _, err := c.kube.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
		warnf("recreate placeholder node %s: %v", name, err)
		return false
	}
	return true
}

// listOrphanCRs returns CRs of the given kind whose spec.nodeName is not a
// currently-existing node, paired with that node name.
func (c *clients) listOrphanCRs(ctx context.Context, gvr schema.GroupVersionResource, exist map[string]struct{}) []orphanCR {
	var out []orphanCR
	cont := ""
	for {
		list, err := c.dynamic.Resource(gvr).List(ctx, metav1.ListOptions{Limit: 500, Continue: cont})
		if err != nil {
			return out
		}
		for i := range list.Items {
			obj := &list.Items[i]
			node, _, _ := unstructured.NestedString(obj.Object, "spec", "nodeName")
			if node == "" {
				continue
			}
			if _, ok := exist[node]; ok {
				continue
			}
			out = append(out, orphanCR{name: obj.GetName(), node: node})
		}
		cont = list.GetContinue()
		if cont == "" {
			break
		}
	}
	return out
}
