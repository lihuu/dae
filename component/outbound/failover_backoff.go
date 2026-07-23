package outbound

import "fmt"

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
