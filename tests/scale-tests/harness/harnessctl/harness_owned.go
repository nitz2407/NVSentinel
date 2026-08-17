/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/


//go:build !injector

package main

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// harnessOwnedPrefix is the ownership boundary for every object harnessctl
// creates or deletes for the connector-pool experiments. The live platform
// connector DaemonSet (default: platform-connectors) is chart-owned and MUST
// never be scaled, patched, or deleted by any harnessctl command — it is only
// read as a pod-template source.
const harnessOwnedPrefix = "nvs-harness-"

// harnessManagedName reports whether name is in the harnessctl ownership
// namespace. All connector-pool create/delete paths must use these names.
func harnessManagedName(name string) bool {
	return strings.HasPrefix(name, harnessOwnedPrefix)
}

// refuseIfNotHarnessManaged blocks destructive ops on anything outside the
// harness ownership prefix (including platform-connectors).
func refuseIfNotHarnessManaged(kind, name string) error {
	if name == "" {
		return fmt.Errorf("refusing to delete %s with empty name", kind)
	}
	if !harnessManagedName(name) {
		return fmt.Errorf("refusing to delete %s %q: not harness-owned (must start with %q); live platform connectors are never cleaned up by harnessctl",
			kind, name, harnessOwnedPrefix)
	}
	return nil
}

// refuseIfPlatformTemplate blocks deleting the DaemonSet used only as a
// clone template, even if a misconfiguration somehow put it under the harness
// prefix.
func refuseIfPlatformTemplate(name, template string) error {
	if template != "" && name == template {
		return fmt.Errorf("refusing to delete %q: that is the platform connector template DaemonSet (--connector-daemonset); harnessctl never removes it", name)
	}
	return nil
}

func ignoreNotFound(err error) error {
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (c *clients) deleteHarnessDaemonSet(ctx context.Context, ns, name, platformTemplate string, pol metav1.DeletionPropagation) error {
	if err := refuseIfNotHarnessManaged("DaemonSet", name); err != nil {
		return err
	}
	if err := refuseIfPlatformTemplate(name, platformTemplate); err != nil {
		return err
	}
	return ignoreNotFound(c.kube.AppsV1().DaemonSets(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &pol}))
}

func (c *clients) deleteHarnessStatefulSet(ctx context.Context, ns, name string, pol metav1.DeletionPropagation) error {
	if err := refuseIfNotHarnessManaged("StatefulSet", name); err != nil {
		return err
	}
	return ignoreNotFound(c.kube.AppsV1().StatefulSets(ns).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &pol}))
}

func (c *clients) deleteHarnessService(ctx context.Context, ns, name string) error {
	if err := refuseIfNotHarnessManaged("Service", name); err != nil {
		return err
	}
	return ignoreNotFound(c.kube.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{}))
}

func (c *clients) deleteHarnessConfigMap(ctx context.Context, ns, name string) error {
	if err := refuseIfNotHarnessManaged("ConfigMap", name); err != nil {
		return err
	}
	return ignoreNotFound(c.kube.CoreV1().ConfigMaps(ns).Delete(ctx, name, metav1.DeleteOptions{}))
}
