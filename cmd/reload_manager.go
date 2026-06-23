/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daeuniverse/dae/config"
	"github.com/daeuniverse/dae/control"
	"github.com/daeuniverse/dae/pkg/config_parser"
	"github.com/daeuniverse/dae/pkg/rulesload"
	"github.com/sirupsen/logrus"
)

type reloadManager struct {
	reloadReqs                   chan reloadRequest
	runStateChanges              chan struct{}
	sigs                         <-chan os.Signal
	reloading                    atomic.Bool
	reloadActive                 atomic.Bool
	reloadPending                atomic.Bool
	mu                           sync.Mutex
	reloadingErr                 error
	lastRetirementMu             sync.Mutex
	lastRetirementCancel         context.CancelFunc
	pendingStagedHandoff         *stagedReloadHandoff
	pendingRetirementDone        <-chan struct{}
	pendingReloadRequestedAt     time.Time
	pendingReloadRequestedAtMono uint64

	// pendingReloadCollector carries the rules-load SummaryCollector for the
	// in-flight reload across the goroutine boundary between the reload handler
	// (which builds the new control plane) and the main loop (which sees the
	// reload finish and emits the rules_load_summary event). Set by the reload
	// handler before notifyRunStateChange; read and cleared by takePendingReloadCollector.
	pendingReloadCollectorMu sync.Mutex
	pendingReloadCollector   *SummaryCollector
}

func newReloadManager(reloadReqs chan reloadRequest, runStateChanges chan struct{}, sigs <-chan os.Signal) *reloadManager {
	return &reloadManager{
		reloadReqs:      reloadReqs,
		runStateChanges: runStateChanges,
		sigs:            sigs,
	}
}

func (m *reloadManager) queueReloadRequest(log *logrus.Logger, req reloadRequest) bool {
	return tryQueueReloadRequest(log, m.reloadReqs, &m.reloadActive, &m.reloadPending, req)
}

func (m *reloadManager) beginHandoff() {
	beginReloadHandoff(&m.reloading, m.runStateChanges)
}

func (m *reloadManager) setReloadError(err error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.reloadingErr = err
	m.mu.Unlock()
}

func (m *reloadManager) reloadError() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reloadingErr
}

// setPendingReloadCollector stashes the in-flight reload's rules-load
// SummaryCollector so the main loop can emit its summary after the reload
// completes. Safe for nil m.
func (m *reloadManager) setPendingReloadCollector(c *SummaryCollector) {
	if m == nil {
		return
	}
	m.pendingReloadCollectorMu.Lock()
	m.pendingReloadCollector = c
	m.pendingReloadCollectorMu.Unlock()
}

// takePendingReloadCollector returns and clears the SummaryCollector stashed
// by setPendingReloadCollector. Returns nil if none was set or m is nil.
func (m *reloadManager) takePendingReloadCollector() *SummaryCollector {
	if m == nil {
		return nil
	}
	m.pendingReloadCollectorMu.Lock()
	defer m.pendingReloadCollectorMu.Unlock()
	c := m.pendingReloadCollector
	m.pendingReloadCollector = nil
	return c
}

// peekPendingReloadCollector returns the stashed SummaryCollector without
// clearing it. Used by the retirement goroutine to emit a follow-up
// rules_load_stage stage=reload_retire event after the main loop has
// already taken (and emitted) the rules_load_summary at [Reload] Finished.
// Returns nil if none was set or m is nil.
func (m *reloadManager) peekPendingReloadCollector() *SummaryCollector {
	if m == nil {
		return nil
	}
	m.pendingReloadCollectorMu.Lock()
	defer m.pendingReloadCollectorMu.Unlock()
	return m.pendingReloadCollector
}

func (m *reloadManager) coalesceReloadRequest(req reloadRequest) reloadRequest {
	reloadStartedAt := req.requestedAt
	if reloadStartedAt.IsZero() {
		reloadStartedAt = time.Now()
	}
	req.requestedAt = reloadStartedAt
coalesce:
	for {
		select {
		case nextReq := <-m.reloadReqs:
			req = nextReq
			if req.requestedAt.IsZero() {
				req.requestedAt = time.Now()
			}
			continue
		default:
			break coalesce
		}
	}
	return req
}

func (m *reloadManager) setPendingStagedHandoff(handoff *stagedReloadHandoff, requestedAt time.Time, requestedAtMono uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingStagedHandoff = handoff
	m.pendingReloadRequestedAt = requestedAt
	m.pendingReloadRequestedAtMono = requestedAtMono
}

func (m *reloadManager) clearPendingStagedHandoff() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pendingStagedHandoff = nil
	m.mu.Unlock()
}

func (m *reloadManager) currentPendingStagedHandoff() *stagedReloadHandoff {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingStagedHandoff
}

func (m *reloadManager) setPendingReloadMetadata(requestedAt time.Time, requestedAtMono uint64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pendingReloadRequestedAt = requestedAt
	m.pendingReloadRequestedAtMono = requestedAtMono
	m.mu.Unlock()
}

func (m *reloadManager) clearPendingRetirement() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pendingRetirementDone = nil
	m.mu.Unlock()
}

func (m *reloadManager) takePendingRetirementDone() <-chan struct{} {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	done := m.pendingRetirementDone
	m.pendingRetirementDone = nil
	return done
}

func (m *reloadManager) buildShutdownHandoff() *signalShutdownStagedHandoff {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pendingStagedHandoff == nil {
		return nil
	}
	return &signalShutdownStagedHandoff{
		oldListener:     m.pendingStagedHandoff.oldListener,
		oldControlPlane: m.pendingStagedHandoff.oldControlPlane,
		newListener:     m.pendingStagedHandoff.newListener,
		newControlPlane: m.pendingStagedHandoff.newControlPlane,
	}
}

func (m *reloadManager) pendingDNSHandoffActive(current *control.ControlPlane) bool {
	if m == nil {
		return false
	}
	handoff := m.currentPendingStagedHandoff()
	return handoff != nil &&
		handoff.oldControlPlane != nil &&
		handoff.oldControlPlane.SharesActiveDnsControllerWith(current)
}

type preparedDNSHandoffHookCallbacks struct {
	reuseController func() bool
	reuseListener   func() bool
	stopOldListener func() error
}

type preparedDNSHandoffHooks struct {
	reuseHook func() error
	startHook func() error
}

func buildPreparedDNSHandoffHooks(log *logrus.Logger, enableReuse bool, callbacks preparedDNSHandoffHookCallbacks) preparedDNSHandoffHooks {
	var hooks preparedDNSHandoffHooks
	if enableReuse {
		hooks.reuseHook = func() error {
			if callbacks.reuseController != nil {
				_ = callbacks.reuseController()
			}
			if callbacks.reuseListener != nil && callbacks.reuseListener() {
				return nil
			}
			return nil
		}
	}
	hooks.startHook = func() error {
		if callbacks.reuseListener != nil && callbacks.reuseListener() {
			return nil
		}
		if callbacks.stopOldListener == nil {
			return nil
		}
		if err := callbacks.stopOldListener(); err != nil {
			if log != nil {
				log.WithError(err).Warnln("[Reload] Failed to stop previous DNS listener before staged cutover")
			}
			return err
		}
		return nil
	}
	return hooks
}

func (m *reloadManager) installPreparedDNSHandoffHooks(log *logrus.Logger, current *control.ControlPlane, conf *config.Config) {
	if m == nil || current == nil || conf == nil {
		return
	}
	handoff := m.currentPendingStagedHandoff()
	if handoff == nil {
		return
	}
	// The DNS controller can be reused when either:
	// (a) the full DNS config fingerprint is unchanged (cheap: no runtime update needed), or
	// (b) only generation-local runtime fields changed (TTL, direct_upstream) while the
	//     persistent FakeIP store identity is unchanged. TryUpdateRuntime will swap the
	//     runtime state in place, preserving the shared FakeIP store across reload.
	reuseController := dnsConfigEqual(handoff.oldConf, conf) ||
		fakeIPStoreIdentity(handoff.oldConf.Dns) == fakeIPStoreIdentity(conf.Dns)
	hooks := buildPreparedDNSHandoffHooks(log, reuseController, preparedDNSHandoffHookCallbacks{
		reuseController: func() bool {
			return current.ReuseDNSControllerFrom(handoff.oldControlPlane)
		},
		reuseListener: func() bool {
			return current.ReuseDNSListenerFrom(handoff.oldControlPlane)
		},
		stopOldListener: handoff.oldControlPlane.StopDNSListener,
	})
	if hooks.reuseHook != nil {
		current.SetPreparedDNSReuseHook(hooks.reuseHook)
	}
	current.SetPreparedDNSStartHook(hooks.startHook)
}

func (m *reloadManager) finishReloadFailure() {
	m.reloading.Store(false)
	m.reloadActive.Store(false)
	clearReloadPending(&m.reloadPending)
}

func (m *reloadManager) finishReloadSuccess() {
	m.reloading.Store(false)
	m.reloadActive.Store(false)
	releaseReloadPendingAfterRetirement(&m.reloadPending, m.takePendingRetirementDone())
}

func (m *reloadManager) startControlPlaneRetirement(
	log *logrus.Logger,
	oldControlPlane *control.ControlPlane,
	successor *control.ControlPlane,
	oldCancel context.CancelFunc,
	abortConnections bool,
	hasOverlap bool,
) {
	if m == nil || oldControlPlane == nil {
		return
	}
	m.lastRetirementMu.Lock()
	if m.lastRetirementCancel != nil {
		m.lastRetirementCancel()
	}
	retireCtx, retireCancel := context.WithCancel(context.Background())
	m.lastRetirementCancel = retireCancel
	m.lastRetirementMu.Unlock()

	if log != nil {
		log.Warnln("[Reload] Retiring old control plane")
	}
	retirementDone := make(chan struct{})
	// lastRetirementMu only serializes cancellation/replacement of the previous
	// retirement goroutine. The timing metadata below belongs to the reload
	// manager state itself, so it is read under m.mu instead. This split is safe
	// because reload requests are handled by a single worker goroutine.
	m.mu.Lock()
	m.pendingRetirementDone = retirementDone
	drainBudget := remainingReloadRetirementBudget(m.pendingReloadRequestedAt, reloadTotalSwitchBudget)
	staleBeforeNs := m.pendingReloadRequestedAtMono
	m.mu.Unlock()

	// Snapshot the rules-load collector for this reload (if any). The
	// retirement goroutine emits a rules_load_stage stage=reload_retire
	// event when it completes so operators can correlate the old-generation
	// drain duration with the matching rules_load_summary. Picking the
	// collector at retirement-start (rather than reading the in-flight
	// pendingReloadCollector when retirement finishes) avoids racing with
	// the main loop, which clears the pointer on takePendingReloadCollector.
	retireCollector := m.peekPendingReloadCollector()
	retireStart := time.Now()

	go func(done chan struct{}) {
		defer close(done)

		oldControlPlane.MarkRetired()
		retireControlPlaneConnections(log, retireCtx, oldControlPlane, abortConnections, hasOverlap, drainBudget)

		if oldCancel != nil {
			oldCancel()
		}
		if closeErr := oldControlPlane.Close(); closeErr != nil && log != nil {
			log.WithError(closeErr).Warnln("[Reload] Old control plane close did not finish cleanly")
		}
		if successor != nil {
			successor.RunReloadRetirementCleanup(staleBeforeNs)
		}
		if log != nil {
			log.Warnln("[Reload] Retired old control plane")
		}

		// Emit a rules_load_stage stage=reload_retire event after retirement
		// completes so operators can identify slow drain/forced-close paths.
		// The rules_load_summary for this reload was already emitted at
		// [Reload] Finished; this is a follow-up stage event only.
		if log != nil && retireCollector != nil {
			rulesload.EmitStage(log, rulesload.LifecycleReload, rulesload.StageReloadRetire,
				time.Since(retireStart).Milliseconds(), 0, 0, "")
		}
	}(retirementDone)
}

func (m *reloadManager) refreshPprofServer(log *logrus.Logger, server **http.Server, port uint16) {
	if server == nil {
		return
	}
	if *server != nil {
		pprofCtx, pprofCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = (*server).Shutdown(pprofCtx)
		pprofCancel()
		*server = nil
	}
	if port != 0 {
		pprofAddr := "localhost:" + strconv.Itoa(int(port))
		*server = &http.Server{Addr: pprofAddr, Handler: nil}
		go func() { _ = (*server).ListenAndServe() }()
	}
}

func dnsConfigEqual(oldConf *config.Config, newConf *config.Config) bool {
	if oldConf == nil || newConf == nil {
		return false
	}
	return dnsConfigFingerprint(oldConf.Dns) == dnsConfigFingerprint(newConf.Dns)
}

// dnsConfigFingerprint must be kept in sync with config.Dns. The companion
// TestDNSConfigFingerprintCoversAllDnsFields fails when new top-level DNS
// fields are added without updating this fingerprint.
func dnsConfigFingerprint(dns config.Dns) string {
	var b strings.Builder
	writeKeyableStrings := func(name string, values []config.KeyableString) {
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(strconv.Itoa(len(values)))
		for _, value := range values {
			b.WriteByte(':')
			b.WriteString(strconv.Quote(string(value)))
		}
		b.WriteByte(';')
	}
	writeFunction := func(f *config_parser.Function) {
		if f == nil {
			b.WriteString("<nil>")
			return
		}
		b.WriteString(f.String(true, true, false))
	}
	writeFunctionOrString := func(name string, value config.FunctionOrString) {
		b.WriteString(name)
		b.WriteByte('=')
		switch value := value.(type) {
		case string:
			b.WriteString("string:")
			b.WriteString(strconv.Quote(value))
		case *config_parser.Function:
			b.WriteString("function:")
			writeFunction(value)
		case []*config_parser.Function:
			b.WriteString("functions:")
			b.WriteString(strconv.Itoa(len(value)))
			for _, f := range value {
				b.WriteByte(':')
				writeFunction(f)
			}
		default:
			b.WriteString("unsupported:")
			fmt.Fprintf(&b, "%T", value)
		}
		b.WriteByte(';')
	}
	writeRules := func(name string, rules []*config_parser.RoutingRule) {
		b.WriteString(name)
		b.WriteByte('=')
		b.WriteString(strconv.Itoa(len(rules)))
		for _, rule := range rules {
			b.WriteByte(':')
			if rule == nil {
				b.WriteString("<nil>")
				continue
			}
			b.WriteString(rule.String(false, true, true))
		}
		b.WriteByte(';')
	}
	writeRouting := func(name string, routing config.DnsRouting) {
		b.WriteString(name)
		b.WriteByte('{')
		writeRules("request.rules", routing.Request.Rules)
		writeFunctionOrString("request.fallback", routing.Request.Fallback)
		writeRules("response.rules", routing.Response.Rules)
		writeFunctionOrString("response.fallback", routing.Response.Fallback)
		b.WriteByte('}')
	}

	b.WriteString("ipversion_prefer=")
	b.WriteString(strconv.Itoa(dns.IpVersionPrefer))
	b.WriteByte(';')
	writeKeyableStrings("fixed_domain_ttl", dns.FixedDomainTtl)
	writeKeyableStrings("upstream", dns.Upstream)
	writeRouting("routing", dns.Routing)
	b.WriteString("bind=")
	b.WriteString(strconv.Quote(dns.Bind))
	b.WriteByte(';')
	b.WriteString("optimistic_cache=")
	b.WriteString(strconv.FormatBool(dns.OptimisticCache))
	b.WriteByte(';')
	b.WriteString("optimistic_cache_ttl=")
	b.WriteString(strconv.Itoa(dns.OptimisticCacheTtl))
	b.WriteByte(';')
	b.WriteString("max_cache_size=")
	b.WriteString(strconv.Itoa(dns.MaxCacheSize))
	b.WriteByte(';')
	b.WriteString("fakeip.enabled=")
	b.WriteString(strconv.FormatBool(dns.FakeIP.Enabled))
	b.WriteByte(';')
	b.WriteString("fakeip.inet4_range=")
	b.WriteString(strconv.Quote(dns.FakeIP.Inet4Range))
	b.WriteByte(';')
	b.WriteString("fakeip.ttl=")
	b.WriteString(strconv.Itoa(dns.FakeIP.TTL))
	b.WriteByte(';')
	b.WriteString("fakeip.store=")
	b.WriteString(strconv.Quote(dns.FakeIP.Store))
	b.WriteByte(';')
	b.WriteString("fakeip.direct_upstream=")
	b.WriteString(strconv.Quote(dns.FakeIP.DirectUpstream))
	b.WriteByte(';')
	return b.String()
}

// fakeIPStoreIdentity returns a stable identity for FakeIP store compatibility
// across reloads. Changes to enabled, inet4_range, or store require a full restart.
func fakeIPStoreIdentity(dns config.Dns) string {
	return dns.FakeIP.StoreIdentity()
}

// validateFakeIPReloadCompatibility checks whether the FakeIP store identity
// (enabled, inet4_range, store path) has changed between the old and new
// configurations. If the identity differs, the persistent BoltDB-backed FakeIP
// store cannot be shared across reload, and a full dae restart is required.
//
// TTL and direct_upstream changes are reload-compatible (they only affect
// generation-local runtime state, not the store identity).
//
// Returns nil if either config is nil (first start or reload-from-nothing).
func validateFakeIPReloadCompatibility(oldConf, newConf *config.Config) error {
	if oldConf == nil || newConf == nil {
		return nil
	}
	if fakeIPStoreIdentity(oldConf.Dns) != fakeIPStoreIdentity(newConf.Dns) {
		return fmt.Errorf("dns.fakeip inet4_range/store changes require a full dae restart")
	}
	return nil
}
