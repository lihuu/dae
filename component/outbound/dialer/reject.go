// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>

package dialer

import (
	"context"
	"fmt"
	"net"

	D "github.com/daeuniverse/outbound/dialer"
	"github.com/daeuniverse/outbound/netproxy"
)

// rejectDialer is the userspace fallback for the main-routing reject action.
// It always fails fast with net.ErrClosed. The primary enforcement for LAN
// ingress TCP is the eBPF reset path; this dialer only handles traffic that
// reaches the control plane and keeps logs readable as a routing decision
// rather than a proxy failure.
type rejectDialer struct {
	dialCallback func()
}

// DialContext implements netproxy.Dialer.
func (d *rejectDialer) DialContext(ctx context.Context, network, addr string) (netproxy.Conn, error) {
	if d.dialCallback != nil {
		d.dialCallback()
	}
	return nil, fmt.Errorf("reject: dial %s %s: %w", network, addr, net.ErrClosed)
}

// NewRejectDialer mirrors NewBlockDialer so the control plane can register a
// built-in "reject" group with the same shape as "block".
func NewRejectDialer(option *GlobalOption, dialCallback func()) (netproxy.Dialer, *Property) {
	_ = option // reserved for interface parity with NewBlockDialer
	return &rejectDialer{dialCallback: dialCallback}, &Property{
		Property: D.Property{
			Name: "reject",
		},
		SubscriptionTag: "",
	}
}

// Compile-time guarantee that rejectDialer satisfies netproxy.Dialer.
var _ netproxy.Dialer = (*rejectDialer)(nil)
