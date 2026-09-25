package outbound

import (
	"fmt"
	"time"
)

type FailoverProbeBackoff uint8

const (
	FailoverProbeBackoffExponential FailoverProbeBackoff = iota
	FailoverProbeBackoffFixed
)

func ParseFailoverProbeBackoff(raw string) (FailoverProbeBackoff, error) {
	switch raw {
	case "exponential":
		return FailoverProbeBackoffExponential, nil
	case "fixed":
		return FailoverProbeBackoffFixed, nil
	default:
		return 0, fmt.Errorf(
			"invalid recovery_probe_backoff %q: accepted values are exponential, fixed",
			raw,
		)
	}
}

func nextRecoveryProbeDelay(
	current time.Duration,
	cfg FailoverRecoveryConfig,
	candidateAdvanced bool,
) time.Duration {
	if candidateAdvanced || cfg.Backoff == FailoverProbeBackoffFixed {
		return cfg.ProbeInitial
	}
	if current >= cfg.ProbeMax || current > cfg.ProbeMax/2 {
		return cfg.ProbeMax
	}
	return current * 2
}
