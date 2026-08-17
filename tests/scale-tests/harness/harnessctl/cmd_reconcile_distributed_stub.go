//go:build injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"fmt"
)

func runReconcileDistributed(context.Context, Config, string) error {
	return fmt.Errorf("distributed reconcile requires the operator harnessctl binary (pass -direct for in-cluster primitives)")
}
