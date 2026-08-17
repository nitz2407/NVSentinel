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
