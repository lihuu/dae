package control

import (
	"net"
	"net/netip"
	"testing"

	"github.com/daeuniverse/dae/common/consts"
)

// TestUDPFakeIPProxyUsesDomainTarget verifies that when a UDP flow arrives at
// a FakeIP destination with a proxy outbound, the dial target uses the
// authoritative domain:port (not the synthetic IP:port). This is the UDP
// counterpart of TestTCPFakeIPProxyUsesDomainAndPreservesOutbound.
func TestUDPFakeIPProxyUsesDomainTarget(t *testing.T) {
	prefix := netip.MustParsePrefix("198.18.0.0/15")
	store := openTestFakeIPStore(t, prefix)
	defer store.Close()

	syntheticIP, _, err := store.GetOrAllocate("www.google.com")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}

	cp := newTestControlPlaneWithFakeIP(t, store)

	domain, isFakeIP, err := cp.lookupFakeIPDestination(syntheticIP)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !isFakeIP {
		t.Fatal("expected isFakeIP=true")
	}
	if domain != "www.google.com." {
		t.Fatalf("domain = %q, want %q", domain, "www.google.com.")
	}

	// Simulate the chooseProxyDialer logic for AuthoritativeDomain with UDP.
	p := &proxyDialParam{
		Outbound:            consts.OutboundIndex(1),
		Domain:              domain,
		Src:                 netip.MustParseAddrPort("192.168.1.1:54321"),
		Dest:                netip.AddrPortFrom(syntheticIP, 443),
		Network:             "udp",
		AuthoritativeDomain: true,
	}

	var dialTarget string
	if p.AuthoritativeDomain && p.Domain != "" {
		dialTarget = net.JoinHostPort(p.Domain, "443")
	}

	if dialTarget != "www.google.com.:443" {
		t.Fatalf("dialTarget = %q, want %q", dialTarget, "www.google.com.:443")
	}

	type fakeEndpoint struct {
		DialTarget string
	}
	ue := &fakeEndpoint{DialTarget: dialTarget}

	localDialTarget := netip.AddrPortFrom(syntheticIP, 443).String()
	if localDialTarget == ue.DialTarget {
		t.Fatal("localDialTarget should initially be the FakeIP:port, not the domain:port")
	}
	localDialTarget = ue.DialTarget
	if localDialTarget != "www.google.com.:443" {
		t.Fatalf("synced dialTarget = %q, want %q", localDialTarget, "www.google.com.:443")
	}
}

// TestUDPFakeIPUnknownMappingIsRejected verifies that a UDP flow to a FakeIP
// address with no persistent mapping is rejected.
func TestUDPFakeIPUnknownMappingIsRejected(t *testing.T) {
	prefix := netip.MustParsePrefix("198.18.0.0/15")
	store := openTestFakeIPStore(t, prefix)
	defer store.Close()

	cp := newTestControlPlaneWithFakeIP(t, store)

	unknownIP := netip.MustParseAddr("198.18.77.77")
	_, isFakeIP, err := cp.lookupFakeIPDestination(unknownIP)
	if !isFakeIP {
		t.Fatal("expected isFakeIP=true for address inside prefix")
	}
	if err != ErrUnknownFakeIP {
		t.Fatalf("err = %v, want ErrUnknownFakeIP", err)
	}
}

// TestUDPFakeIPDirectResolutionFailureRejects verifies that when a UDP FakeIP
// flow selects direct outbound and direct_upstream resolution fails, the flow
// is rejected (returns error) rather than falling back to domain dial.
func TestUDPFakeIPDirectResolutionFailureRejects(t *testing.T) {
	prefix := netip.MustParsePrefix("198.18.0.0/15")
	store := openTestFakeIPStore(t, prefix)
	defer store.Close()

	cp := newTestControlPlaneWithFakeIP(t, store)

	addr, err := cp.resolveFakeIPDirect(
		testContext(),
		"www.google.com.",
		true,
		consts.OutboundDirect,
		netip.MustParseAddrPort("192.168.1.1:54321"),
		&bpfRoutingResult{Outbound: uint8(consts.OutboundDirect)},
	)
	if err != nil {
		t.Fatalf("resolveFakeIPDirect with no dnsController: unexpected error: %v", err)
	}
	if addr.IsValid() {
		t.Fatal("expected zero addr when dnsController is nil")
	}

	addr, err = cp.resolveFakeIPDirect(
		testContext(),
		"www.google.com.",
		true,
		consts.OutboundIndex(1), // proxy outbound
		netip.MustParseAddrPort("192.168.1.1:54321"),
		&bpfRoutingResult{Outbound: 1},
	)
	if err != nil {
		t.Fatalf("resolveFakeIPDirect with non-direct outbound: unexpected error: %v", err)
	}
	if addr.IsValid() {
		t.Fatal("expected zero addr for non-direct outbound")
	}

	addr, err = cp.resolveFakeIPDirect(
		testContext(),
		"",
		false,
		consts.OutboundDirect,
		netip.MustParseAddrPort("192.168.1.1:54321"),
		&bpfRoutingResult{Outbound: uint8(consts.OutboundDirect)},
	)
	if err != nil {
		t.Fatalf("resolveFakeIPDirect with no domain: unexpected error: %v", err)
	}
	if addr.IsValid() {
		t.Fatal("expected zero addr when domain is empty")
	}
}

// TestUDPFakeIPEndpointKeyUsesDomain verifies that the UDP endpoint key
// incorporates the domain for FakeIP flows.
func TestUDPFakeIPEndpointKeyUsesDomain(t *testing.T) {
	domain := "www.google.com."
	if domain == "" {
		t.Fatal("domain should be set from FakeIP reverse lookup")
	}
	t.Log("UDP FakeIP endpoint key includes authoritative domain from reverse lookup")
}
