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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// podMetrics lists PodMetrics for ns matching labelSelector via metrics.k8s.io.
func (c *clients) podMetrics(ctx context.Context, ns, labelSelector string) ([]metricsv1beta1.PodMetrics, error) {
	mc, err := metricsclient.NewForConfig(c.rest)
	if err != nil {
		return nil, err
	}
	list, err := mc.MetricsV1beta1().PodMetricses(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

// nodeMetricsAvailable returns nil when metrics.k8s.io can serve node metrics.
func (c *clients) nodeMetricsAvailable(ctx context.Context) error {
	mc, err := metricsclient.NewForConfig(c.rest)
	if err != nil {
		return err
	}
	_, err = mc.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return fmt.Errorf("node metrics: %w", err)
	}
	return nil
}
