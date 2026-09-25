//go:build linux && dae_bpf_tests

package control

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/daeuniverse/dae/common/consts"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const currentTCNetnsLookupID = ^uint32(0)

func configureDefaultGateway(t *testing.T, nsHandle netns.NsHandle, gwIP net.IP) {
	t.Helper()
	withNetns(t, nsHandle, func() error {
		route := &netlink.Route{
			Scope: netlink.SCOPE_UNIVERSE,
			Gw:    gwIP,
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

func newTransientNetns(t *testing.T) netns.NsHandle {
	t.Helper()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origin, err := netns.Get()
	if err != nil {
		t.Fatalf("get current netns: %v", err)
	}
	defer func() { _ = origin.Close() }()

	nsHandle, err := netns.New()
	if err != nil {
		t.Fatalf("create transient netns: %v", err)
	}
	if err := netns.Set(origin); err != nil {
		_ = nsHandle.Close()
		t.Fatalf("restore original netns after creation: %v", err)
	}
	return nsHandle
}

func withNetns(t *testing.T, nsHandle netns.NsHandle, fn func() error) {
	t.Helper()

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origin, err := netns.Get()
	if err != nil {
		t.Fatalf("get current netns: %v", err)
	}
	defer func() { _ = origin.Close() }()

	if err := netns.Set(nsHandle); err != nil {
		t.Fatalf("switch netns: %v", err)
	}
	defer func() {
		if err := netns.Set(origin); err != nil {
			t.Fatalf("restore original netns: %v", err)
		}
	}()

	if err := fn(); err != nil {
		t.Fatal(err)
	}
}

func createVethPairInNamespaces(t *testing.T, lanIfName, clientIfName string, lanNs, clientNs netns.NsHandle) {
	t.Helper()

	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name: lanIfName,
		},
		PeerName: clientIfName,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		t.Fatalf("create veth pair %s/%s: %v", lanIfName, clientIfName, err)
	}

	lanLink, err := netlink.LinkByName(lanIfName)
	if err != nil {
		t.Fatalf("lookup lan veth %s: %v", lanIfName, err)
	}
	clientLink, err := netlink.LinkByName(clientIfName)
	if err != nil {
		t.Fatalf("lookup client veth %s: %v", clientIfName, err)
	}

	if err := netlink.LinkSetNsFd(lanLink, int(lanNs)); err != nil {
		t.Fatalf("move %s to lan netns: %v", lanIfName, err)
	}
	if err := netlink.LinkSetNsFd(clientLink, int(clientNs)); err != nil {
		t.Fatalf("move %s to client netns: %v", clientIfName, err)
	}
}

func configureIPv4Interface(t *testing.T, nsHandle netns.NsHandle, ifName, cidr string) {
	t.Helper()

	withNetns(t, nsHandle, func() error {
		loopback, err := netlink.LinkByName("lo")
		if err != nil {
			return fmt.Errorf("lookup loopback: %w", err)
		}
		if err := netlink.LinkSetUp(loopback); err != nil {
			return fmt.Errorf("bring loopback up: %w", err)
		}

		link, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("lookup interface %s: %w", ifName, err)
		}

		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			return fmt.Errorf("parse addr %s: %w", cidr, err)
		}
		if err := netlink.AddrAdd(link, addr); err != nil {
			return fmt.Errorf("assign addr %s to %s: %w", cidr, ifName, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("bring %s up: %w", ifName, err)
		}
		return nil
	})
}

func interfaceIndex(t *testing.T, nsHandle netns.NsHandle, ifName string) int {
	t.Helper()

	var index int
	withNetns(t, nsHandle, func() error {
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("lookup interface %s: %w", ifName, err)
		}
		index = link.Attrs().Index
		return nil
	})
	return index
}

func listenUDPInNetns(t *testing.T, nsHandle netns.NsHandle, addr *net.UDPAddr) *net.UDPConn {
	t.Helper()

	var (
		conn *net.UDPConn
		err  error
	)
	withNetns(t, nsHandle, func() error {
		conn, err = net.ListenUDP("udp4", addr)
		if err != nil {
			return fmt.Errorf("listen UDP on %v: %w", addr, err)
		}
		return nil
	})
	return conn
}

func sendUDPFromNamespace(t *testing.T, nsHandle netns.NsHandle, localAddr, remoteAddr *net.UDPAddr, payload []byte) {
	t.Helper()

	withNetns(t, nsHandle, func() error {
		conn, err := net.DialUDP("udp4", localAddr, remoteAddr)
		if err != nil {
			return fmt.Errorf("dial UDP %v -> %v: %w", localAddr, remoteAddr, err)
		}
		defer func() { _ = conn.Close() }()

		for range 3 {
			if _, err := conn.Write(payload); err != nil {
				return fmt.Errorf("write UDP payload to %v: %w", remoteAddr, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	})
}

func attachLanIngressFilter(t *testing.T, nsHandle netns.NsHandle, ifName string, prog *ebpf.Program) {
	t.Helper()

	withNetns(t, nsHandle, func() error {
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("lookup interface %s for tc attach: %w", ifName, err)
		}

		qdisc := buildClsactQdisc(link)
		if err := netlink.QdiscAdd(qdisc); err != nil && err != unix.EEXIST {
			return fmt.Errorf("add clsact qdisc on %s: %w", ifName, err)
		}

		filter := &netlink.BpfFilter{
			FilterAttrs: netlink.FilterAttrs{
				LinkIndex: link.Attrs().Index,
				Parent:    netlink.HANDLE_MIN_INGRESS,
				Handle:    netlink.MakeHandle(0x2026, 1),
				Protocol:  unix.ETH_P_ALL,
				Priority:  1,
			},
			Fd:           prog.FD(),
			Name:         "dae_test_lan_ingress_l2",
			DirectAction: true,
		}
		if err := netlink.FilterAdd(filter); err != nil {
			return fmt.Errorf("attach lan ingress filter to %s: %w", ifName, err)
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
		"EVENT_RATE": eventRateValue(),
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
