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
	"os"
	"path/filepath"
	"time"

	"github.com/nvidia/nvsentinel/tests/scale-tests/harness/harnessctl/assets"
	"gopkg.in/yaml.v3"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	"helm.sh/helm/v3/pkg/storage/driver"
	"helm.sh/helm/v3/pkg/strvals"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// helmSettings returns Helm CLI settings (same kubeconfig/context as the operator shell).
func helmSettings() *cli.EnvSettings {
	return cli.New()
}

// helmActionConfig initializes a Helm action.Configuration for ns.
func helmActionConfig(ns string) (*action.Configuration, *cli.EnvSettings, error) {
	settings := helmSettings()
	// Match helm CLI `-n`: ConfigFlags.Namespace backs DefaultNamespace() for
	// manifests that omit metadata.namespace. Without this, those objects land
	// in "default" while Release.Namespace (and explicit-ns templates) use ns.
	settings.SetNamespace(ns)
	cfg := new(action.Configuration)
	if err := cfg.Init(settings.RESTClientGetter(), ns, os.Getenv("HELM_DRIVER"), func(format string, v ...interface{}) {
		infof("helm: "+format, v...)
	}); err != nil {
		return nil, nil, fmt.Errorf("helm action config: %w", err)
	}
	regClient, err := registry.NewClient()
	if err != nil {
		return nil, nil, fmt.Errorf("helm registry client: %w", err)
	}
	cfg.RegistryClient = regClient
	return cfg, settings, nil
}

// helmRepoAddUpdate adds a chart repo (idempotent) and refreshes its index.
func helmRepoAddUpdate(_ context.Context, name, url string) error {
	settings := helmSettings()
	repoFile := settings.RepositoryConfig
	if err := os.MkdirAll(filepath.Dir(repoFile), 0o755); err != nil {
		return err
	}
	f := repo.NewFile()
	if b, err := os.ReadFile(repoFile); err == nil {
		_ = yaml.Unmarshal(b, f)
	}
	entry := &repo.Entry{Name: name, URL: url}
	if f.Has(name) {
		f.Update(entry)
	} else {
		f.Add(entry)
	}
	if err := f.WriteFile(repoFile, 0o644); err != nil {
		return fmt.Errorf("write helm repos: %w", err)
	}
	r, err := repo.NewChartRepository(entry, getter.All(settings))
	if err != nil {
		return err
	}
	if _, err := r.DownloadIndexFile(); err != nil {
		return fmt.Errorf("helm repo update %s: %w", name, err)
	}
	infof("helm repo %s ready (%s)", name, url)
	return nil
}

func locateChart(cfg *action.Configuration, settings *cli.EnvSettings, chartRef, version string) (*chart.Chart, error) {
	// NewInstall copies cfg.RegistryClient onto ChartPathOptions (required for OCI).
	inst := action.NewInstall(cfg)
	inst.Version = version
	path, err := inst.ChartPathOptions.LocateChart(chartRef, settings)
	if err != nil {
		return nil, fmt.Errorf("locate chart %s: %w", chartRef, err)
	}
	ch, err := loader.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load chart %s: %w", path, err)
	}
	return ch, nil
}

// helmUpgradeInstall runs helm upgrade --install via the Helm Go SDK.
func helmUpgradeInstall(ctx context.Context, releaseName, chartRef, ns, version string, vals map[string]interface{}, takeOwnership bool, timeout time.Duration) (string, error) {
	cfg, settings, err := helmActionConfig(ns)
	if err != nil {
		return "", err
	}

	ch, err := locateChart(cfg, settings, chartRef, version)
	if err != nil {
		return "", err
	}

	hist := action.NewHistory(cfg)
	hist.Max = 1
	_, histErr := hist.Run(releaseName)
	missing := histErr == driver.ErrReleaseNotFound

	if missing {
		infof("helm install %s (%s) ns=%s version=%s", releaseName, chartRef, ns, version)
		inst := action.NewInstall(cfg)
		inst.ReleaseName = releaseName
		inst.Namespace = ns
		inst.Wait = true
		inst.Timeout = timeout
		inst.Version = version
		inst.TakeOwnership = takeOwnership
		inst.CreateNamespace = false
		rel, err := inst.RunWithContext(ctx, ch, vals)
		if err != nil {
			return err.Error(), err
		}
		return rel.Info.Description, nil
	}

	infof("helm upgrade %s (%s) ns=%s version=%s", releaseName, chartRef, ns, version)
	up := action.NewUpgrade(cfg)
	up.Namespace = ns
	up.Wait = true
	up.Timeout = timeout
	up.Version = version
	up.TakeOwnership = takeOwnership
	rel, err := up.RunWithContext(ctx, releaseName, ch, vals)
	if err != nil {
		return err.Error(), err
	}
	return rel.Info.Description, nil
}

// helmStatus returns the release status string, or "" if missing.
func helmStatus(_ context.Context, releaseName, ns string) (string, error) {
	cfg, _, err := helmActionConfig(ns)
	if err != nil {
		return "", err
	}
	st := action.NewStatus(cfg)
	rel, err := st.Run(releaseName)
	if err != nil {
		if err == driver.ErrReleaseNotFound {
			return "", nil
		}
		return "", err
	}
	return string(rel.Info.Status), nil
}

// helmRollback rolls back a release to the previous revision.
func helmRollback(_ context.Context, releaseName, ns string) error {
	cfg, _, err := helmActionConfig(ns)
	if err != nil {
		return err
	}
	rb := action.NewRollback(cfg)
	rb.Wait = true
	rb.Timeout = 5 * time.Minute
	if err := rb.Run(releaseName); err != nil {
		rb.Version = 0
		return rb.Run(releaseName)
	}
	return nil
}

// loadEmbeddedValues unmarshals an embedded Helm values YAML into a map.
func loadEmbeddedValues(name string) (map[string]interface{}, error) {
	b, err := assets.FS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("embed %s: %w", name, err)
	}
	return unmarshalValuesYAML(b, "embed:"+name)
}

// loadValuesFile unmarshals a Helm values YAML from disk into a map.
func loadValuesFile(path string) (map[string]interface{}, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read values %s: %w", path, err)
	}
	return unmarshalValuesYAML(b, path)
}

func unmarshalValuesYAML(b []byte, src string) (map[string]interface{}, error) {
	var vals map[string]interface{}
	if err := yaml.Unmarshal(b, &vals); err != nil {
		return nil, fmt.Errorf("parse values %s: %w", src, err)
	}
	if vals == nil {
		vals = map[string]interface{}{}
	}
	return vals, nil
}

// mergeValuesMaps deep-merges src into dst (Helm -f overlay semantics: src wins).
func mergeValuesMaps(dst, src map[string]interface{}) map[string]interface{} {
	if dst == nil {
		dst = map[string]interface{}{}
	}
	for k, v := range src {
		if vMap, ok := asStringMap(v); ok {
			if dMap, ok := asStringMap(dst[k]); ok {
				dst[k] = mergeValuesMaps(dMap, vMap)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}

func asStringMap(v interface{}) (map[string]interface{}, bool) {
	switch m := v.(type) {
	case map[string]interface{}:
		return m, true
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(m))
		for k, val := range m {
			ks, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[ks] = val
		}
		return out, true
	default:
		return nil, false
	}
}

// loadNVSValues loads embedded harness values, then merges optional
// --nvsentinel-values overlays.
func loadNVSValues(cfg Config) (map[string]interface{}, error) {
	return loadValuesWithOverlays("nvsentinel/values-harness.yaml", cfg.NVSentinelValuesFiles)
}

// loadMonitoringValues loads embedded kube-prometheus-stack values, then merges
// optional --monitoring-values overlays.
func loadMonitoringValues(cfg Config) (map[string]interface{}, error) {
	return loadValuesWithOverlays("monitoring/values-kube-prometheus-stack.yaml", cfg.MonitoringValuesFiles)
}

func loadValuesWithOverlays(embedded string, overlays []string) (map[string]interface{}, error) {
	vals, err := loadEmbeddedValues(embedded)
	if err != nil {
		return nil, err
	}
	for _, path := range overlays {
		infof("values overlay: %s", path)
		overlay, err := loadValuesFile(path)
		if err != nil {
			return nil, err
		}
		vals = mergeValuesMaps(vals, overlay)
	}
	return vals, nil
}

// readEmbedded returns an embedded asset's bytes.
func readEmbedded(name string) ([]byte, error) {
	b, err := assets.FS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("embed %s: %w", name, err)
	}
	return b, nil
}

// mergeSet merges --set style key=value pairs into vals.
func mergeSet(vals map[string]interface{}, set ...string) error {
	for _, s := range set {
		if err := strvals.ParseInto(s, vals); err != nil {
			return err
		}
	}
	return nil
}

// ensureNamespace creates ns if it does not already exist.
func (c *clients) ensureNamespace(ctx context.Context, ns string) error {
	_, err := c.kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	infof("creating namespace %s", ns)
	_, err = c.kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{})
	return err
}

// waitDeployRollout waits until a Deployment has rolled out, or timeout.
func (c *clients) waitDeployRollout(ctx context.Context, ns, name string, timeout time.Duration) error {
	ok, err := c.waitRolloutComplete(ctx, ns, "deployment", name, timeout)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("timeout waiting for deploy/%s in %s", name, ns)
	}
	infof("ready: deploy/%s in %s", name, ns)
	return nil
}

// waitUntil retries fn until it returns true/nil or the timeout elapses.
func waitUntil(ctx context.Context, timeout, interval time.Duration, desc string, fn func() error) error {
	deadline := time.Now().Add(timeout)
	infof("waiting (<=%s) for: %s", timeout, desc)
	var last error
	for {
		if err := fn(); err == nil {
			infof("ready: %s", desc)
			return nil
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			if last != nil {
				return fmt.Errorf("timeout after %s waiting for %s: %w", timeout, desc, last)
			}
			return fmt.Errorf("timeout after %s waiting for %s", timeout, desc)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// resolveInstallVersion returns target if set, otherwise the baked-in default
// used when bringup actually needs to install a missing component.
func resolveInstallVersion(target, fallback string) string {
	if target != "" {
		return target
	}
	return fallback
}
