/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/


//go:build !injector

package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestComputePoolSizing(t *testing.T) {
	tests := []struct {
		name                                     string
		emulated, realNodes, perNode             int
		wantReplicas, wantNodesPerConn, wantMult int
		wantCeiling                              int
		wantCeilingReached                       bool
	}{
		{
			name:     "capped at pod ceiling with rate multiplier",
			emulated: 50000, realNodes: 18, perNode: 50,
			wantReplicas: 900, wantNodesPerConn: 56, wantMult: 56,
			wantCeiling: 900, wantCeilingReached: true,
		},
		{
			name:     "under ceiling: one connector per node",
			emulated: 100, realNodes: 18, perNode: 50,
			wantReplicas: 100, wantNodesPerConn: 1, wantMult: 1,
			wantCeiling: 900, wantCeilingReached: false,
		},
		{
			name:     "low density cap shrinks the pool",
			emulated: 10000, realNodes: 32, perNode: 5,
			wantReplicas: 160, wantNodesPerConn: 63, wantMult: 63,
			wantCeiling: 160, wantCeilingReached: true,
		},
		{
			name:     "zero real nodes guarded to 1",
			emulated: 10, realNodes: 0, perNode: 50,
			wantReplicas: 10, wantNodesPerConn: 1, wantMult: 1,
			wantCeiling: 50, wantCeilingReached: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := computePoolSizing(tc.emulated, tc.realNodes, tc.perNode)
			if s.RealConnectors != tc.wantReplicas {
				t.Errorf("replicas = %d, want %d", s.RealConnectors, tc.wantReplicas)
			}
			if s.NodesPerConnector != tc.wantNodesPerConn {
				t.Errorf("nodesPerConn = %d, want %d", s.NodesPerConnector, tc.wantNodesPerConn)
			}
			if s.RateMultiplier != tc.wantMult {
				t.Errorf("rateMultiplier = %d, want %d", s.RateMultiplier, tc.wantMult)
			}
			if s.PodCeiling != tc.wantCeiling {
				t.Errorf("podCeiling = %d, want %d", s.PodCeiling, tc.wantCeiling)
			}
			if s.PodCeilingReached != tc.wantCeilingReached {
				t.Errorf("ceilingReached = %v, want %v", s.PodCeilingReached, tc.wantCeilingReached)
			}
			// Every emulated node must be covered: replicas * nodesPerConn >= emulated.
			if s.RealConnectors*s.NodesPerConnector < s.EmulatedNodes {
				t.Errorf("coverage gap: %d*%d < %d", s.RealConnectors, s.NodesPerConnector, s.EmulatedNodes)
			}
		})
	}
}

func TestConnectorSocket(t *testing.T) {
	containers := []corev1.Container{{
		Name: "platform-connector",
		Args: []string{"--config=/etc/config/config.json", "--socket=/var/run/nvsentinel.sock"},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "var-run-vol", MountPath: "/var/run"},
			{Name: "cfg", MountPath: "/etc/config"},
		},
	}}
	socket, mount := connectorSocket(containers)
	if socket != "/var/run/nvsentinel.sock" {
		t.Errorf("socket = %q", socket)
	}
	if mount.Name != "var-run-vol" || mount.MountPath != "/var/run" {
		t.Errorf("mount = %+v, want var-run-vol@/var/run", mount)
	}
}
