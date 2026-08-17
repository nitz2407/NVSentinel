/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package assets

import "embed"

// FS holds bringup Helm values and KWOK stage manifests (same pattern as
// dgxcops embedding RBAC/Job templates). Keep in sync with the sibling copies
// under tests/scale-tests/harness/{nvsentinel,monitoring,kwok}/.
//
//go:embed monitoring/values-kube-prometheus-stack.yaml
//go:embed nvsentinel/values-harness.yaml
//go:embed kwok/stages-custom.yaml
var FS embed.FS
