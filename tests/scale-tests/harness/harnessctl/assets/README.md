# Embedded bringup assets

These YAML files are baked into the `harnessctl` binary via `go:embed` (same
approach as `dgxcops` CLI templates). `stack bringup` reads them from the
binary — no `--assets-dir` or separate install of YAML is required.

Keep in sync with:

- `../monitoring/values-kube-prometheus-stack.yaml`
- `../nvsentinel/values-harness.yaml`
- `../kwok/stages-custom.yaml`
