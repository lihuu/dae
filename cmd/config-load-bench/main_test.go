// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunMeasuresCopiedConfigMerge(t *testing.T) {
	dir := t.TempDir()
	routing := filepath.Join(dir, "routing.dae")
	if err := os.WriteFile(routing, []byte("routing {\n    domain(example.test) -> proxy\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.dae")
	if err := os.WriteFile(config, []byte("global {\n}\n\ninclude {\n    routing.dae\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := run(&out, options{ConfigPath: config, Runs: 2}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"source=",
		"runs=2",
		"entries=2",
		"sections=3",
		"merge_ms",
		"summary merge_ms",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q:\n%s", want, got)
		}
	}
}

func TestRunRejectsMissingConfig(t *testing.T) {
	var out bytes.Buffer
	if err := run(&out, options{}); err == nil {
		t.Fatal("run without config succeeded")
	}
}
