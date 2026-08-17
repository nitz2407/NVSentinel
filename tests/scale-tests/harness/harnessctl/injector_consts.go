/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

// Shared constants for the in-cluster injector image (slim harness-inject binary)
// and the operator CLI that points pool create / events inject at that image.

const (
	// binInImage is the inject/reconcile binary inside the injector container.
	binInImage = "/usr/local/bin/harness-inject"
	// defaultInjectorImage is the multi-arch image pool create deploys by default.
	defaultInjectorImage = "ghcr.io/nvidia/nvsentinel/harness-inject:latest"
)
