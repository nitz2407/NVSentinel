/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

const kwokNodeLabel = "type=kwok"

type clients struct {
	rest    *rest.Config
	kube    *kubernetes.Clientset
	dynamic dynamic.Interface
}

// newClients builds a REST config from in-cluster config (when running as a Job)
// or the local kubeconfig with an optional context override (operator CLI).
func newClients(cfg Config) (*clients, error) {
	var restCfg *rest.Config
	var err error

	if inCluster, e := rest.InClusterConfig(); e == nil {
		restCfg = inCluster
	} else {
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		overrides := &clientcmd.ConfigOverrides{}
		if cfg.KubeContext != "" {
			overrides.CurrentContext = cfg.KubeContext
		}
		restCfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
	}
	// Give the scaler real headroom; the API server, not the client, should be
	// the thing under test.
	restCfg.QPS = 200
	restCfg.Burst = 400

	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("kube client: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("dynamic client: %w", err)
	}
	return &clients{rest: restCfg, kube: kube, dynamic: dyn}, nil
}

// buildNode constructs a GPU-shaped fake KWOK node object for the given index.
func buildNode(cfg Config, idx int) *corev1.Node {
	name := fmt.Sprintf("%s-%d", cfg.NodePrefix, idx)
	gpu := fmt.Sprintf("%d", cfg.GPUCount)
	capacity := corev1.ResourceList{
		corev1.ResourceCPU:                    resource.MustParse(cfg.NodeCPU),
		corev1.ResourceMemory:                 resource.MustParse(cfg.NodeMemory),
		corev1.ResourcePods:                   resource.MustParse(fmt.Sprintf("%d", cfg.NodeMaxPods)),
		corev1.ResourceName("nvidia.com/gpu"): resource.MustParse(gpu),
	}
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Annotations: map[string]string{
				"kwok.x-k8s.io/node":           "fake",
				"node.alpha.kubernetes.io/ttl": "0",
			},
			Labels: map[string]string{
				"type":                                "kwok",
				"kubernetes.io/hostname":              name,
				"kubernetes.io/os":                    "linux",
				"kubernetes.io/arch":                  "amd64",
				"node-role.kubernetes.io/worker":      "",
				"nvidia.com/gpu.present":              "true",
				"nvidia.com/gpu.count":                gpu,
				"nvidia.com/gpu.product":              "NVIDIA-H100-80GB-HBM3",
				"nvidia.com/gpu.deploy.driver":        "true",
				"nvidia.com/gpu.deploy.dcgm":          "true",
				"nvidia.com/gpu.deploy.device-plugin": "true",
			},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{
				Key:    "kwok.x-k8s.io/node",
				Value:  "fake",
				Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Status: corev1.NodeStatus{
			Capacity:    capacity,
			Allocatable: capacity,
			NodeInfo: corev1.NodeSystemInfo{
				Architecture:    "amd64",
				OperatingSystem: "linux",
				KubeletVersion:  "kwok",
				BootID:          "00000000-0000-0000-0000-000000000000",
			},
			Phase: corev1.NodeRunning,
		},
	}
}

// scaleNodes creates simulated nodes up to cfg.NodeCount with bounded
// concurrency. Idempotent: existing nodes are skipped. Returns (created,
// skipped, error-count).
func (c *clients) scaleNodes(ctx context.Context, cfg Config) (int, int, int) {
	existing := c.countKwokNodes(ctx)
	infof("existing kwok nodes: %d; target: %d", existing, cfg.NodeCount)
	if existing >= cfg.NodeCount {
		return 0, existing, 0
	}

	conc := cfg.NodeBatch
	if conc < 1 {
		conc = 100
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var created, skipped, failed int64

	for i := existing; i < cfg.NodeCount; i++ {
		select {
		case <-ctx.Done():
			warnf("scale interrupted at index %d", i)
			wg.Wait()
			return int(created), int(skipped), int(failed)
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.createOneNode(ctx, cfg, idx); err != nil {
				if apierrors.IsAlreadyExists(err) {
					atomic.AddInt64(&skipped, 1)
					return
				}
				atomic.AddInt64(&failed, 1)
				if failed < 10 {
					warnf("create node %d: %v", idx, err)
				}
				return
			}
			n := atomic.AddInt64(&created, 1)
			if n%2000 == 0 {
				infof("  created %d/%d nodes", existing+int(n), cfg.NodeCount)
			}
		}(i)
	}
	wg.Wait()
	return int(created), int(skipped), int(failed)
}

func (c *clients) createOneNode(ctx context.Context, cfg Config, idx int) error {
	node := buildNode(cfg, idx)
	if _, err := c.kube.CoreV1().Nodes().Create(ctx, node, metav1.CreateOptions{}); err != nil {
		return err
	}
	// Best-effort: ensure capacity/allocatable/nodeInfo are persisted (status is
	// not honored on create by every API server). The KWOK controller starts
	// mutating the node (Ready condition + lease) the instant it is created, so
	// UpdateStatus must GET the latest revision and retry on conflict. A failure
	// here does NOT mean the node failed — it already exists and KWOK will make
	// it Ready — so we log and move on rather than counting it as a create error.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := c.kube.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		cur.Status.Capacity = node.Status.Capacity
		cur.Status.Allocatable = node.Status.Allocatable
		cur.Status.NodeInfo = node.Status.NodeInfo
		_, err = c.kube.CoreV1().Nodes().UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		return err
	}); err != nil {
		warnf("node %s created but status touch-up failed (non-fatal): %v", node.Name, err)
	}
	return nil
}

func (c *clients) countKwokNodes(ctx context.Context) int {
	list, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: kwokNodeLabel})
	if err != nil {
		return 0
	}
	return len(list.Items)
}

// waitNodesReady uses a label-scoped node informer (single LIST + WATCH) and
// polls the in-memory lister until `target` nodes are Ready or timeout elapses.
// Returns (readyCount, ok).
func (c *clients) waitNodesReady(ctx context.Context, target int, timeout time.Duration) (int, bool) {
	factory := informers.NewSharedInformerFactoryWithOptions(
		c.kube, 0,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = kwokNodeLabel }),
	)
	nodeInformer := factory.Core().V1().Nodes()
	lister := nodeInformer.Lister()
	stop := make(chan struct{})
	defer close(stop)
	factory.Start(stop)
	if !cache.WaitForCacheSync(stop, nodeInformer.Informer().HasSynced) {
		warnf("node informer cache did not sync; falling back to direct list")
	}

	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	last := 0
	for {
		ready := countReady(lister)
		if ready != last {
			infof("kwok nodes Ready: %d/%d", ready, target)
			last = ready
		}
		if ready >= target {
			return ready, true
		}
		if time.Now().After(deadline) {
			return ready, false
		}
		select {
		case <-ctx.Done():
			return ready, false
		case <-tick.C:
		}
	}
}

func countReady(lister interface {
	List(labels.Selector) ([]*corev1.Node, error)
}) int {
	nodes, err := lister.List(labels.Everything())
	if err != nil {
		return 0
	}
	ready := 0
	for _, n := range nodes {
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				ready++
				break
			}
		}
	}
	return ready
}

func (c *clients) teardownNodes(ctx context.Context) error {
	return c.kube.CoreV1().Nodes().DeleteCollection(ctx,
		metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: kwokNodeLabel},
	)
}

// simulateReboot flips a node NotReady, waits, then Ready with a fresh bootID —
// the reboot the janitor's post-reboot reconciliation keys on.
func (c *clients) simulateReboot(ctx context.Context, node string, down time.Duration, newBootID string) error {
	notReady := []byte(`{"status":{"conditions":[{"type":"Ready","status":"False","reason":"NodeRebooting","message":"harnessctl: rebooting","lastHeartbeatTime":"` + metav1.Now().Format(time.RFC3339) + `","lastTransitionTime":"` + metav1.Now().Format(time.RFC3339) + `"}]}}`)
	if _, err := c.kube.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, notReady, metav1.PatchOptions{}, "status"); err != nil {
		return fmt.Errorf("mark NotReady: %w", err)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(down):
	}
	ready := []byte(`{"status":{"nodeInfo":{"bootID":"` + newBootID + `"},"conditions":[{"type":"Ready","status":"True","reason":"KubeletReady","message":"harnessctl: rebooted","lastHeartbeatTime":"` + metav1.Now().Format(time.RFC3339) + `","lastTransitionTime":"` + metav1.Now().Format(time.RFC3339) + `"}]}}`)
	if _, err := c.kube.CoreV1().Nodes().Patch(ctx, node, types.StrategicMergePatchType, ready, metav1.PatchOptions{}, "status"); err != nil {
		return fmt.Errorf("mark Ready: %w", err)
	}
	return nil
}

func (c *clients) nodeBootID(ctx context.Context, node string) string {
	n, err := c.kube.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return n.Status.NodeInfo.BootID
}

func (c *clients) firstKwokNode(ctx context.Context) (string, error) {
	list, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: kwokNodeLabel, Limit: 1})
	if err != nil {
		return "", err
	}
	if len(list.Items) == 0 {
		return "", fmt.Errorf("no kwok nodes found (run scale-nodes first)")
	}
	return list.Items[0].Name, nil
}

// firstRealNode returns a node WITHOUT the type=kwok label (a real node).
func (c *clients) firstRealNode(ctx context.Context) (string, error) {
	list, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, n := range list.Items {
		if n.Labels["type"] != "kwok" {
			return n.Name, nil
		}
	}
	return "", fmt.Errorf("no real (non-kwok) node found")
}

func (c *clients) labelNode(ctx context.Context, node, key, value string) error {
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:%q}}}`, key, value))
	_, err := c.kube.CoreV1().Nodes().Patch(ctx, node, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// mongoConn is a resolved reconciler connection: a seed URI plus optional TLS
// secret + auth settings, letting P0.3 target plain, TLS, or mTLS/X.509 stores.
type mongoConn struct {
	uri           string // seed URI (FQDN host so it matches server-cert SANs)
	tlsSecret     string // secret mounted at /etc/mongo-certs (empty = no TLS)
	authMechanism string // e.g. MONGODB-X509 ("" = URI-embedded creds)
	authSource    string // e.g. $external
}

// deriveMongoConn discovers how the reconciler should reach MongoDB so the suite
// runs unchanged on any cluster:
//   - If the cert-manager client secret exists (chart default), use mTLS + X.509
//     (auth via the client cert subject, no password in the URI).
//   - Otherwise fall back to root SCRAM over a plain connection.
//
// HARNESS_MONGO_URI, when set, is used verbatim (TLS still auto-attaches if the
// client secret is present, so a creds-less override URI works).
func (c *clients) deriveMongoConn(ctx context.Context, cfg Config) (mongoConn, error) {
	var mc mongoConn
	fqdn := fmt.Sprintf("%s.%s.svc.cluster.local", cfg.MongoService, cfg.NVSNamespace)
	rsParam := ""
	if cfg.MongoReplicaSet != "" {
		rsParam = "&replicaSet=" + cfg.MongoReplicaSet
	}

	useTLS := false
	if cfg.MongoTLSSecret != "" {
		if _, err := c.kube.CoreV1().Secrets(cfg.NVSNamespace).Get(ctx, cfg.MongoTLSSecret, metav1.GetOptions{}); err == nil {
			useTLS = true
			mc.tlsSecret = cfg.MongoTLSSecret
			mc.authMechanism = "MONGODB-X509"
			mc.authSource = "$external"
		}
	}

	if cfg.MongoURI != "" {
		mc.uri = cfg.MongoURI
		return mc, nil
	}

	if useTLS {
		mc.uri = fmt.Sprintf("mongodb://%s:%s/?tls=true%s", fqdn, cfg.MongoPort, rsParam)
		return mc, nil
	}

	pw, err := c.mongoRootPassword(ctx, cfg)
	if err != nil {
		return mongoConn{}, err
	}
	mc.uri = fmt.Sprintf("mongodb://root:%s@%s:%s/?authSource=admin%s", pw, fqdn, cfg.MongoPort, rsParam)
	return mc, nil
}

// mongoRootPassword reads a root password from the datastore secret for non-TLS
// installs, tolerating the common key names across chart variants.
func (c *clients) mongoRootPassword(ctx context.Context, cfg Config) (string, error) {
	sec, err := c.kube.CoreV1().Secrets(cfg.NVSNamespace).Get(ctx, cfg.MongoRootSecret, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get secret %s: %w", cfg.MongoRootSecret, err)
	}
	for _, k := range []string{"mongodb-root-password", "root-password", "mongodb-password", "password"} {
		if v, ok := sec.Data[k]; ok && len(v) > 0 {
			return string(v), nil
		}
	}
	return "", fmt.Errorf("secret %s has no known root-password key", cfg.MongoRootSecret)
}

// ---- dynamic CR helpers (janitor.dgxc.nvidia.com/v1alpha1) ----

var (
	rebootGVR   = schema.GroupVersionResource{Group: "janitor.dgxc.nvidia.com", Version: "v1alpha1", Resource: "rebootnodes"}
	gpuresetGVR = schema.GroupVersionResource{Group: "janitor.dgxc.nvidia.com", Version: "v1alpha1", Resource: "gpuresets"}
)

func (c *clients) applyRebootNode(ctx context.Context, name, node string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "janitor.dgxc.nvidia.com/v1alpha1",
		"kind":       "RebootNode",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"force": true, "nodeName": node},
	}}
	_, err := c.dynamic.Resource(rebootGVR).Create(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (c *clients) applyGPUReset(ctx context.Context, name, node string) error {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "janitor.dgxc.nvidia.com/v1alpha1",
		"kind":       "GPUReset",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"nodeName": node},
	}}
	_, err := c.dynamic.Resource(gpuresetGVR).Create(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

// conditionStatus returns the status of a named condition in a CR's .status.conditions.
func (c *clients) conditionStatus(ctx context.Context, gvr schema.GroupVersionResource, name, condType string) (string, error) {
	obj, err := c.dynamic.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	conds, found, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if !found {
		return "", nil
	}
	for _, cc := range conds {
		m, ok := cc.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == condType {
			if s, ok := m["status"].(string); ok {
				return s, nil
			}
		}
	}
	return "", nil
}

func (c *clients) phase(ctx context.Context, gvr schema.GroupVersionResource, name string) (string, error) {
	obj, err := c.dynamic.Resource(gvr).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	s, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	return s, nil
}
