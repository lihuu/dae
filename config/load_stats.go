/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config

import "time"

// LoadStats reports per-stage durations and size counters accumulated during
// configuration loading. Stage durations are the sum of all invocations within
// one MergeWithStats() / NewWithStats() / ReadConfigWithStats() call; the
// caller is responsible for combining a Merge and a New result when needed.
//
// The struct is intentionally instrumentation-neutral: it has no logrus,
// SummaryCollector, or rulesload dependency, so the config package can keep
// its package-level dependency surface unchanged.
//
// Spec: docs/superpowers/specs/2026-06-25-dae-config-load-breakdown-observability-design.md
//
//   - ReadFilesDuration:     open, stat, permission-check, read of every .dae file.
//   - ParseDuration:         config_parser.Parse over every read file.
//   - IncludeExpandDuration: include glob expansion, stat/filter, building the
//     child-entry list passed to the merge recursion.
//   - MergeDuration:         the recursive include traversal AFTER expansion,
//     section-map conversion, and the final merged-section
//     construction. Per-file read + parse durations live
//     in their own buckets above; this stage is the
//     merge work that wraps them.
//   - DecodeDuration:        config.New validation, decode, unknown-section check.
//   - PatchDuration:         config patches pipeline.
//
// Count fields:
//   - IncludedFiles:   unique .dae files successfully admitted into the merge,
//     including the root config file.
//   - ConfigBytes:     total bytes read from those files before parsing.
//   - ParsedSections:  parser sections produced across files before section-name
//     deduplication and include merging.
//   - RawRoutingRules: routing rules in the final merged `routing` section before
//     optimizer processing and routing_outbound(...) expansion.
//
// FailedStage names exactly the child stage that produced an error; the
// original returned error is preserved by the caller.
type LoadStats struct {
	ReadFilesDuration     time.Duration
	ParseDuration         time.Duration
	IncludeExpandDuration time.Duration
	MergeDuration         time.Duration
	DecodeDuration        time.Duration
	PatchDuration         time.Duration

	IncludedFiles   int
	ConfigBytes     int64
	ParsedSections  int
	RawRoutingRules int

	FailedStage string
}

// Child-stage tokens reported via LoadStats.FailedStage. Operators see these
// verbatim in the `stage=` field of failed rules_load_stage events.
const (
	StageConfigReadFiles     = "config_read_files"
	StageConfigParse         = "config_parse"
	StageConfigIncludeExpand = "config_include_expand"
	StageConfigMerge         = "config_merge"
	StageConfigDecode        = "config_decode"
	StageConfigPatch         = "config_patch"
)

// Add accumulates the durations and counters of other into receiver. The
// caller uses this to combine a Merger.MergeWithStats() result with a
// NewWithStats() result without losing per-stage attribution. FailedStage is
// preserved from whichever side reports a non-empty value first.
func (s *LoadStats) Add(other LoadStats) {
	s.ReadFilesDuration += other.ReadFilesDuration
	s.ParseDuration += other.ParseDuration
	s.IncludeExpandDuration += other.IncludeExpandDuration
	s.MergeDuration += other.MergeDuration
	s.DecodeDuration += other.DecodeDuration
	s.PatchDuration += other.PatchDuration
	s.IncludedFiles += other.IncludedFiles
	s.ConfigBytes += other.ConfigBytes
	s.ParsedSections += other.ParsedSections
	// Latest non-zero value wins for raw routing rules; New is what reports
	// it, so Add() preserves the New side.
	if other.RawRoutingRules != 0 {
		s.RawRoutingRules = other.RawRoutingRules
	}
	if s.FailedStage == "" {
		s.FailedStage = other.FailedStage
	}
}
