/*
Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/


//go:build !injector

package main

import "testing"

func TestSumAcked(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want int
	}{
		{"none", "nothing here", 0},
		{"single", "done: sent=200 acked=200", 200},
		{"multi", "done: sent=200 acked=198\ndone: sent=200 acked=200\n", 398},
		{"interleaved", "x acked=5 y\nz acked=10\n", 15},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sumAcked(tc.out); got != tc.want {
				t.Fatalf("sumAcked(%q) = %d, want %d", tc.out, got, tc.want)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"simple", "simple"},
		{"-flag=value", "-flag=value"},
		{"has space", "'has space'"},
		{"mongodb://root:p@w$d@h:27017", `'mongodb://root:p@w$d@h:27017'`},
		{"it's", `'it'\''s'`},
	}
	for _, tc := range cases {
		if got := shellQuote(tc.in); got != tc.want {
			t.Fatalf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestShellQuoteRun(t *testing.T) {
	got := shellQuoteRun(binInImage, []string{"reconcile", "-run-id=p03-1", "-uri=mongodb://a:b@h/?x=1&y=2"})
	want := binInImage + " reconcile -run-id=p03-1 '-uri=mongodb://a:b@h/?x=1&y=2'"
	if got != want {
		t.Fatalf("shellQuoteRun = %q, want %q", got, want)
	}
}

func TestReconcileArgsTLS(t *testing.T) {
	cfg := Config{MongoDB: "db", MongoColl: "coll", FieldPrefix: "healthevent", RunLabel: "r", IDLabel: "i", MaxLossFrac: 0}
	conn := mongoConn{uri: "mongodb://h", tlsSecret: "s", authMechanism: "MONGODB-X509", authSource: "$external"}
	args := reconcileArgs(cfg, conn, "run1")
	joined := shellQuoteRun(binInImage, args)
	for _, want := range []string{"-run-id=run1", "-tls-cert-dir=/etc/mongo-certs", "-auth-mechanism=MONGODB-X509", "-db=db"} {
		if !contains(joined, want) {
			t.Fatalf("reconcileArgs missing %q in %q", want, joined)
		}
	}

	// Plain (no TLS) must not emit TLS flags.
	plain := shellQuoteRun(binInImage, reconcileArgs(cfg, mongoConn{uri: "mongodb://h"}, "run2"))
	if contains(plain, "tls-cert-dir") {
		t.Fatalf("plain reconcileArgs unexpectedly set TLS: %q", plain)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// Doing: lock -count to event volume, independent of fleet size.
//
// injectAcrossPool used to hardcode each connector's count to the number of
// nodes it represented, so the total was always the fleet size and -count was
// silently discarded. A spec asking for 40000 events against 1000 nodes got
// 1000, and reconcile -expect-injected=40000 then failed the run at 97.5%
// loss.
func TestShardEventCountSplitsRequestedVolume(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		total, totalConn, npc int
		wantPerConn, wantSum  int
	}{{
		name: "divides evenly", total: 40000, totalConn: 1000, npc: 1,
		wantPerConn: 40, wantSum: 40000,
	}, {
		name: "volume below fleet", total: 100, totalConn: 100, npc: 10,
		wantPerConn: 1, wantSum: 100,
	}, {
		// Uniform COUNT per connector means the total rounds up.
		name: "rounds up when indivisible", total: 1000, totalConn: 32, npc: 1,
		wantPerConn: 32, wantSum: 1024,
	}, {
		// Unset count keeps the fleet-storm shape the scale rungs rely on.
		name: "unset falls back to one per node", total: 0, totalConn: 100, npc: 10,
		wantPerConn: 10, wantSum: 1000,
	}, {
		name: "negative treated as unset", total: -1, totalConn: 8, npc: 4,
		wantPerConn: 4, wantSum: 32,
	}, {
		name: "no connectors", total: 500, totalConn: 0, npc: 1,
		wantPerConn: 0, wantSum: 0,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			perConn, sum := shardEventCount(tc.total, tc.totalConn, tc.npc)
			if perConn != tc.wantPerConn || sum != tc.wantSum {
				t.Fatalf("shardEventCount(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tc.total, tc.totalConn, tc.npc, perConn, sum, tc.wantPerConn, tc.wantSum)
			}
		})
	}
}

// Doing: the reported expect must be what was sent, not what was asked for.
// reconcile fails a run on any gap between expect and stored, so reporting the
// unrounded request would fail every run whose count is not divisible by the
// connector count.
func TestShardEventCountExpectMatchesWhatIsSent(t *testing.T) {
	perConn, expect := shardEventCount(1000, 32, 1)
	if expect != perConn*32 {
		t.Fatalf("expect %d is not perConn %d x 32 connectors", expect, perConn)
	}
	if expect < 1000 {
		t.Fatalf("expect %d under-counts the 1000 requested; reconcile would see phantom surplus", expect)
	}
}
