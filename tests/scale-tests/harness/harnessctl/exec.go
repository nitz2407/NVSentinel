/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

// Image-free in-cluster execution. Everything the harness needs to run inside
// the cluster (stage the binary, inject, reconcile) is driven by exec-ing the
// resident injector DaemonSet pods (stock alpine) and running the LOCALLY BUILT
// harnessctl binary staged onto the node hostPath. This replaces the manual
// `kubectl cp` / `kubectl exec` / port-forward steps and the harnessctl image:
// the operator runs one command per phase and the harness reaches into the
// cluster itself. It shells out to kubectl (already required by the install
// scripts and always present operator-side) honoring the configured context, so
// no extra client-go transport dependencies are pulled in.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// binOnNode is where the staged harnessctl binary lives on each connector node's
// hostPath (mounted into the injector pod at /pool-sockets). It survives injector
// pod restarts and is reused by every later inject/reconcile.
const binOnNode = "/pool-sockets/bin/harnessctl"

// kubectl runs kubectl against the caller's current context and returns combined
// output. The context is caller-managed; the harness never passes --context.
func (c *clients) kubectl(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return out.String(), nil
}

// execSh runs a /bin/sh -c script inside a pod (image-free node work).
func (c *clients) execSh(ctx context.Context, ns, pod, script string) (string, error) {
	return c.kubectl(ctx, nil, "-n", ns, "exec", pod, "--", "/bin/sh", "-c", script)
}

// execShEnv runs a /bin/sh -c script inside a pod with extra env vars set.
func (c *clients) execShEnv(ctx context.Context, ns, pod string, env map[string]string, script string) (string, error) {
	args := []string{"-n", ns, "exec", pod, "--", "env"}
	for k, v := range env {
		args = append(args, k+"="+v)
	}
	args = append(args, "/bin/sh", "-c", script)
	return c.kubectl(ctx, nil, args...)
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

// localBinarySum returns the sha256 of the local binary path.
func localBinarySum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// stageBinaryToInjector copies the local harnessctl binary into one injector pod's
// node hostPath (binOnNode), idempotently: it first checks the on-node sha256 and
// skips the copy when already current. The copy goes to a .tmp path then does an
// atomic chmod+mv so a torn binary is never left behind, and the staged sum is
// re-verified to catch a truncated transfer.
func (c *clients) stageBinaryToInjector(ctx context.Context, ns, pod, localBin, wantSum string, force bool) error {
	if !force {
		have, _ := c.execSh(ctx, ns, pod, "sha256sum "+binOnNode+" 2>/dev/null | awk '{print $1}'")
		if strings.TrimSpace(have) == wantSum {
			return nil
		}
	}
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		if _, err := c.execSh(ctx, ns, pod, "mkdir -p /pool-sockets/bin"); err != nil {
			lastErr = err
		} else if _, err := c.kubectl(ctx, nil, "-n", ns, "cp", localBin, pod+":"+binOnNode+".tmp"); err != nil {
			lastErr = err
		} else if _, err := c.execSh(ctx, ns, pod,
			fmt.Sprintf("chmod +x %s.tmp && mv -f %s.tmp %s && test -x %s", binOnNode, binOnNode, binOnNode, binOnNode)); err != nil {
			lastErr = err
		} else {
			got, _ := c.execSh(ctx, ns, pod, "sha256sum "+binOnNode+" 2>/dev/null | awk '{print $1}'")
			if strings.TrimSpace(got) == wantSum {
				return nil
			}
			lastErr = fmt.Errorf("staged sum mismatch on %s (got %q want %q)", pod, strings.TrimSpace(got), wantSum)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt*5) * time.Second):
		}
	}
	return fmt.Errorf("stage binary to %s failed after retries: %w", pod, lastErr)
}

// resolveLocalBinary finds the linux/amd64 harnessctl binary to stage into the
// injectors. Preference: explicit override, else the running executable (the
// operator builds harnessctl for linux/amd64, which the alpine injector can run).
func resolveLocalBinary(override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err != nil {
			return "", fmt.Errorf("harness binary %q: %w", override, err)
		}
		return override, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate self binary: %w", err)
	}
	return self, nil
}
