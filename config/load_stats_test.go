/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalDaeConfig is a syntactically valid .dae snippet that exercises every
// required top-level section. Tests that need a richer config inject extra
// sections by string substitution.
const minimalDaeConfig = `global {}
routing {
  fallback: direct
}
dns {
  upstream {
    cn: "udp://223.5.5.5:53"
  }
  routing {
    request {
      fallback: cn
    }
    response {
      fallback: accept
    }
  }
}
`

// writeDae writes content to path with mode 0600 so the Merger's permission
// check accepts it. The file's parent directory is t.TempDir(), so cleanup is
// automatic.
func writeDae(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %v: %v", path, err)
	}
}

// TestMergeWithStats_BasicAggregation checks that loading a single config
// file populates ReadFilesDuration, ParseDuration, IncludedFiles, ConfigBytes,
// ParsedSections. The wrapper MergeDuration is non-negative; IncludeExpand /
// MergeDuration may be zero when there are no includes.
func TestMergeWithStats_BasicAggregation(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "config.dae")
	writeDae(t, entry, minimalDaeConfig)

	sections, includes, stats, err := NewMerger(entry).MergeWithStats()
	require.NoError(t, err)
	require.NotEmpty(t, sections)
	assert.Equal(t, []string{entry}, includes)

	// Spec semantics: included_files counts every admitted .dae file.
	assert.Equal(t, 1, stats.IncludedFiles)
	assert.Equal(t, int64(len(minimalDaeConfig)), stats.ConfigBytes)
	// minimalDaeConfig has global / routing / dns top-level sections.
	assert.Equal(t, 3, stats.ParsedSections)

	// Durations must be non-negative. Read/parse always run; merge wrapper
	// may be 0 on very fast machines, that is allowed.
	assert.GreaterOrEqual(t, int64(stats.ReadFilesDuration), int64(0))
	assert.GreaterOrEqual(t, int64(stats.ParseDuration), int64(0))
	assert.GreaterOrEqual(t, int64(stats.IncludeExpandDuration), int64(0))
	assert.GreaterOrEqual(t, int64(stats.MergeDuration), int64(0))
	assert.Empty(t, stats.FailedStage)
}

// TestMergeWithStats_IncludeAggregatesAcrossFiles verifies that include files
// contribute to ConfigBytes, ParsedSections, and IncludedFiles, and that the
// include expansion stage observes non-negative time.
func TestMergeWithStats_IncludeAggregatesAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "config.dae")
	child := filepath.Join(dir, "child.dae")

	writeDae(t, child, "node {\n  fixed: 'ss://aGVsbG8='\n}\n")

	entryBody := minimalDaeConfig + "include {\n  './child.dae'\n}\n"
	writeDae(t, entry, entryBody)

	sections, includes, stats, err := NewMerger(entry).MergeWithStats()
	require.NoError(t, err)
	require.NotEmpty(t, sections)

	// Spec: included_files counts every admitted file including the root.
	assert.Equal(t, 2, stats.IncludedFiles)
	assert.ElementsMatch(t, []string{entry, child}, includes)

	// ConfigBytes is the sum of bytes read across all admitted files.
	assert.Equal(t, int64(len(entryBody)+len("node {\n  fixed: 'ss://aGVsbG8='\n}\n")), stats.ConfigBytes)

	// Each file contributes its own parser sections before include merging.
	// minimalDaeConfig produces 3 sections; entryBody adds the include
	// section (4 total); child.dae produces 1 section. Spec semantics:
	// parsed_sections counts pre-dedup, pre-include-merge.
	assert.Equal(t, 5, stats.ParsedSections)
	assert.GreaterOrEqual(t, int64(stats.IncludeExpandDuration), int64(0))
}

// TestMergeWithStats_ParseFailureAttribution verifies that a syntactically
// invalid file fails the merger with FailedStage=config_parse and that the
// returned LoadStats still preserves ReadFilesDuration and ConfigBytes for
// the bytes successfully read.
func TestMergeWithStats_ParseFailureAttribution(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "config.dae")
	// Unterminated string literal: valid file IO, broken grammar.
	writeDae(t, entry, "global { log_level: \"info\n")

	_, _, stats, err := NewMerger(entry).MergeWithStats()
	require.Error(t, err)
	assert.Equal(t, StageConfigParse, stats.FailedStage)
	assert.Greater(t, stats.ConfigBytes, int64(0))
	// IncludedFiles is only populated on success; on failure the partial
	// map may not be a useful count, so we don't assert it.
}

// TestMergeWithStats_ReadFailureAttribution verifies that a missing or
// directory entry fails with FailedStage=config_read_files.
func TestMergeWithStats_ReadFailureAttribution(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.dae")

	_, _, stats, err := NewMerger(missing).MergeWithStats()
	require.Error(t, err)
	assert.Equal(t, StageConfigReadFiles, stats.FailedStage)
}

// TestMergeWithStats_CircularIncludeAttribution verifies that the legacy
// ErrCircularInclude path is attributed to config_merge: the merge recursion
// detects it before any new file IO.
func TestMergeWithStats_CircularIncludeAttribution(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.dae")
	b := filepath.Join(dir, "b.dae")
	writeDae(t, a, minimalDaeConfig+"include {\n  './b.dae'\n}\n")
	writeDae(t, b, "include {\n  './a.dae'\n}\n")

	_, _, stats, err := NewMerger(a).MergeWithStats()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "circular include"),
		"expected circular include error, got %q", err.Error())
	assert.Equal(t, StageConfigMerge, stats.FailedStage)
}

// TestMergeWithStats_PermissionFailureAttribution verifies that a file with
// world/group-too-open permissions fails with FailedStage=config_read_files,
// since the merger rejects the file during the stat phase.
//
// The test only runs when the current user can actually create a 0644 file;
// some sandboxed CI runners enforce a stricter umask and this assertion is
// fine to skip there.
func TestMergeWithStats_PermissionFailureAttribution(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "config.dae")
	if err := os.WriteFile(entry, []byte(minimalDaeConfig), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	fi, err := os.Stat(entry)
	require.NoError(t, err)
	if fi.Mode()&0037 == 0 {
		t.Skip("filesystem coerced 0644 to a tighter mode; permission test does not apply")
	}

	_, _, stats, err := NewMerger(entry).MergeWithStats()
	require.Error(t, err)
	assert.Equal(t, StageConfigReadFiles, stats.FailedStage)
}

// TestMerge_LegacyDelegationPreservesBehaviour confirms that calling the
// legacy Merge() entry point still returns the same sections/includes as
// the stats-aware path and does not surface a stats argument.
func TestMerge_LegacyDelegationPreservesBehaviour(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "config.dae")
	writeDae(t, entry, minimalDaeConfig)

	legacySections, legacyIncludes, legacyErr := NewMerger(entry).Merge()
	statsSections, statsIncludes, _, statsErr := NewMerger(entry).MergeWithStats()
	require.NoError(t, legacyErr)
	require.NoError(t, statsErr)
	assert.Equal(t, len(statsSections), len(legacySections))
	assert.Equal(t, statsIncludes, legacyIncludes)
}

// TestNewWithStats_RawRoutingRulesAndPatchAttribution verifies that
// NewWithStats reports the final raw_routing_rules count and that successful
// decode/patch stages have non-negative durations.
func TestNewWithStats_RawRoutingRulesAndPatchAttribution(t *testing.T) {
	raw := `global {}
routing {
  fallback: direct
  ip(8.8.8.8) -> direct
  dport(443) -> direct
}
dns {
  upstream {
    cn: "udp://223.5.5.5:53"
  }
  routing {
    request {
      fallback: cn
    }
    response {
      fallback: accept
    }
  }
}
`
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	conf, stats, err := NewWithStats(sections)
	require.NoError(t, err)
	require.NotNil(t, conf)
	// The fallback rule plus two explicit rules. Raw_routing_rules counts
	// the rules in the final merged section before optimization and before
	// routing_outbound() expansion.
	assert.GreaterOrEqual(t, stats.RawRoutingRules, 2)
	assert.GreaterOrEqual(t, int64(stats.DecodeDuration), int64(0))
	assert.GreaterOrEqual(t, int64(stats.PatchDuration), int64(0))
	// MergeWithStats fields stay zero in NewWithStats.
	assert.Zero(t, stats.ReadFilesDuration)
	assert.Zero(t, stats.ParseDuration)
	assert.Zero(t, stats.IncludedFiles)
	assert.Empty(t, stats.FailedStage)
}

// TestNewWithStats_DecodeFailureAttribution forces a missing required section
// to produce a config_decode failure with FailedStage set accordingly.
func TestNewWithStats_DecodeFailureAttribution(t *testing.T) {
	raw := `global {}
` // routing section is required by config; omit it to force a decode error.
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	_, stats, err := NewWithStats(sections)
	require.Error(t, err)
	assert.Equal(t, StageConfigDecode, stats.FailedStage)
	assert.Zero(t, stats.PatchDuration, "patch must not run when decode fails")
}

// TestNewWithStats_UnknownSectionAttribution verifies that an unknown
// top-level section is reported as a config_decode failure (the spec puts
// unknown-section checks in the decode stage).
func TestNewWithStats_UnknownSectionAttribution(t *testing.T) {
	raw := minimalDaeConfig + "absurd {}\n"
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	_, stats, err := NewWithStats(sections)
	require.Error(t, err)
	assert.Equal(t, StageConfigDecode, stats.FailedStage)
}

// TestLoadStats_AddCombinesMergeAndNew checks that the LoadStats.Add helper
// produces the totals a caller will report to rules_load: durations sum,
// counts sum, RawRoutingRules takes the New-side value, FailedStage carries
// the first non-empty stage.
func TestLoadStats_AddCombinesMergeAndNew(t *testing.T) {
	merge := LoadStats{
		ReadFilesDuration:     5,
		ParseDuration:         10,
		IncludeExpandDuration: 1,
		MergeDuration:         3,
		IncludedFiles:         4,
		ConfigBytes:           1024,
		ParsedSections:        7,
	}
	decode := LoadStats{
		DecodeDuration:  20,
		PatchDuration:   2,
		RawRoutingRules: 36,
	}
	merge.Add(decode)
	assert.Equal(t, int64(5), int64(merge.ReadFilesDuration))
	assert.Equal(t, int64(10), int64(merge.ParseDuration))
	assert.Equal(t, int64(20), int64(merge.DecodeDuration))
	assert.Equal(t, int64(2), int64(merge.PatchDuration))
	assert.Equal(t, 4, merge.IncludedFiles)
	assert.Equal(t, int64(1024), merge.ConfigBytes)
	assert.Equal(t, 7, merge.ParsedSections)
	assert.Equal(t, 36, merge.RawRoutingRules)
	assert.Empty(t, merge.FailedStage)

	withFail := LoadStats{FailedStage: StageConfigPatch, PatchDuration: 9}
	merge.Add(withFail)
	// Add takes other.FailedStage when the receiver had none. Patch
	// duration accumulates on top of the existing one.
	assert.Equal(t, StageConfigPatch, merge.FailedStage)
	assert.Equal(t, int64(11), int64(merge.PatchDuration))

	// Now exercise the "merge already failed" → "merge keeps its stage" rule:
	// once FailedStage is set, Add must not overwrite it with a later one.
	first := LoadStats{FailedStage: StageConfigParse}
	first.Add(LoadStats{FailedStage: StageConfigPatch})
	assert.Equal(t, StageConfigParse, first.FailedStage)
}
