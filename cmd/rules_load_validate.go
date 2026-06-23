/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package cmd

import (
	"strings"
)

// rulesLoadTimingsEnabled returns true when the validate command should
// surface a `rules_load_summary` event (lifecycle=validate). Spec
// docs/superpowers/specs/2026-06-18-dae-rules-load-observability-design.md
// requires an explicit operator opt-in so the default `dae validate -c`
// stdout/stderr contract stays compatible with the production workflow.
//
// The control sources are:
//   - --timings cobra flag (preferred for interactive use)
//   - DAE_RULES_LOAD_TIMINGS env var (for systemd-style invocation)
//
// Accepted truthy env values follow conventional boolean-ish parsing:
// `1`, `true`, `yes`, `on` (case-insensitive). Empty string is false.
func rulesLoadTimingsEnabled(flag bool, env string) bool {
	if flag {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
