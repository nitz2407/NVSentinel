//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Cursor reset: make the remediation consumers forget where they were in the
// health-event change stream, so a run only remediates the events that run
// injects.
//
// Why a run needs this. fault-quarantine, node-drainer and
// health-events-analyzer are change-stream consumers: each stores a resume
// token and picks up from it on restart. That is right in production and wrong
// for a scale rung, because KWOK node names are deterministic (kwok-gpu-N), so
// a fatal event stored by an earlier run matches a node the next run creates
// under the same name. Measured on this harness, a fleet carrying 399k stored
// events replayed into 4,158 cordons that no run had asked for, at a rate
// (~1.7/s) that the janitor could not drain (~0.02/s) — so the fleet drifted
// further from a clean starting state the longer it ran, and the cordon counts
// in the report belonged to previous runs.
//
// This was invisible until the fault-quarantine circuit breaker was disabled:
// while it was tripped nothing was dequeued at all, so the backlog accumulated
// silently across every run.
//
// How the reset works. NVSentinel already supports this through the shared
// `resume-control` ConfigMap: setting a consumer's key to CREATE makes it
// delete its resume token on next startup and cold-start from a cutoff of
// "now", then flip its own key back to RESUME. Driving that documented
// handshake — rather than deleting token documents out of MongoDB — means no
// database credentials, no TLS setup, and the component decides its own cutoff,
// so the harness cannot leave a half-reset cursor behind.
//
// The restart is required, not incidental: the token is read once at startup,
// so a running consumer would keep serving its open change stream (and could
// checkpoint the old position back).

// cursorConsumers are the change-stream consumers whose position determines
// which events a run remediates. fault-remediation has a resume-control key too
// but is included only if asked for, since it does not drive cordons or CRs.
var cursorConsumers = []string{"fault-quarantine", "node-drainer", "health-events-analyzer"}

const resumeControlConfigMap = "resume-control"

// runResetCursor cold-starts the remediation consumers at "now".
//
// Run it before injecting, so the cutoff each consumer picks predates this
// run's events but postdates every earlier run's.
func runResetCursor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("events reset-cursor", flag.ExitOnError)
	cfg := defaultConfig()
	bindNvsNamespaceFlag(fs, &cfg)
	list := fs.String("consumers", strings.Join(cursorConsumers, ","),
		"comma-separated change-stream consumers to cold-start")
	timeout := fs.Duration("timeout", 5*time.Minute, "per-consumer rollout timeout")
	_ = fs.Parse(args)

	c, err := newClients(cfg)
	if err != nil {
		return err
	}

	var names []string
	for _, n := range strings.Split(*list, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("no consumers given")
	}

	var failed []string
	for _, name := range names {
		stepf("reset-cursor: %s", name)
		if err := c.resetConsumerCursor(ctx, cfg, name, *timeout); err != nil {
			warnf("%s: %v", name, err)
			failed = append(failed, name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("cursor reset failed for %s: a run started now would remediate earlier runs' events",
			strings.Join(failed, ", "))
	}
	infof("cursor reset complete for %d consumer(s); each will remediate only events generated from now on", len(names))
	return nil
}

// resetConsumerCursor requests CREATE, restarts the consumer, and confirms the
// request was consumed.
func (c *clients) resetConsumerCursor(ctx context.Context, cfg Config, name string, timeout time.Duration) error {
	if err := c.setResumeControlMode(ctx, cfg.NVSNamespace, name, "CREATE"); err != nil {
		return err
	}
	if _, err := c.rolloutRestart(ctx, cfg.NVSNamespace, "deployment", name); err != nil {
		return err
	}
	ok, err := c.waitRolloutComplete(ctx, cfg.NVSNamespace, "deployment", name, timeout)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("did not become ready within %s", timeout)
	}

	// The consumer flips its own key back to RESUME once it has deleted the
	// token. Still CREATE means the restart did not reach that code, so the old
	// position may survive — worth failing on rather than assuming a clean run.
	mode, err := c.resumeControlMode(ctx, cfg.NVSNamespace, name)
	if err != nil {
		return err
	}
	if strings.EqualFold(mode, "CREATE") {
		return fmt.Errorf("resume-control still CREATE after restart: token may not have been deleted")
	}
	infof("%s cold-started (resume-control now %s)", name, mode)
	return nil
}

func (c *clients) setResumeControlMode(ctx context.Context, ns, client, mode string) error {
	cm, err := c.kube.CoreV1().ConfigMaps(ns).Get(ctx, resumeControlConfigMap, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Absent means no consumer has ever recorded a mode, so there is nothing
		// to forget; create it anyway so the CREATE request is visible at startup.
		_, err = c.kube.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: resumeControlConfigMap, Namespace: ns},
			Data:       map[string]string{client: mode},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create %s: %w", resumeControlConfigMap, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s: %w", resumeControlConfigMap, err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[client] = mode
	if _, err := c.kube.CoreV1().ConfigMaps(ns).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update %s: %w", resumeControlConfigMap, err)
	}
	return nil
}

func (c *clients) resumeControlMode(ctx context.Context, ns, client string) (string, error) {
	cm, err := c.kube.CoreV1().ConfigMaps(ns).Get(ctx, resumeControlConfigMap, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("get %s: %w", resumeControlConfigMap, err)
	}
	return cm.Data[client], nil
}
