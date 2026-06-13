package dialer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/daeuniverse/outbound/netproxy"
	"github.com/sirupsen/logrus"
)

type diagnosticTestDialer struct {
	conn netproxy.Conn
	err  error
}

func (d *diagnosticTestDialer) DialContext(_ context.Context, _, _ string) (netproxy.Conn, error) {
	return d.conn, d.err
}

func TestProxyTransportDiagnosticDialerLogsActualAddressAndTrafficTarget(t *testing.T) {
	var output bytes.Buffer
	log := logrus.New()
	log.SetOutput(&output)
	log.SetFormatter(&logrus.JSONFormatter{})

	parent := &diagnosticTestDialer{err: errors.New("dial timeout")}
	d := newProxyTransportDiagnosticDialer(
		parent,
		log,
		"primary-node",
		"proxy.example:443",
	)
	ctx := WithProxyTransportDiagnosticContext(
		context.Background(),
		"www.google.com:443",
		"www.google.com",
		"proxy_failover",
		"failover",
	)

	network := netproxy.MagicNetwork{Network: "tcp", IPVersion: "4"}.Encode()
	_, err := d.DialContext(ctx, network, "203.0.113.10:443")
	if err == nil {
		t.Fatal("DialContext() error = nil, want dial timeout")
	}

	got := output.String()
	for _, want := range []string{
		`"event":"proxy_transport_dial"`,
		`"node":"primary-node"`,
		`"proxy_addr":"proxy.example:443"`,
		`"actual_addr":"203.0.113.10:443"`,
		`"traffic_target":"www.google.com:443"`,
		`"sniffed_domain":"www.google.com"`,
		`"outbound":"proxy_failover"`,
		`"policy":"failover"`,
		`"network":"tcp4"`,
		`"result":"failure"`,
		`"error":"dial timeout"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostic log %q does not contain %q", got, want)
		}
	}
}
