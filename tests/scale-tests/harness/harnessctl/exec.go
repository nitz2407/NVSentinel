/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/


//go:build !injector

package main

// In-cluster execution via the Kubernetes API (client-go remotecommand) — no
// kubectl CLI. Resident injectors run the slim multi-arch harness-inject image;
// inject and reconcile exec the in-image binary at binInImage.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// podExec runs a command inside a pod via the API server exec subresource.
func (c *clients) podExec(ctx context.Context, ns, pod string, stdin io.Reader, command ...string) (string, error) {
	opts := &corev1.PodExecOptions{
		Command: command,
		Stdin:   stdin != nil,
		Stdout:  true,
		Stderr:  true,
	}
	req := c.kube.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(ns).
		SubResource("exec").
		VersionedParams(opts, scheme.ParameterCodec)

	executor, err := newPodExecutor(c, req.URL().String())
	if err != nil {
		return "", err
	}
	var stdout, stderr bytes.Buffer
	streamErr := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	})
	out := stdout.String() + stderr.String()
	if streamErr != nil {
		return out, fmt.Errorf("pod exec %s/%s %v: %w: %s", ns, pod, command, streamErr, strings.TrimSpace(out))
	}
	return out, nil
}

// newPodExecutor prefers WebSocket with SPDY fallback (same strategy as kubectl).
func newPodExecutor(c *clients, rawURL string) (remotecommand.Executor, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	spdy, err := remotecommand.NewSPDYExecutor(c.rest, "POST", u)
	if err != nil {
		return nil, fmt.Errorf("spdy executor: %w", err)
	}
	ws, err := remotecommand.NewWebSocketExecutor(c.rest, "GET", rawURL)
	if err != nil {
		// Older clusters: SPDY only.
		return spdy, nil
	}
	return remotecommand.NewFallbackExecutor(ws, spdy, func(error) bool { return true })
}

// execSh runs a /bin/sh -c script inside a pod.
func (c *clients) execSh(ctx context.Context, ns, pod, script string) (string, error) {
	return c.podExec(ctx, ns, pod, nil, "/bin/sh", "-c", script)
}

// execShEnv runs a /bin/sh -c script inside a pod with extra env vars set.
func (c *clients) execShEnv(ctx context.Context, ns, pod string, env map[string]string, script string) (string, error) {
	args := []string{"env"}
	for k, v := range env {
		args = append(args, k+"="+v)
	}
	args = append(args, "/bin/sh", "-c", script)
	return c.podExec(ctx, ns, pod, nil, args...)
}

// injectorPodsByNode maps each connector node to its resident injector pod (the
// persistent nvs-harness-pool-injector DaemonSet, one Running pod per node).
func (c *clients) injectorPodsByNode(ctx context.Context, ns string) (map[string]string, error) {
	pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: poolInjectorLabel + "=true",
	})
	if err != nil {
		return nil, err
	}
	byNode := map[string]string{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" && podReady(p) {
			byNode[p.Spec.NodeName] = p.Name
		}
	}
	if len(byNode) == 0 {
		return nil, fmt.Errorf("no Running injector pods for %s=true (deploy the pool first)", poolInjectorLabel)
	}
	return byNode, nil
}

func podReady(p *corev1.Pod) bool {
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
