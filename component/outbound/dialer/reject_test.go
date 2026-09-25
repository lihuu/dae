// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>

package dialer

import (
	"context"
	"errors"
	"net"
	"testing"
)

func TestRejectDialerReturnsErrClosed(t *testing.T) {
	d, p := NewRejectDialer(nil, nil)
	if p == nil {
		t.Fatal("NewRejectDialer returned nil Property")
	}
	if p.Name != "reject" {
		t.Fatalf("Property.Name = %q, want %q", p.Name, "reject")
	}
	_, err := d.DialContext(context.Background(), "tcp", "198.51.100.1:443")
	if err == nil {
		t.Fatal("DialContext returned nil error; reject must always fail")
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("DialContext err = %v, want errors.Is net.ErrClosed", err)
	}
}
