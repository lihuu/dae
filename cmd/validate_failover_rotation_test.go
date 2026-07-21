/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package cmd

import (
	"testing"

	"github.com/daeuniverse/dae/config"
	"github.com/stretchr/testify/require"
)

// TestValidateFailoverSettings is the RED/GREEN anchor for the cmd-side
// primary_rotation_attempts validator. It exercises the same setting-specific
// error fragments that NewDialerSelectionPolicyFromGroupParam produces, so
// `dae validate` reports rotation misuse consistently with the daemon's
// startup/reload path.
func TestValidateFailoverSettings(t *testing.T) {
	tests := []struct {
		name    string
		group   config.Group
		wantErr string
	}{
		{name: "disabled failover", group: config.Group{Name: "g", Policy: "failover"}},
		{name: "enabled failover", group: config.Group{Name: "g", Policy: "failover", PrimaryRotationAttempts: 5}},
		{name: "negative", group: config.Group{Name: "g", Policy: "failover", PrimaryRotationAttempts: -1}, wantErr: "must not be negative"},
		{name: "non failover", group: config.Group{Name: "g", Policy: "random", PrimaryRotationAttempts: 5}, wantErr: "requires policy: failover"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := &config.Config{Group: []config.Group{tt.group}}
			err := validateFailoverSettings(conf)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}