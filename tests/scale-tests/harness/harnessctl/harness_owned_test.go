/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/


//go:build !injector

package main

import "testing"

func TestHarnessManagedName(t *testing.T) {
	owned := []string{connectorPoolName, poolInjectorDaemonSet, poolConfigConfigMap, "nvs-harness-anything"}
	for _, n := range owned {
		if !harnessManagedName(n) {
			t.Errorf("%q should be harness-owned", n)
		}
	}
	forbidden := []string{"platform-connectors", "platform-connector", "mongodb", "", "nvsentinel", "nvs-harness"}
	for _, n := range forbidden {
		if harnessManagedName(n) {
			t.Errorf("%q must NOT be treated as harness-owned", n)
		}
	}
}

func TestRefusePlatformConnectorDeletes(t *testing.T) {
	if err := refuseIfNotHarnessManaged("DaemonSet", "platform-connectors"); err == nil {
		t.Fatal("expected refuse for platform-connectors")
	}
	if err := refuseIfNotHarnessManaged("DaemonSet", poolInjectorDaemonSet); err != nil {
		t.Fatalf("injector DS should be allowed: %v", err)
	}
	if err := refuseIfPlatformTemplate(poolInjectorDaemonSet, "platform-connectors"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if err := refuseIfPlatformTemplate("platform-connectors", "platform-connectors"); err == nil {
		t.Fatal("expected refuse when name equals template")
	}
}

func TestPoolObjectNamesAreHarnessOwned(t *testing.T) {
	for _, n := range []string{connectorPoolName, poolInjectorDaemonSet, poolConfigConfigMap} {
		if err := refuseIfNotHarnessManaged("object", n); err != nil {
			t.Fatalf("pool constant %q must stay under %q: %v", n, harnessOwnedPrefix, err)
		}
	}
}
