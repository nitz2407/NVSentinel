/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/


//go:build !injector

package main

import (
	"compress/gzip"
	"context"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestIsRetryableTeardownErr(t *testing.T) {
	nodes := schema.GroupResource{Resource: "nodes"}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "context canceled", err: context.Canceled, want: false},
		{name: "context deadline", err: context.DeadlineExceeded, want: false},
		{
			// The failure that used to abort cleanup: a proxied delete-collection
			// whose compressed response body arrives corrupt. client-go wraps the
			// raw gzip error with no Status attached.
			name: "corrupt gzip response body",
			err: fmt.Errorf("unexpected error when reading response body. Please retry. Original error: %w",
				gzip.ErrHeader),
			want: true,
		},
		{
			name: "server timeout after clearing a batch",
			err:  apierrors.NewServerTimeout(nodes, "deletecollection", 1),
			want: true,
		},
		{
			name: "expired continue token",
			err:  apierrors.NewResourceExpired("continue token expired"),
			want: true,
		},
		{
			name: "throttled",
			err:  apierrors.NewTooManyRequests("slow down", 1),
			want: true,
		},
		{
			// A real rejection: retrying can never clear it, so teardown must not
			// spin on it.
			name: "forbidden",
			err:  apierrors.NewForbidden(nodes, "kwok-gpu-0", fmt.Errorf("nope")),
			want: false,
		},
		{
			name: "not found",
			err:  apierrors.NewNotFound(nodes, "kwok-gpu-0"),
			want: false,
		},
		{
			name: "invalid",
			err:  apierrors.NewInvalid(schema.GroupKind{Kind: "Node"}, "kwok-gpu-0", nil),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableTeardownErr(tc.err); got != tc.want {
				t.Errorf("isRetryableTeardownErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A wrapped transport failure must stay retryable, and a wrapped Status must stay
// fatal, so the classification survives the %w wrapping the call path adds.
func TestIsRetryableTeardownErrThroughWrapping(t *testing.T) {
	transport := fmt.Errorf("teardown nodes: %w",
		fmt.Errorf("reading body: %w", gzip.ErrHeader))
	if !isRetryableTeardownErr(transport) {
		t.Error("wrapped transport failure should be retryable")
	}

	forbidden := fmt.Errorf("teardown nodes: %w",
		apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", fmt.Errorf("nope")))
	if isRetryableTeardownErr(forbidden) {
		t.Error("wrapped Forbidden should not be retryable")
	}
}

func TestIsRetryableTeardownErrStatusWithUnknownReason(t *testing.T) {
	// A Status the server sent but that carries no reason we treat as transient
	// is a genuine rejection, not a transport hiccup.
	err := &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    400,
		Message: "bad request",
	}}
	if isRetryableTeardownErr(err) {
		t.Error("unrecognized Status should not be retryable")
	}
}
