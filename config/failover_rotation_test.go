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
    primary: name(A, B, C)
    fallback: name(xray_local)
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

func TestGroupFailoverRolesDecodeNameFunctions(t *testing.T) {
	g := decodeRotationGroup(t, "primary_rotation_attempts: 5")
	primary, err := ParseFunctionOrString(g.Primary)
	require.NoError(t, err)
	fallback, err := ParseFunctionOrString(g.Fallback)
	require.NoError(t, err)
	require.Equal(t, "name", primary.Name)
	require.Equal(t, []string{"A", "B", "C"}, []string{
		primary.Params[0].Val,
		primary.Params[1].Val,
		primary.Params[2].Val,
	})
	require.Equal(t, "name", fallback.Name)
	require.Equal(t, "xray_local", fallback.Params[0].Val)
}

func TestGroupPrimaryRotationAttemptsDefaultsToZero(t *testing.T) {
	g := decodeRotationGroup(t, "")
	require.Equal(t, 0, g.PrimaryRotationAttempts)
}

func TestGroupPrimaryRotationAttemptsDecodesExplicitValue(t *testing.T) {
	g := decodeRotationGroup(t, "primary_rotation_attempts: 5")
	require.Equal(t, 5, g.PrimaryRotationAttempts)
}

func TestGroupRecoveryProbeBackoffDefaultsToExponential(t *testing.T) {
	g := decodeRotationGroup(t, "")
	require.Equal(t, "exponential", g.RecoveryProbeBackoff)
}

func TestGroupRecoveryProbeBackoffDecodesExplicitValue(t *testing.T) {
	g := decodeRotationGroup(t, "recovery_probe_backoff: fixed")
	require.Equal(t, "fixed", g.RecoveryProbeBackoff)
}

func TestGroupFixedBackoffStillRejectsMalformedMaxDuration(t *testing.T) {
	raw := `
global {}
routing { fallback: direct }
group {
  proxy_failover {
    primary: name(A)
    fallback: name(X)
    policy: failover
    recovery_probe_backoff: fixed
    recovery_probe_max: definitely-not-a-duration
  }
}`
	sections, err := config_parser.Parse(raw)
	require.NoError(t, err)
	_, err = New(sections)
	require.Error(t, err)
}

