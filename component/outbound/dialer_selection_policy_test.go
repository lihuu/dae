/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"testing"

	"github.com/daeuniverse/dae/config"
	"github.com/stretchr/testify/require"
)

func TestNewDialerSelectionPolicyFromGroupParamRejectsInvalidPolicyType(t *testing.T) {
	_, err := NewDialerSelectionPolicyFromGroupParam(&config.Group{
		Policy: 123,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported function-list-or-string value type")
}

// TestNewDialerSelectionPolicyFromGroupParamRejectsNegativeRotationAttempts
// proves the daemon's startup/reload path independently rejects negative
// primary_rotation_attempts values, even if the operator skipped `dae validate`.
func TestNewDialerSelectionPolicyFromGroupParamRejectsNegativeRotationAttempts(t *testing.T) {
	_, err := NewDialerSelectionPolicyFromGroupParam(&config.Group{
		Policy:                  "failover",
		PrimaryRotationAttempts: -1,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "must not be negative")
}

// TestNewDialerSelectionPolicyFromGroupParamRejectsRotationOnNonFailover
// proves rotation attempts on a non-failover policy are rejected at policy
// construction time, not only by `dae validate`.
func TestNewDialerSelectionPolicyFromGroupParamRejectsRotationOnNonFailover(t *testing.T) {
	_, err := NewDialerSelectionPolicyFromGroupParam(&config.Group{
		Policy:                  "random",
		PrimaryRotationAttempts: 5,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "requires policy: failover")
}

// TestNewDialerSelectionPolicyFromGroupParamAcceptsRotationOnFailover confirms
// a positive rotation value combined with policy: failover parses successfully.
func TestNewDialerSelectionPolicyFromGroupParamAcceptsRotationOnFailover(t *testing.T) {
	policy, err := NewDialerSelectionPolicyFromGroupParam(&config.Group{
		Policy:                  "failover",
		PrimaryRotationAttempts: 5,
	})
	require.NoError(t, err)
	require.NotNil(t, policy)
}
