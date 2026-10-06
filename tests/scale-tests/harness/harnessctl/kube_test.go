//go:build !injector

// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// expiredContinueErr is the error the API server returns once the revision a
// paged walk started at has been compacted: "The provided continue parameter is
// too old to display a consistent list result."
func expiredContinueErr() error {
	return apierrors.NewResourceExpired("The provided continue parameter is too old to display a consistent list result")
}

func TestIsExpiredContinueErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"expired continue token", expiredContinueErr(), true},
		{"gone", apierrors.NewGone("too old"), true},
		{"not found", apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "kwok-gpu-1"), false},
		{"timeout", apierrors.NewTimeoutError("slow", 1), false},
		{"plain error", errors.New("connection reset"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExpiredContinueErr(tc.err); got != tc.want {
				t.Fatalf("isExpiredContinueErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A 50k-node walk needs ~100 pages, long enough for etcd to compact the token
// out from under it. That aborted `pool create` mid-run with "no live KWOK nodes
// found" while 50,000 nodes were Ready, so a restart must recover the count.
func TestWalkWithRestartsRecoversFromExpiredToken(t *testing.T) {
	calls := 0
	got, err := walkWithRestarts("count", func() (int, error) {
		calls++
		if calls <= 2 {
			return 0, expiredContinueErr()
		}
		return 50000, nil
	})
	if err != nil {
		t.Fatalf("expected recovery after restarts, got error: %v", err)
	}
	if got != 50000 {
		t.Fatalf("count = %d, want 50000", got)
	}
	if calls != 3 {
		t.Fatalf("walk called %d times, want 3 (two expired, one success)", calls)
	}
}

// A partial count must never be returned as if it were the whole fleet: the pool
// sizes itself to this number, and N is the denominator of every per-node rate.
func TestWalkWithRestartsGivesUpAfterBoundedRestarts(t *testing.T) {
	calls := 0
	got, err := walkWithRestarts("count", func() (int, error) {
		calls++
		return 17, expiredContinueErr()
	})
	if err == nil {
		t.Fatal("expected an error once restarts are exhausted, got nil")
	}
	if got != 0 {
		t.Fatalf("got %d on failure, want the zero value so no caller mistakes a partial walk for the fleet", got)
	}
	if calls != listRestarts+1 {
		t.Fatalf("walk called %d times, want %d", calls, listRestarts+1)
	}
	if !apierrors.IsResourceExpired(err) {
		t.Fatalf("expected the expiry cause to stay unwrappable, got %v", err)
	}
}

// Any other list failure is the caller's problem to report, not something to
// retry: retrying a permission or connection error just delays the diagnosis.
func TestWalkWithRestartsDoesNotRetryOtherErrors(t *testing.T) {
	calls := 0
	sentinel := apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("nope"))
	_, err := walkWithRestarts("count", func() (int, error) {
		calls++
		return 0, sentinel
	})
	if calls != 1 {
		t.Fatalf("walk called %d times, want 1 (no retry on a non-expiry error)", calls)
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("expected the original error to pass through, got %v", err)
	}
}

// walkWithRestarts is generic so the report's node-stats walk shares it; check a
// non-scalar payload survives both the success and failure paths.
func TestWalkWithRestartsHandlesStructPayload(t *testing.T) {
	calls := 0
	ns, err := walkWithRestarts("report: list nodes", func() (nodeStats, error) {
		calls++
		if calls == 1 {
			return nodeStats{Total: 3}, expiredContinueErr()
		}
		return nodeStats{Total: 50000}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ns.Total != 50000 {
		t.Fatalf("Total = %d, want 50000", ns.Total)
	}
}
