// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/antlr/antlr4/runtime/Go/antlr/v4"
)

func TestRunSyntheticJSON(t *testing.T) {
	var out bytes.Buffer
	err := run(&out, options{
		SyntheticRoutingRules: 10,
		Runs:                  2,
		Mode:                  "ll",
		Walk:                  true,
		Format:                "json",
	})
	if err != nil {
		t.Fatal(err)
	}

	var got report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Source != "synthetic:routing_rules=10" {
		t.Fatalf("source = %q", got.Source)
	}
	if len(got.Runs) != 2 {
		t.Fatalf("runs = %d", len(got.Runs))
	}
	for _, result := range got.Runs {
		if result.TokenCount == 0 {
			t.Fatal("token count is zero")
		}
		if result.SectionCount != 1 {
			t.Fatalf("section count = %d", result.SectionCount)
		}
		if result.ParserStartNS <= 0 {
			t.Fatalf("parser start duration = %d", result.ParserStartNS)
		}
	}
	if got.Summary["parser_start"].MaxNS <= 0 {
		t.Fatal("parser_start summary is empty")
	}
}

func TestRunSeparatesTokenizeFromParserStart(t *testing.T) {
	input := syntheticInputForTest(20)
	result, err := parseOnce(input, antlr.PredictionModeLL, true, false, "start")
	if err != nil {
		t.Fatal(err)
	}
	if result.TokenizeNS <= 0 || result.ParserStartNS <= 0 || result.TreeWalkNS <= 0 {
		t.Fatalf("unexpected phase durations: %+v", result)
	}
}

func TestRunRejectsInvalidOptions(t *testing.T) {
	tests := []options{
		{},
		{ConfigPath: "a.dae", SyntheticRoutingRules: 1, Runs: 1, Mode: "ll", Format: "text"},
		{SyntheticRoutingRules: 1, Runs: 0, Mode: "ll", Format: "text"},
		{SyntheticRoutingRules: 1, Runs: 1, Mode: "invalid", Format: "text"},
		{SyntheticRoutingRules: 1, Runs: 1, Mode: "ll", Format: "yaml"},
	}
	for _, opts := range tests {
		if err := run(&bytes.Buffer{}, opts); err == nil {
			t.Fatalf("run(%+v) succeeded", opts)
		}
	}
}

func TestTextOutputNamesParserStart(t *testing.T) {
	var out bytes.Buffer
	err := run(&out, options{
		SyntheticRoutingRules: 2,
		Runs:                  1,
		Mode:                  "sll",
		Format:                "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "parser_start_ms") {
		t.Fatalf("output does not contain parser_start_ms:\n%s", out.String())
	}
}

func syntheticInputForTest(rules int) string {
	var b strings.Builder
	b.WriteString("routing {\n")
	for i := 0; i < rules; i++ {
		b.WriteString("domain(suffix: example.test) -> proxy\n")
	}
	b.WriteString("}\n")
	return b.String()
}
