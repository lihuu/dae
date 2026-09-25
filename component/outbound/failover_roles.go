package outbound

import (
	"fmt"

	"github.com/daeuniverse/dae/component/outbound/dialer"
	"github.com/daeuniverse/dae/pkg/config_parser"
)

// exactFailoverRoleNames extracts ordered dialer names from a failover role function.
func exactFailoverRoleNames(role string, fn *config_parser.Function, requireOne bool) ([]string, error) {
	if fn == nil {
		return nil, fmt.Errorf("missing %s failover role", role)
	}
	if fn.Name != "name" {
		return nil, fmt.Errorf("failover %s must use exact name() function, got %q", role, fn.Name)
	}
	if fn.Not {
		return nil, fmt.Errorf("failover %s cannot use a negated function", role)
	}
	if len(fn.Params) == 0 {
		return nil, fmt.Errorf("failover %s name() requires at least one parameter", role)
	}
	if requireOne && len(fn.Params) != 1 {
		return nil, fmt.Errorf("failover %s must be exactly one name", role)
	}

	names := make([]string, 0, len(fn.Params))
	for _, p := range fn.Params {
		if p.Key != "" {
			return nil, fmt.Errorf("failover %s name() parameter must not use keys like %q", role, p.Key)
		}
		if p.Val == "" {
			return nil, fmt.Errorf("failover %s name() parameter must not be empty", role)
		}
		names = append(names, p.Val)
	}
	return names, nil
}

func (s *DialerSet) resolveExactDialer(role, name string) (*dialer.Dialer, error) {
	var found *dialer.Dialer
	var matches int
	for _, d := range s.dialers {
		if d.Property() != nil && d.Property().Name == name {
			found = d
			matches++
		}
	}
	if matches == 0 {
		return nil, fmt.Errorf("failover %s dialer %q is missing", role, name)
	}
	if matches > 1 {
		return nil, fmt.Errorf("failover %s dialer %q matches %d dialers (must be unique)", role, name, matches)
	}
	return found, nil
}

func validateFailoverRecoveryConfig(recovery FailoverRecoveryConfig) error {
	if recovery.RotationAttempts < 0 {
		return fmt.Errorf("primary_rotation_attempts must not be negative: got %d", recovery.RotationAttempts)
	}
	if recovery.ProbeInitial <= 0 {
		return fmt.Errorf("recovery_probe_initial must be positive")
	}
	switch recovery.Backoff {
	case FailoverProbeBackoffFixed:
		// ProbeMax is syntactically decoded by config but semantically unused.
	case FailoverProbeBackoffExponential:
		if recovery.ProbeMax <= 0 {
			return fmt.Errorf("recovery_probe_max must be positive")
		}
		if recovery.ProbeInitial > recovery.ProbeMax {
			return fmt.Errorf(
				"recovery_probe_initial (%v) must not exceed recovery_probe_max (%v)",
				recovery.ProbeInitial,
				recovery.ProbeMax,
			)
		}
	default:
		return fmt.Errorf("invalid failover recovery backoff enum: %d", recovery.Backoff)
	}
	if recovery.Successes < 1 {
		return fmt.Errorf("recovery_successes must be at least 1")
	}
	if recovery.StableTime <= 0 {
		return fmt.Errorf("recovery_stable_time must be positive")
	}
	return nil
}

func (s *DialerSet) ResolveFailoverRoles(
	primary *config_parser.Function,
	fallback *config_parser.Function,
	recovery FailoverRecoveryConfig,
) (
	dialers []*dialer.Dialer,
	annotations []*dialer.Annotation,
	cfg *FailoverConfig,
	err error,
) {
	if err := validateFailoverRecoveryConfig(recovery); err != nil {
		return nil, nil, nil, err
	}

	primaryNames, err := exactFailoverRoleNames("primary", primary, false)
	if err != nil {
		return nil, nil, nil, err
	}
	fallbackNames, err := exactFailoverRoleNames("fallback", fallback, true)
	if err != nil {
		return nil, nil, nil, err
	}

	ordered := make([]*dialer.Dialer, 0, len(primaryNames)+1)
	seen := make(map[*dialer.Dialer]string, len(primaryNames)+1)
	for _, name := range primaryNames {
		d, err := s.resolveExactDialer("primary", name)
		if err != nil {
			return nil, nil, nil, err
		}
		if previous, ok := seen[d]; ok {
			return nil, nil, nil, fmt.Errorf("primary name %q resolves to the same dialer as %q", name, previous)
		}
		seen[d] = name
		ordered = append(ordered, d)
	}

	fallbackDialer, err := s.resolveExactDialer("fallback", fallbackNames[0])
	if err != nil {
		return nil, nil, nil, err
	}
	if primaryName, ok := seen[fallbackDialer]; ok {
		return nil, nil, nil, fmt.Errorf("fallback name %q overlaps primary %q", fallbackNames[0], primaryName)
	}
	ordered = append(ordered, fallbackDialer)

	primaryIdxs := make([]int, len(primaryNames))
	for i := range primaryIdxs {
		primaryIdxs[i] = i
	}
	annotations = make([]*dialer.Annotation, len(ordered))
	for i := range annotations {
		annotations[i] = &dialer.Annotation{}
	}
	cfg = &FailoverConfig{
		PrimaryCandidateIdxs: primaryIdxs,
		FallbackIdx:          len(primaryNames),
		Recovery:             recovery,
	}

	return ordered, annotations, cfg, nil
}
