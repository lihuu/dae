//go:build linux && dae_bpf_tests

package control

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/daeuniverse/dae/common/consts"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func configureDefaultGateway(t *testing.T, nsHandle netns.NsHandle, gwIP net.IP) {
	t.Helper()
	withNetns(t, nsHandle, func() error {
		route := &netlink.Route{
			Scope:     netlink.SCOPE_UNIVERSE,
			Gw:        gwIP,
		}
		if err := netlink.RouteAdd(route); err != nil {
			return fmt.Errorf("add default gateway %v: %w", gwIP, err)
		}
		return nil
	})
}

func enableIPForwarding(t *testing.T, nsHandle netns.NsHandle) {
	t.Helper()
	withNetns(t, nsHandle, func() error {
		err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644)
		if err != nil {
			return fmt.Errorf("enable ip_forward: %w", err)
		}
		return nil
	})
}

func loadFakeIPTestBpfObjects(t *testing.T, dae0Ifindex uint32, fakeIPV4Network, fakeIPV4Mask uint32) *bpfObjects {
	t.Helper()

	var obj bpfObjects
	opts := &ebpf.CollectionOptions{
		Programs: ebpf.ProgramOptions{
			LogLevel:     ebpf.LogLevelInstruction,
			LogSizeStart: 1 << 20,
		},
	}
	constants := map[string]interface{}{
		"PARAM": bpfDaeParam{
			Dae0Ifindex:     dae0Ifindex,
			DaeNetnsId:      currentTCNetnsLookupID,
			FakeipV4Network: fakeIPV4Network,
			FakeipV4Mask:    fakeIPV4Mask,
			FakeipEnabled:   1,
		},
	}

	if err := loadBpfObjectsWithConstantsAndCustomizer(&obj, opts, constants, disableAllPinnedMapsForTests); err != nil {
		t.Fatalf("load main bpf objects for fakeip integration test: %v", err)
	}
	return &obj
}

func TestFakeipIntegration_NoLeakageOnWan(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root")
	}

	// 1. Setup namespaces
	routerNs := newTransientNetns(t)
	defer func() { _ = routerNs.Close() }()

	clientNs := newTransientNetns(t)
	defer func() { _ = clientNs.Close() }()

	wanNs := newTransientNetns(t)
	defer func() { _ = wanNs.Close() }()

	// 2. Create veth pairs
	// client <-> router (LAN)
	lanRouterIfName := "v_lan_router"
	lanClientIfName := "v_lan_client"
	createVethPairInNamespaces(t, lanRouterIfName, lanClientIfName, routerNs, clientNs)

	// router <-> wan (WAN)
	wanRouterIfName := "v_wan_router"
	wanClientIfName := "v_wan_client"
	createVethPairInNamespaces(t, wanRouterIfName, wanClientIfName, routerNs, wanNs)

	// 3. Configure IP addresses
	configureIPv4Interface(t, clientNs, lanClientIfName, "192.168.2.2/24")
	configureIPv4Interface(t, routerNs, lanRouterIfName, "192.168.2.1/24")
	configureIPv4Interface(t, routerNs, wanRouterIfName, "10.0.0.1/24")
	configureIPv4Interface(t, wanNs, wanClientIfName, "10.0.0.2/24")
	configureIPv4Interface(t, wanNs, wanClientIfName, "198.18.0.29/32")

	// 4. Configure routing and forwarding
	enableIPForwarding(t, routerNs)
	configureDefaultGateway(t, clientNs, net.ParseIP("192.168.2.1"))
	configureDefaultGateway(t, routerNs, net.ParseIP("10.0.0.2"))

	// 5. Load and attach BPF on router LAN interface
	lanRouterIfindex := interfaceIndex(t, routerNs, lanRouterIfName)

	addrBytes := [4]byte{198, 18, 0, 0}
	fakeIPV4Network := binary.LittleEndian.Uint32(addrBytes[:])
	maskBytes := [4]byte{255, 254, 0, 0}
	fakeIPV4Mask := binary.LittleEndian.Uint32(maskBytes[:])

	bpfObj := loadFakeIPTestBpfObjects(t, uint32(lanRouterIfindex), fakeIPV4Network, fakeIPV4Mask)
	defer func() { _ = bpfObj.Close() }()

	attachLanIngressFilter(t, routerNs, lanRouterIfName, bpfObj.TproxyLanIngressL2)

	// Set fallback routing rule to OUTBOUND_DIRECT.
	// This ensures that normal traffic is forwarded directly (no proxy interception),
	// whereas FakeIP traffic must be intercepted by eBPF.
	activeRulesLen := uint32(1)
	if err := bpfObj.RoutingMetaMap.Update(uint32(0), activeRulesLen, ebpf.UpdateAny); err != nil {
		t.Fatalf("initialize routing_meta_map: %v", err)
	}
	matchSet := bpfMatchSet{
		Type:     uint8(consts.MatchType_Fallback),
		Outbound: uint8(consts.OutboundDirect),
	}
	if err := bpfObj.RoutingMap.Update(uint32(0), matchSet, ebpf.UpdateAny); err != nil {
		t.Fatalf("initialize fallback routing rule: %v", err)
	}

	// 6. Test direct traffic (non-FakeIP) to ensure environment forwards packets
	t.Run("DirectTrafficForwardsNormally", func(t *testing.T) {
		wanConn := listenUDPInNetns(t, wanNs, &net.UDPAddr{
			IP:   net.ParseIP("10.0.0.2"),
			Port: 12345,
		})
		defer func() { _ = wanConn.Close() }()

		recvCh := make(chan error, 1)
		go func() {
			buf := make([]byte, 256)
			_ = wanConn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _, err := wanConn.ReadFromUDP(buf)
			recvCh <- err
		}()

		payload := []byte("hello-direct-wan")
		sendUDPFromNamespace(t, clientNs, &net.UDPAddr{
			IP:   net.ParseIP("192.168.2.2"),
			Port: 0,
		}, &net.UDPAddr{
			IP:   net.ParseIP("10.0.0.2"),
			Port: 12345,
		}, payload)

		if err := <-recvCh; err != nil {
			t.Fatalf("expected direct packet to be forwarded to WAN: %v", err)
		}
	})

	// 7. Test FakeIP traffic to ensure it is intercepted and DOES NOT leak to WAN
	t.Run("FakeIPTrafficDoesNotLeak", func(t *testing.T) {
		wanConn := listenUDPInNetns(t, wanNs, &net.UDPAddr{
			IP:   net.ParseIP("198.18.0.29"),
			Port: 443,
		})
		defer func() { _ = wanConn.Close() }()

		recvCh := make(chan error, 1)
		go func() {
			buf := make([]byte, 256)
			_ = wanConn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, _, err := wanConn.ReadFromUDP(buf)
			recvCh <- err
		}()

		payload := []byte("hello-fakeip-block")
		sendUDPFromNamespace(t, clientNs, &net.UDPAddr{
			IP:   net.ParseIP("192.168.2.2"),
			Port: 0,
		}, &net.UDPAddr{
			IP:   net.ParseIP("198.18.0.29"),
			Port: 443,
		}, payload)

		err := <-recvCh
		if err == nil {
			t.Fatal("SECURITY FAILURE: FakeIP traffic leaked onto the WAN namespace")
		}
		if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			t.Fatalf("unexpected read error: %v", err)
		}
	})
}
