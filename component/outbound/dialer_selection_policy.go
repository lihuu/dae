/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"fmt"
	"strconv"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/config"
)

type DialerSelectionPolicy struct {
	Policy     consts.DialerSelectionPolicy
	FixedIndex int
}

func NewDialerSelectionPolicyFromGroupParam(param *config.Group) (policy *DialerSelectionPolicy, err error) {
	fs, err := config.ParseFunctionListOrString(param.Policy)
	if err != nil {
		return nil, err
	}
	if len(fs) > 1 || len(fs) == 0 {
		return nil, fmt.Errorf("policy should be exact 1 function: got %v", len(fs))
	}
	f := fs[0]
	// primary_rotation_attempts is an opt-in failover-only setting. Reject
	// negative values and any nonzero value on a non-failover policy at
	// policy construction time, so startup/reload fails the same way
	// `dae validate` does even if the operator skipped the validate step.
	if param.PrimaryRotationAttempts < 0 {
		return nil, fmt.Errorf("primary_rotation_attempts must not be negative: got %d", param.PrimaryRotationAttempts)
	}
	if param.PrimaryRotationAttempts != 0 && f.Name != string(consts.DialerSelectionPolicy_Failover) {
		return nil, fmt.Errorf("primary_rotation_attempts requires policy: failover, got %q", f.Name)
	}
	switch fName := consts.DialerSelectionPolicy(f.Name); fName {
	case consts.DialerSelectionPolicy_Random,
		consts.DialerSelectionPolicy_MinAverage10Latencies,
		consts.DialerSelectionPolicy_MinLastLatency,
		consts.DialerSelectionPolicy_MinMovingAverageLatencies,
		consts.DialerSelectionPolicy_Failover:
		return &DialerSelectionPolicy{
			Policy: fName,
		}, nil
	case consts.DialerSelectionPolicy_Fixed:

		if f.Not {
			return nil, fmt.Errorf("policy param does not support not operator: !%v()", f.Name)
		}
		if len(f.Params) != 1 || f.Params[0].Key != "" {
			return nil, fmt.Errorf(`invalid "%v" param format`, f.Name)
		}
		strIndex := f.Params[0].Val
		index, err := strconv.Atoi(strIndex)
		if err != nil {
			return nil, fmt.Errorf(`invalid "%v" param format: %w`, f.Name, err)
		}
		return &DialerSelectionPolicy{
			Policy:     consts.DialerSelectionPolicy(f.Name),
			FixedIndex: index,
		}, nil

	default:
		return nil, fmt.Errorf("unexpected policy: %v", f.Name)
	}
}
