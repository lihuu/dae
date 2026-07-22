package control

import (
	"fmt"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component/outbound"
	"github.com/daeuniverse/dae/component/outbound/dialer"
	"github.com/daeuniverse/dae/config"
)

func resolveConfiguredGroupDialers(
	dialerSet *outbound.DialerSet,
	group config.Group,
	policy outbound.DialerSelectionPolicy,
) ([]*dialer.Dialer, []*dialer.Annotation, *outbound.FailoverConfig, error) {
	if policy.Policy == consts.DialerSelectionPolicy_Failover {
		if len(group.Filter) != 0 {
			return nil, nil, nil, fmt.Errorf("group %q: policy failover does not allow filter; use primary and fallback", group.Name)
		}
		primary, err := config.ParseFunctionOrString(group.Primary)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("group %q primary: %w", group.Name, err)
		}
		fallback, err := config.ParseFunctionOrString(group.Fallback)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("group %q fallback: %w", group.Name, err)
		}
		recovery := outbound.FailoverRecoveryConfig{
			ProbeInitial:     group.RecoveryProbeInitial,
			ProbeMax:         group.RecoveryProbeMax,
			Successes:        group.RecoverySuccesses,
			StableTime:       group.RecoveryStableTime,
			RotationAttempts: group.PrimaryRotationAttempts,
		}
		dialers, annotations, cfg, err := dialerSet.ResolveFailoverRoles(primary, fallback, recovery)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("group %q: %w", group.Name, err)
		}
		return dialers, annotations, cfg, nil
	}

	if group.Primary != nil || group.Fallback != nil {
		return nil, nil, nil, fmt.Errorf("group %q: primary and fallback require policy: failover", group.Name)
	}
	dialers, annotations, err := dialerSet.FilterAndAnnotate(group.Filter, group.FilterAnnotation)
	return dialers, annotations, nil, err
}
