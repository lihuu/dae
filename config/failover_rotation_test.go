/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package config

import (
	"testing"

	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/stretchr/testify/require"
)

// decodeRotationGroup parses a failover group with the given trailing setting
// lines and returns the decoded Group. It is the shared decoder harness for
// primary_rotation_attempts coverage.
func decodeRotationGroup(t *testing.T, setting string) Group {
	t.Helper()
	raw := `
global {}
routing { fallback: direct }
group {
  proxy_failover {
    filter: name(A) [priority: 0]
    filter: name(xray_local) [priority: 1]
    policy: failover
` + setting + `
  }
}`
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	conf, err := New(sections)
	require.NoError(t, err)
	require.Len(t, conf.Group, 1)
	return conf.Group[0]
}

func TestGroupPrimaryRotationAttemptsDefaultsToZero(t *testing.T) {
	g := decodeRotationGroup(t, "")
	require.Equal(t, 0, g.PrimaryRotationAttempts)
}

func TestGroupPrimaryRotationAttemptsDecodesExplicitValue(t *testing.T) {
	g := decodeRotationGroup(t, "primary_rotation_attempts: 5")
	require.Equal(t, 5, g.PrimaryRotationAttempts)
}
