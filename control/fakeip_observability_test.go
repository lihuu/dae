/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"fmt"
	"net/netip"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

type fakeipEventHook struct {
	mu      sync.Mutex
	entries []*logrus.Entry
}

func (h *fakeipEventHook) Levels() []logrus.Level {
	return logrus.AllLevels
}

func (h *fakeipEventHook) Fire(entry *logrus.Entry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, entry)
	return nil
}

func (h *fakeipEventHook) find(event string) *logrus.Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.entries {
		if e.Data["event"] == event {
			return e
		}
	}
	return nil
}

func (h *fakeipEventHook) findAll(event string) []*logrus.Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	var result []*logrus.Entry
	for _, e := range h.entries {
		if e.Data["event"] == event {
			result = append(result, e)
		}
	}
	return result
}

func newTestLoggerWithHook() (*logrus.Logger, *fakeipEventHook) {
	log := logrus.New()
	log.SetLevel(logrus.TraceLevel)
	hook := &fakeipEventHook{}
	log.AddHook(hook)
	return log, hook
}

func TestFakeIPEventBase(t *testing.T) {
	fields := fakeipEventBase("fakeip_flow")
	require.Equal(t, "fakeip", fields["component"])
	require.Equal(t, "fakeip_flow", fields["event"])
	require.Equal(t, 1, fields["event_version"])
}

func TestLogFakeIPDNSAnswerOK(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &DnsController{log: log}

	fakeip := netip.MustParseAddr("198.18.0.102")
	c.logFakeIPDNSAnswerOK("www.wikipedia.org.", fakeip, 60, true)

	entry := hook.find("fakeip_dns_answer")
	require.NotNil(t, entry, "fakeip_dns_answer event should be emitted")
	require.Equal(t, logrus.DebugLevel, entry.Level)
	require.Equal(t, "fakeip", entry.Data["component"])
	require.Equal(t, 1, entry.Data["event_version"])
	require.Equal(t, "www.wikipedia.org.", entry.Data["domain"])
	require.Equal(t, "A", entry.Data["qtype"])
	require.Equal(t, "ok", entry.Data["result"])
	require.Equal(t, "198.18.0.102", entry.Data["fakeip"])
	require.Equal(t, 60, entry.Data["ttl"])
	require.Equal(t, true, entry.Data["allocated"])
}

func TestLogFakeIPDNSAnswerOK_ExistingAllocation(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &DnsController{log: log}

	fakeip := netip.MustParseAddr("198.18.0.102")
	c.logFakeIPDNSAnswerOK("www.wikipedia.org.", fakeip, 60, false)

	entry := hook.find("fakeip_dns_answer")
	require.NotNil(t, entry)
	require.Equal(t, false, entry.Data["allocated"])
}

func TestLogFakeIPDNSAnswerNODATA(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &DnsController{log: log}

	c.logFakeIPDNSAnswerNODATA("www.wikipedia.org.", 28) // 28 = AAAA

	entry := hook.find("fakeip_dns_answer")
	require.NotNil(t, entry, "fakeip_dns_answer NODATA event should be emitted")
	require.Equal(t, logrus.DebugLevel, entry.Level)
	require.Equal(t, "www.wikipedia.org.", entry.Data["domain"])
	require.Equal(t, "AAAA", entry.Data["qtype"])
	require.Equal(t, "ok", entry.Data["result"])
	require.Equal(t, "nodata", entry.Data["answer"])
	require.Nil(t, entry.Data["fakeip"], "NODATA should not have fakeip field")
}

func TestLogFakeIPDNSAnswerFailed(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &DnsController{log: log}

	c.logFakeIPDNSAnswerFailed("www.wikipedia.org.", 1, "fakeip_store_error", fmt.Errorf("disk full"))

	entry := hook.find("fakeip_dns_answer")
	require.NotNil(t, entry, "fakeip_dns_answer failed event should be emitted")
	require.Equal(t, logrus.WarnLevel, entry.Level)
	require.Equal(t, "www.wikipedia.org.", entry.Data["domain"])
	require.Equal(t, "A", entry.Data["qtype"])
	require.Equal(t, "failed", entry.Data["result"])
	require.Equal(t, "fakeip_store_error", entry.Data["error_class"])
	require.Equal(t, "disk full", entry.Data["error"])
}

func TestLogFakeIPFlow(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &ControlPlane{log: log}

	fakeip := netip.MustParseAddr("198.18.0.102")
	client := netip.MustParseAddrPort("192.168.2.7:54321")
	c.logFakeIPFlow("www.wikipedia.org.", fakeip, client, "tcp", 443, "proxy_failover", "failover", "www.wikipedia.org:443")

	entry := hook.find("fakeip_flow")
	require.NotNil(t, entry, "fakeip_flow event should be emitted")
	require.Equal(t, logrus.InfoLevel, entry.Level)
	require.Equal(t, "fakeip", entry.Data["component"])
	require.Equal(t, 1, entry.Data["event_version"])
	require.Equal(t, "www.wikipedia.org.", entry.Data["domain"])
	require.Equal(t, "198.18.0.102", entry.Data["fakeip"])
	require.Equal(t, "192.168.2.7", entry.Data["client"])
	require.Equal(t, "tcp", entry.Data["network"])
	require.Equal(t, uint16(443), entry.Data["port"])
	require.Equal(t, "proxy_failover", entry.Data["outbound"])
	require.Equal(t, "ok", entry.Data["result"])
	require.Equal(t, "failover", entry.Data["policy"])
	require.Equal(t, "www.wikipedia.org:443", entry.Data["dial_target"])
}

func TestLogFakeIPFlow_UDP(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &ControlPlane{log: log}

	fakeip := netip.MustParseAddr("198.18.0.103")
	client := netip.MustParseAddrPort("192.168.2.8:12345")
	c.logFakeIPFlow("dns.google.", fakeip, client, "udp", 53, "proxy", "fixed", "dns.google:53")

	entry := hook.find("fakeip_flow")
	require.NotNil(t, entry)
	require.Equal(t, "udp", entry.Data["network"])
	require.Equal(t, "proxy", entry.Data["outbound"])
}

func TestLogFakeIPFlow_OmitsEmptyOptional(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &ControlPlane{log: log}

	fakeip := netip.MustParseAddr("198.18.0.102")
	client := netip.MustParseAddrPort("192.168.2.7:54321")
	c.logFakeIPFlow("www.wikipedia.org.", fakeip, client, "tcp", 443, "direct", "", "")

	entry := hook.find("fakeip_flow")
	require.NotNil(t, entry)
	require.Equal(t, "direct", entry.Data["outbound"])
	require.Nil(t, entry.Data["policy"], "empty policy should be omitted")
	require.Nil(t, entry.Data["dial_target"], "empty dial_target should be omitted")
}

func TestLogFakeIPUnknown(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &ControlPlane{log: log}

	fakeip := netip.MustParseAddr("198.18.1.12")
	client := netip.MustParseAddrPort("192.168.2.9:44000")
	c.logFakeIPUnknown(fakeip, client, "tcp", 443)

	entry := hook.find("fakeip_unknown")
	require.NotNil(t, entry, "fakeip_unknown event should be emitted")
	require.Equal(t, logrus.WarnLevel, entry.Level)
	require.Equal(t, "198.18.1.12", entry.Data["fakeip"])
	require.Equal(t, "192.168.2.9", entry.Data["client"])
	require.Equal(t, "tcp", entry.Data["network"])
	require.Equal(t, uint16(443), entry.Data["port"])
	require.Equal(t, "rejected", entry.Data["result"])
	require.Equal(t, "fakeip_unknown_mapping", entry.Data["error_class"])
}

func TestLogFakeIPDirectResolveOK(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &ControlPlane{log: log}

	fakeip := netip.MustParseAddr("198.18.0.130")
	resolvedIP := netip.MustParseAddr("93.184.216.34")
	c.logFakeIPDirectResolveOK("example.com.", fakeip, "cn", "tcp", 443, resolvedIP)

	entry := hook.find("fakeip_direct_resolve")
	require.NotNil(t, entry, "fakeip_direct_resolve ok event should be emitted")
	require.Equal(t, logrus.InfoLevel, entry.Level)
	require.Equal(t, "example.com.", entry.Data["domain"])
	require.Equal(t, "198.18.0.130", entry.Data["fakeip"])
	require.Equal(t, "cn", entry.Data["upstream"])
	require.Equal(t, "tcp", entry.Data["network"])
	require.Equal(t, uint16(443), entry.Data["port"])
	require.Equal(t, "ok", entry.Data["result"])
	require.Equal(t, "93.184.216.34", entry.Data["resolved_ip"])
}

func TestLogFakeIPDirectResolveFailed(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &ControlPlane{log: log}

	fakeip := netip.MustParseAddr("198.18.0.130")
	c.logFakeIPDirectResolveFailed("example.com.", fakeip, "cn", "tcp", 443, fmt.Errorf("no A records"))

	entry := hook.find("fakeip_direct_resolve")
	require.NotNil(t, entry, "fakeip_direct_resolve failed event should be emitted")
	require.Equal(t, logrus.WarnLevel, entry.Level)
	require.Equal(t, "failed", entry.Data["result"])
	require.Equal(t, "direct_resolve_failed", entry.Data["error_class"])
	require.Equal(t, "no A records", entry.Data["error"])
}

func TestLogFakeIPDialError(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &ControlPlane{log: log}

	fakeip := netip.MustParseAddr("198.18.0.120")
	client := netip.MustParseAddrPort("192.168.2.7:54321")
	c.logFakeIPDialError("www.github.com.", fakeip, client, "tcp", 443, "proxy_failover", "www.github.com:443", "proxy_dial_failed", fmt.Errorf("connection refused"))

	entry := hook.find("fakeip_dial_error")
	require.NotNil(t, entry, "fakeip_dial_error event should be emitted")
	require.Equal(t, logrus.WarnLevel, entry.Level)
	require.Equal(t, "www.github.com.", entry.Data["domain"])
	require.Equal(t, "198.18.0.120", entry.Data["fakeip"])
	require.Equal(t, "192.168.2.7", entry.Data["client"])
	require.Equal(t, "tcp", entry.Data["network"])
	require.Equal(t, uint16(443), entry.Data["port"])
	require.Equal(t, "proxy_failover", entry.Data["outbound"])
	require.Equal(t, "www.github.com:443", entry.Data["dial_target"])
	require.Equal(t, "failed", entry.Data["result"])
	require.Equal(t, "proxy_dial_failed", entry.Data["error_class"])
	require.Equal(t, "connection refused", entry.Data["error"])
}

func TestLogFakeIPReloadOK(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &ControlPlane{log: log}

	c.logFakeIPReloadOK(103, "/var/lib/dae/fakeip.db", "198.18.0.0/15")

	entry := hook.find("fakeip_reload")
	require.NotNil(t, entry, "fakeip_reload ok event should be emitted")
	require.Equal(t, logrus.InfoLevel, entry.Level)
	require.Equal(t, "ok", entry.Data["result"])
	require.Equal(t, 103, entry.Data["replayed"])
	require.Equal(t, "/var/lib/dae/fakeip.db", entry.Data["store"])
	require.Equal(t, "198.18.0.0/15", entry.Data["cidr"])
}

func TestLogFakeIPReloadFailed(t *testing.T) {
	log, hook := newTestLoggerWithHook()
	c := &ControlPlane{log: log}

	c.logFakeIPReloadFailed("reload_replay_failed", fmt.Errorf("store corrupted"))

	entry := hook.find("fakeip_reload")
	require.NotNil(t, entry, "fakeip_reload failed event should be emitted")
	require.Equal(t, logrus.WarnLevel, entry.Level)
	require.Equal(t, "failed", entry.Data["result"])
	require.Equal(t, "reload_replay_failed", entry.Data["error_class"])
	require.Equal(t, "store corrupted", entry.Data["error"])
}

func TestFakeIPEventsNilLogger(t *testing.T) {
	fakeip := netip.MustParseAddr("198.18.0.102")
	client := netip.MustParseAddrPort("192.168.2.7:54321")

	dns := &DnsController{log: nil}
	dns.logFakeIPDNSAnswerOK("test.", fakeip, 60, true)
	dns.logFakeIPDNSAnswerNODATA("test.", 28)
	dns.logFakeIPDNSAnswerFailed("test.", 1, "fakeip_store_error", fmt.Errorf("err"))

	cp := &ControlPlane{log: nil}
	cp.logFakeIPFlow("test.", fakeip, client, "tcp", 443, "proxy", "fixed", "test:443")
	cp.logFakeIPUnknown(fakeip, client, "tcp", 443)
	cp.logFakeIPDirectResolveOK("test.", fakeip, "cn", "tcp", 443, fakeip)
	cp.logFakeIPDirectResolveFailed("test.", fakeip, "cn", "tcp", 443, fmt.Errorf("err"))
	cp.logFakeIPDialError("test.", fakeip, client, "tcp", 443, "proxy", "test:443", "proxy_dial_failed", fmt.Errorf("err"))
	cp.logFakeIPReloadOK(0, "", "")
	cp.logFakeIPReloadFailed("reload_replay_failed", fmt.Errorf("err"))
}

func TestFakeIPEventsLogLevelSuppressed(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.ErrorLevel)

	hook := &fakeipEventHook{}
	log.AddHook(hook)

	dns := &DnsController{log: log}
	dns.logFakeIPDNSAnswerOK("test.", netip.MustParseAddr("198.18.0.1"), 60, true)
	dns.logFakeIPDNSAnswerNODATA("test.", 28)

	cp := &ControlPlane{log: log}
	cp.logFakeIPFlow("test.", netip.MustParseAddr("198.18.0.1"), netip.MustParseAddrPort("192.168.2.7:1234"), "tcp", 443, "proxy", "fixed", "test:443")

	require.Empty(t, hook.entries, "debug/info events should be suppressed at error level")
}
