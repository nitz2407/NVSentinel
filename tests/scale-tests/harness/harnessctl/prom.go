//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

type promVectorSample struct {
	Metric map[string]string
	Value  float64
}

// promInstantQuery runs a Prometheus instant query through the API server
// service proxy (no port-forward, no curl/jq). Returns the first sample's scalar
// value, or (0, false) if there is no data.
func (c *clients) promInstantQuery(ctx context.Context, cfg Config, query string) (float64, bool) {
	samples := c.promQueryVector(ctx, cfg, query)
	if len(samples) == 0 {
		return 0, false
	}
	return samples[0].Value, true
}

func (c *clients) promQueryVector(ctx context.Context, cfg Config, query string) []promVectorSample {
	raw, err := c.kube.CoreV1().
		Services(cfg.MonitoringNamespace).
		ProxyGet("http", cfg.MonPromSvc, cfg.MonPromPort, "/api/v1/query", map[string]string{"query": query}).
		DoRaw(ctx)
	if err != nil {
		warnf("prometheus query failed (%s:%s in %s): %v", cfg.MonPromSvc, cfg.MonPromPort, cfg.MonitoringNamespace, err)
		return nil
	}
	samples, err := parsePromQueryResponse(raw)
	if err != nil {
		warnf("prometheus response parse error: %v", err)
		return nil
	}
	return samples
}

func parsePromQueryResponse(raw []byte) ([]promVectorSample, error) {
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"` // [ <ts float>, "<value string>" ]
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return nil, nil
	}
	out := make([]promVectorSample, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		if len(r.Value) != 2 {
			continue
		}
		s, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		out = append(out, promVectorSample{Metric: r.Metric, Value: f})
	}
	return out, nil
}

const apiserverP99Query = `histogram_quantile(0.99, sum(rate(apiserver_request_duration_seconds_bucket{verb!~"WATCH|CONNECT"}[5m])) by (le))`

// apiserverP99WindowQuery is apiserverP99Query with a caller-chosen rate window,
// so a ceiling sweep can use a short (responsive) window per step.
func apiserverP99WindowQuery(window string) string {
	return fmt.Sprintf(`histogram_quantile(0.99, sum(rate(apiserver_request_duration_seconds_bucket{verb!~"WATCH|CONNECT"}[%s])) by (le))`, window)
}

// apiserverResourceP99Query is the p99 for one verb+resource (e.g. LIST nodes —
// the heaviest large-collection read, the leading indicator of the etcd/read
// ceiling on a managed control plane where etcd metrics are not exposed).
func apiserverResourceP99Query(verb, resource, window string) string {
	return fmt.Sprintf(`histogram_quantile(0.99, sum(rate(apiserver_request_duration_seconds_bucket{verb="%s",resource="%s"}[%s])) by (le))`, verb, resource, window)
}

// kwokControllerCPUQuery returns the KWOK controller's CPU (cores) so a sweep can
// attribute "nodes not going Ready" to KWOK-controller saturation (a harness
// limit) rather than the API server.
func kwokControllerCPUQuery(window string) string {
	return fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{namespace="kube-system",pod=~"kwok-controller.*",container!="",container!="POD"}[%s]))`, window)
}

func fmtFloat(f float64, ok bool) string {
	if !ok {
		return "unknown"
	}
	return fmt.Sprintf("%.3f", f)
}
