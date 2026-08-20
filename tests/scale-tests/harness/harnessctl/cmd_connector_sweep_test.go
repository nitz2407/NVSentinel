//go:build !injector

/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package main

import (
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseMaxConnectionCount(t *testing.T) {
	jsonLog := `{"t":{"$date":"2026-07-30T12:00:00.000Z"},"s":"I","c":"NETWORK","id":22943,"msg":"Connection accepted","attr":{"connectionId":12,"connectionCount":153}}
{"attr":{"connectionCount":160}}`
	n, ok := parseMaxConnectionCount(jsonLog)
	if !ok || n != 160 {
		t.Fatalf("json logs: got %d ok=%v, want 160", n, ok)
	}

	textLog := `2026-07-30T12:00:00.000+0000 I NETWORK [listener] connection accepted from 10.0.0.1:1234 #42 (connectionCount:77)`
	n, ok = parseMaxConnectionCount(textLog)
	if !ok || n != 77 {
		t.Fatalf("text logs: got %d ok=%v, want 77", n, ok)
	}

	if _, ok := parseMaxConnectionCount("no connections here"); ok {
		t.Fatal("empty logs must not report a count")
	}
}

func TestIsMongoDBServerPod(t *testing.T) {
	sts := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "mongodb-1", Labels: map[string]string{"app.kubernetes.io/name": "mongodb"}}}
	if !isMongoDBServerPod(sts) {
		t.Fatal("mongodb-1 must count as a server pod")
	}
	job := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:   "mongodb-create-mongodb-database-abc",
		Labels: map[string]string{"app.kubernetes.io/name": "mongodb", "job-name": "create-mongodb-database"},
	}}
	if isMongoDBServerPod(job) {
		t.Fatal("job pods must be excluded")
	}
	arbiter := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:   "mongodb-arbiter-0",
		Labels: map[string]string{"app.kubernetes.io/name": "mongodb", "app.kubernetes.io/component": "arbiter"},
	}}
	if isMongoDBServerPod(arbiter) {
		t.Fatal("arbiter pods must be excluded")
	}
}

func TestMongoServerContainer(t *testing.T) {
	p := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: "metrics"},
		{Name: "mongodb"},
	}}}
	if got := mongoServerContainer(p); got != "mongodb" {
		t.Fatalf("got %q, want mongodb", got)
	}
}

func TestNanoCoresToMilli(t *testing.T) {
	if got := nanoCoresToMilli(268521173); got != 269 {
		t.Fatalf("nanoCoresToMilli= %d, want 269", got)
	}
	if got := nanoCoresToMilli(0); got != 0 {
		t.Fatalf("zero nano = %d", got)
	}
}

func TestLookupMongoUsage(t *testing.T) {
	raw := []byte(`{
	  "pods": [{
	    "podRef": {"name": "mongodb-0", "namespace": "nvsentinel"},
	    "cpu": {"usageNanoCores": 268521173},
	    "memory": {"workingSetBytes": 3441160192},
	    "containers": [
	      {"name": "mongodb", "cpu": {"usageNanoCores": 173306811}, "memory": {"workingSetBytes": 3282108416}},
	      {"name": "metrics", "cpu": {"usageNanoCores": 146556181}, "memory": {"workingSetBytes": 183029760}}
	    ]
	  }]
	}`)
	var sum kubeletSummary
	if err := json.Unmarshal(raw, &sum); err != nil {
		t.Fatal(err)
	}
	u, ok := lookupMongoUsage(&sum, "nvsentinel", "mongodb-0")
	if !ok {
		t.Fatal("expected mongodb-0 in summary")
	}
	if u.cpuMilli != 269 {
		t.Fatalf("pod milli=%d, want 269", u.cpuMilli)
	}
	if u.mongodMilli != 173 {
		t.Fatalf("mongod milli=%d, want 173", u.mongodMilli)
	}
	if u.memMi != 3281 {
		t.Fatalf("pod memMi=%d, want 3281 (workingSetBytes/Mi)", u.memMi)
	}
}

func TestCPULimitMilli(t *testing.T) {
	p := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "mongodb",
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1500m")},
		},
	}}}}
	if got := cpuLimitMilli(p, "mongodb"); got != 1500 {
		t.Fatalf("limit=%d, want 1500", got)
	}
}

func TestParseLookback(t *testing.T) {
	if d := parseLookback("5m", 30); d != 5*time.Minute {
		t.Fatalf("window 5m settle 30: got %s", d)
	}
	if d := parseLookback("bogus", 90); d != 5*time.Minute {
		t.Fatalf("invalid window falls back to 5m: got %s", d)
	}
	if d := parseLookback("1m", 90); d != 120*time.Second {
		t.Fatalf("settle+30s must widen a short window: got %s", d)
	}
}
