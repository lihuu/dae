/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component/outbound/dialer"
	_ "github.com/daeuniverse/outbound/dialer"
	"github.com/daeuniverse/outbound/netproxy"
	"github.com/sirupsen/logrus"
)

// FailoverConfig holds the configuration needed to create a failover controller
// for a DialerGroup. Only used when the policy is DialerSelectionPolicy_Failover.
//
// PrimaryCandidateIdxs is the ordered list of primary candidate dialer indices.
// Index 0 is the initial current primary; the remaining indices are standby
// candidates in the order they should be probed during rotation. FallbackIdx is
// the index of the fixed fallback dialer and never participates in rotation.
type FailoverConfig struct {
	PrimaryCandidateIdxs []int
	FallbackIdx          int
	Recovery             FailoverRecoveryConfig
}

var ErrNoAliveDialer = fmt.Errorf("no alive dialer")

type DialerGroup struct {
	netproxy.Dialer

	log  *logrus.Logger
	Name string

	Dialers []*dialer.Dialer

	selectionState   atomic.Pointer[dialerGroupSelectionState]
	selectionStateMu sync.Mutex

	dialersAnnotations  []*dialer.Annotation
	checkTolerance      time.Duration
	aliveChangeCallback func(alive bool, networkType *dialer.NetworkType, isInit bool)

	resuscitateLastTime atomic.Int64
	noAliveLogLastTimes [8]atomic.Int64

	cachedMinCheckInterval time.Duration

	// failoverController is non-nil only for failover policy groups.
	failoverController *FailoverController
	// failoverCfg maps the ordered Primary candidates and fixed Fallback role to
	// actual dialer indices. Non-nil only for failover policy groups.
	failoverCfg *FailoverConfig
}

type dialerGroupSelectionState struct {
	policy          DialerSelectionPolicy
	aliveDialerSets [8]*dialer.AliveDialerSet
}

// ReloadSelectionFallback records the candidate selected by a fresh group
// before reload health inheritance applies the previous generation's state.
type ReloadSelectionFallback [8]*dialer.Dialer

func NewDialerGroup(
	option *dialer.GlobalOption,
	name string,
	dialers []*dialer.Dialer,
	dialersAnnotations []*dialer.Annotation,
	p DialerSelectionPolicy,
	aliveChangeCallback func(alive bool, networkType *dialer.NetworkType, isInit bool),
	failoverCfg *FailoverConfig,
) *DialerGroup {
	log := option.Log

	group := &DialerGroup{
		log:                 log,
		Name:                name,
		Dialers:             dialers,
		dialersAnnotations:  dialersAnnotations,
		checkTolerance:      option.CheckTolerance,
		aliveChangeCallback: aliveChangeCallback,
	}

	if p.Policy == consts.DialerSelectionPolicy_Failover && failoverCfg != nil {
		// Failover policy: use the failover controller instead of AliveDialerSet.
		group.failoverCfg = failoverCfg
		// Map PrimaryCandidateIdxs to dialer pointers in configured role order.
		primaryCandidates := make([]*dialer.Dialer, 0, len(failoverCfg.PrimaryCandidateIdxs))
		for _, idx := range failoverCfg.PrimaryCandidateIdxs {
			primaryCandidates = append(primaryCandidates, dialers[idx])
		}
		group.failoverController = NewFailoverControllerWithCandidates(
			log, name,
			primaryCandidates,
			dialers[failoverCfg.FallbackIdx],
			failoverCfg.Recovery,
		)
		// Failover doesn't use AliveDialerSet, so we store a minimal state.
		group.selectionState.Store(&dialerGroupSelectionState{policy: p})
		// Activate every Primary candidate's connectivity check so that
		// traffic-driven TCP failures can trigger the failover controller's
		// health transition callback. The event callback wiring that actually
		// drives failover transitions is added in a later packet; this packet
		// only ensures the candidates' connectivity-check workers are
		// registered/kept-alive so health callbacks can fire later.
		for _, idx := range failoverCfg.PrimaryCandidateIdxs {
			dialers[idx].ActivateCheck()
		}
	} else {
		state := group.buildSelectionState(p, true)
		group.registerAliveDialerSets(state.aliveDialerSets)
		group.selectionState.Store(state)
	}
	group.cachedMinCheckInterval = group.MinCheckInterval()

	// Initialize BPF connectivity map for all outbound groups.
	// This must be called for failover groups too, otherwise BPF will
	// consider the outbound as dead and drop all packets.
	for _, nt := range standardSelectionNetworkTypes() {
		aliveChangeCallback(true, nt, true)
	}

	return group
}

func (g *DialerGroup) Close() error {
	if g.failoverController != nil {
		g.failoverController.Close()
	}
	g.unregisterAliveDialerSets(g.currentSelectionState().aliveDialerSets)
	return nil
}

// HasFailoverController returns true if this group uses a failover policy.
func (g *DialerGroup) HasFailoverController() bool {
	return g.failoverController != nil
}

// SetFailoverEventCallback installs a failover event callback on the
// controller. It is a no-op if the group does not use a failover policy.
// Must be called immediately after NewDialerGroup, before traffic starts.
// Keeping this separate from NewDialerGroup avoids changing its signature
// (which has many existing callers across tests and production).
func (g *DialerGroup) SetFailoverEventCallback(cb FailoverEventCallback) {
	if g.failoverController != nil {
		g.failoverController.SetEventCallback(cb)
	}
}

// FailoverIdentity returns the names of the primary and fallback dialers
// for failover identity matching during reload. Returns empty strings if
// the group does not use failover. The primary name is the current primary
// candidate (primaryCandidates[currentPrimary]).
func (g *DialerGroup) FailoverIdentity() (primaryName, fallbackName string) {
	if g.failoverController == nil {
		return "", ""
	}
	g.failoverController.mu.Lock()
	primary := g.failoverController.primaryDialer()
	g.failoverController.mu.Unlock()
	return dialerName(primary), dialerName(g.failoverController.fallback)
}

// CaptureFailoverSnapshot captures the failover controller state for reload.
// Returns nil if the group does not use failover.
func (g *DialerGroup) CaptureFailoverSnapshot() *FailoverControllerSnapshot {
	if g.failoverController == nil {
		return nil
	}
	snap := g.failoverController.CaptureSnapshot()
	return &snap
}

// RestoreFailoverSnapshot restores the failover controller state from a reload.
// This should only be called when the primary and fallback identities match
// between old and new groups.
func (g *DialerGroup) RestoreFailoverSnapshot(snap *FailoverControllerSnapshot) {
	if g.failoverController == nil || snap == nil {
		return
	}
	g.failoverController.RestoreSnapshot(*snap)
}

// PrepareFailoverReloadFrom compares this group's failover identity against the
// previous generation's group and returns a transactional transfer. On
// compatibility (same fixed fallback name, same ordered primary-candidate
// names, identical recovery policy) the returned transfer has paused the old
// generation and restored this group's controller with the captured rotation
// state; the reason string is empty. On incompatibility the returned transfer
// is nil and the reason is the deterministic reset reason
// (fallback_changed > primary_candidates_changed > recovery_policy_changed);
// this group's controller stays at its fresh primary[0] initial candidate and
// the info-level failover_rotation_state_reset log has already been emitted by
// the controller.
//
// The caller drives the transfer to a terminal state with Commit (keep the new
// generation) or Rollback (discard the new generation's restore and resume
// exactly one old-generation probe). Exactly one of Commit/Rollback takes
// effect; both are idempotent.
//
// Returns (nil, "") if either group does not use a failover controller.
func (g *DialerGroup) PrepareFailoverReloadFrom(old *DialerGroup) (*FailoverReloadTransfer, string) {
	if g == nil || old == nil {
		return nil, ""
	}
	if !g.HasFailoverController() || !old.HasFailoverController() {
		return nil, ""
	}
	return g.failoverController.prepareReloadTransfer(old.failoverController)
}

func (g *DialerGroup) SetSelectionPolicy(policy DialerSelectionPolicy) {
	g.selectionStateMu.Lock()
	defer g.selectionStateMu.Unlock()

	current := g.currentSelectionState()
	currentNeedsAliveState := policyNeedsAliveState(current.policy.Policy)
	newNeedsAliveState := policyNeedsAliveState(policy.Policy)

	switch {
	case currentNeedsAliveState && newNeedsAliveState:
		if current.policy.Policy != policy.Policy {
			for _, set := range uniqueAliveDialerSets(current.aliveDialerSets) {
				set.SetSelectionPolicy(policy.Policy)
			}
		}
		next := &dialerGroupSelectionState{
			policy:          policy,
			aliveDialerSets: current.aliveDialerSets,
		}
		g.selectionState.Store(next)

	case !currentNeedsAliveState && !newNeedsAliveState:
		g.selectionState.Store(&dialerGroupSelectionState{policy: policy})

	case !currentNeedsAliveState && newNeedsAliveState:
		next := g.buildSelectionState(policy, true)
		g.registerAliveDialerSets(next.aliveDialerSets)
		for _, d := range g.Dialers {
			d.ActivateCheck()
		}
		g.selectionState.Store(next)

	case currentNeedsAliveState && !newNeedsAliveState:
		oldSets := current.aliveDialerSets
		g.selectionState.Store(&dialerGroupSelectionState{policy: policy})
		g.unregisterAliveDialerSets(oldSets)
	}
}

func (g *DialerGroup) GetSelectionPolicy() (policy consts.DialerSelectionPolicy) {
	return g.currentSelectionState().policy.Policy
}

func (g *DialerGroup) MinCheckInterval() time.Duration {
	if len(g.Dialers) == 0 {
		return 30 * time.Second
	}
	min := g.Dialers[0].CheckInterval
	for _, d := range g.Dialers[1:] {
		if d.CheckInterval < min {
			min = d.CheckInterval
		}
	}
	if min < 2*time.Second {
		return 2 * time.Second
	}
	return min
}

func (d *DialerGroup) MustGetAliveDialerSet(typ *dialer.NetworkType) *dialer.AliveDialerSet {
	return d.currentSelectionState().aliveDialerSets[typ.Index()]
}

// CaptureReloadSelectionFallback captures one fallback candidate per network
// type so reload inheritance can avoid leaving a group with no selectable dialer.
func (g *DialerGroup) CaptureReloadSelectionFallback() ReloadSelectionFallback {
	var fallback ReloadSelectionFallback
	if g == nil {
		return fallback
	}
	for _, nt := range standardSelectionNetworkTypes() {
		d, _, _, err := g.SelectWithExclusionResult(nt, false, nil)
		if err == nil && d != nil {
			fallback[nt.Index()] = d
		}
	}
	return fallback
}

// EnsureReloadSelectionFloor keeps exactly one fallback candidate alive for
// network types whose inherited health state would otherwise be empty.
func (g *DialerGroup) EnsureReloadSelectionFloor(fallback ReloadSelectionFallback) {
	if g == nil {
		return
	}
	for _, nt := range standardSelectionNetworkTypes() {
		set := g.MustGetAliveDialerSet(nt)
		if set == nil || set.Len() > 0 {
			continue
		}
		candidate := fallback[nt.Index()]
		if candidate == nil && len(g.Dialers) > 0 {
			candidate = g.Dialers[0]
		}
		if candidate == nil {
			continue
		}
		candidate.MarkAliveForReloadFallback(nt)
		if g.log != nil && g.log.IsLevelEnabled(logrus.DebugLevel) {
			dialerName := ""
			if p := candidate.Property(); p != nil {
				dialerName = p.Name
			}
			g.log.WithFields(logrus.Fields{
				"dialer":  dialerName,
				"group":   g.Name,
				"network": nt.String(),
			}).Debugln("Reload health inheritance kept a selection fallback alive")
		}
	}
}

// tryDoRateLimitedAction checks if an action can be performed based on a rate limit.
// It uses atomic operations to ensure thread-safety with minimal overhead.
func (g *DialerGroup) tryDoRateLimitedAction(last *atomic.Int64, interval time.Duration) bool {
	now := time.Now().UnixNano()
	l := last.Load()
	if now-l < int64(interval) {
		return false
	}
	return last.CompareAndSwap(l, now)
}

// HandleNoAliveDialer is the unified entry point for handling dialer selection failures.
// IT MUST ONLY BE CALLED ON THE ERROR PATH to ensure zero overhead for successful requests.
// It automatically triggers a resuscitation probe and logs the failure, both subject to
// their respective (cached) rate limits.
func (g *DialerGroup) HandleNoAliveDialer(
	origNetworkType string,
	selectionNetworkType *dialer.NetworkType,
	src netip.AddrPort,
	dst netip.AddrPort,
	domain string,
	strictIpVersion bool,
) {
	// 1. Attempt resuscitation (rate-limited by min check interval)
	if g.tryDoRateLimitedAction(&g.resuscitateLastTime, g.cachedMinCheckInterval) {
		g.resuscitate(selectionNetworkType)
	}

	// 2. Log the failure (rate-limited by 5x check interval, min 10s)
	idx := selectionNetworkType.Index()
	logInterval := max(g.cachedMinCheckInterval*5, 10*time.Second)

	if g.tryDoRateLimitedAction(&g.noAliveLogLastTimes[idx], logInterval) {
		g.logNoAlive(origNetworkType, selectionNetworkType, src, dst, domain, strictIpVersion, logInterval)
	}
}

// Resuscitate triggers a targeted health check for all dialers in the group.
// It is rate-limited to once per group per MinCheckInterval to prevent worker pool starvation.
// Returns true if a resuscitation probe was actually signaled.
func (g *DialerGroup) Resuscitate(networkType *dialer.NetworkType) bool {
	if g.tryDoRateLimitedAction(&g.resuscitateLastTime, g.cachedMinCheckInterval) {
		g.resuscitate(networkType)
		return true
	}
	return false
}

func (g *DialerGroup) resuscitate(networkType *dialer.NetworkType) {
	for _, d := range g.Dialers {
		if networkType.L4Proto == consts.L4ProtoStr_UDP {
			// UDP admission may recover through DNS-UDP first and then shared TCP.
			// Probe both families so emergency recovery does not wait for the next
			// periodic full check when only the TCP fallback has come back.
			d.NotifyCheckDnsUdp()
			d.NotifyCheckTcp()
			continue
		}
		d.NotifyCheckTcp()
	}
}

func (g *DialerGroup) logNoAlive(
	origNetworkType string,
	selectionNetworkType *dialer.NetworkType,
	src netip.AddrPort,
	dst netip.AddrPort,
	domain string,
	strictIpVersion bool,
	interval time.Duration,
) {
	total := len(g.Dialers)
	alive := 0
	if a := g.MustGetAliveDialerSet(selectionNetworkType); a != nil {
		alive = a.Len()
	}

	g.log.WithFields(logrus.Fields{
		"outbound":               g.Name,
		"orig_network_type":      origNetworkType,
		"selection_network_type": selectionNetworkType.String(),
		"src":                    src.String(),
		"to":                     dst.String(),
		"sniffed":                domain,
		"interval":               interval.String(),
		"total":                  total,
		"alive":                  alive,
	}).Warn("no alive dialer for selection (rate-limited)")
}

// Select is a backward-compatible wrapper for SelectWithExclusion.
func (g *DialerGroup) Select(networkType *dialer.NetworkType, strictIpVersion bool) (d *dialer.Dialer, latency time.Duration, err error) {
	d, latency, _, err = g.SelectWithExclusionResult(networkType, strictIpVersion, nil)
	return d, latency, err
}

// SelectWithExclusion selects a dialer from group according to selectionPolicy.
// The 'excluded' parameter specifies a dialer to avoid during selection (for
// failover scenarios). Note that Fixed policy ignores 'excluded' because user
// configuration takes precedence over automatic exclusion.
// If 'strictIpVersion' is false and no alive dialer, it will fallback to another ipversion.
func (g *DialerGroup) SelectWithExclusion(networkType *dialer.NetworkType, strictIpVersion bool, excluded *dialer.Dialer) (d *dialer.Dialer, latency time.Duration, err error) {
	d, latency, _, err = g.SelectWithExclusionResult(networkType, strictIpVersion, excluded)
	return d, latency, err
}

// SelectWithExclusionResult returns the chosen dialer together with the health
// domain actually used to admit that dialer. For ordinary selections this is
// the requested network type; for data-UDP recovery it may be DNS-UDP or TCP.
func (g *DialerGroup) SelectWithExclusionResult(networkType *dialer.NetworkType, strictIpVersion bool, excluded *dialer.Dialer) (d *dialer.Dialer, latency time.Duration, selectedNetworkType *dialer.NetworkType, err error) {
	state := g.currentSelectionState()
	policy := state.policy
	d, latency, selectedNetworkType, err = g._select(networkType, state, policy, excluded)
	if !strictIpVersion && errors.Is(err, ErrNoAliveDialer) {
		// Fallback to another ipversion. Use local copy to avoid modifying the original networkType if it's passed by reference.
		nt := *networkType
		nt.IpVersion = (consts.IpVersion_X - networkType.IpVersion.ToIpVersionType()).ToIpVersionStr()
		return g._select(&nt, state, policy, excluded)
	}
	if err == nil {
		return d, latency, selectedNetworkType, nil
	}
	if errors.Is(err, ErrNoAliveDialer) && len(g.Dialers) == 1 {
		// There is only one dialer in this group. Just choose it instead of return error.
		if d, _, selectedNetworkType, err = g._select(networkType, state, DialerSelectionPolicy{
			Policy:     consts.DialerSelectionPolicy_Fixed,
			FixedIndex: 0,
		}, excluded); err != nil {
			return nil, 0, nil, err
		}
		return d, dialer.Timeout, selectedNetworkType, nil
	}
	return nil, latency, selectedNetworkType, err
}

func (g *DialerGroup) _select(networkType *dialer.NetworkType, state *dialerGroupSelectionState, policy DialerSelectionPolicy, excluded *dialer.Dialer) (d *dialer.Dialer, latency time.Duration, selectedNetworkType *dialer.NetworkType, err error) {
	if len(g.Dialers) == 0 {
		return nil, 0, nil, fmt.Errorf("no dialer in this group")
	}
	switch policy.Policy {
	case consts.DialerSelectionPolicy_Random:
		networkTypes, count := g.selectionNetworkTypes(networkType, policy)
		for i := range count {
			a := state.aliveDialerSets[networkTypes[i].Index()]
			d := a.GetRandExcluded(excluded)
			if d != nil {
				selected := preferAlternateSelectionNetworkType(d, &networkTypes[i])
				return d, 0, selected, nil
			}
		}
		return nil, time.Hour, nil, ErrNoAliveDialer

	case consts.DialerSelectionPolicy_Failover:
		// Failover policy: select from the controller's atomic snapshot.
		// The hot path performs one atomic load and never traverses candidates
		// or acquires the controller mutex. Rotation/recovery is off the hot path.
		if g.failoverController == nil || g.failoverCfg == nil {
			return nil, 0, nil, fmt.Errorf("failover controller not initialized")
		}
		d, usingFallback := g.failoverController.ActiveDialer()
		if d == nil {
			return nil, 0, nil, fmt.Errorf("failover active dialer is nil")
		}
		if excluded != nil && d == excluded {
			if !usingFallback {
				// Active primary is excluded — fall through to the fixed
				// fallback. Rotation to another Primary is a recovery concern
				// and must not happen on the hot path.
				fallback := g.Dialers[g.failoverCfg.FallbackIdx]
				if fallback != nil && fallback != excluded {
					return fallback, 0, preferAlternateSelectionNetworkType(fallback, networkType), nil
				}
			}
			// Fallback is excluded (or fallback is the active role and excluded)
			// — return an error rather than an unconfirmed primary.
			return nil, 0, nil, ErrNoAliveDialer
		}
		return d, 0, preferAlternateSelectionNetworkType(d, networkType), nil

	case consts.DialerSelectionPolicy_Fixed:
		// Fixed policy represents explicit user intent to use a specific dialer.
		// It ignores the 'excluded' parameter because user configuration takes
		// precedence over automatic exclusion. Even if the dialer is marked as
		// excluded, Fixed policy returns it as configured.
		if policy.FixedIndex < 0 || policy.FixedIndex >= len(g.Dialers) {
			return nil, 0, nil, fmt.Errorf("selected dialer index is out of range")
		}
		selected := preferAlternateSelectionNetworkType(g.Dialers[policy.FixedIndex], networkType)
		return g.Dialers[policy.FixedIndex], 0, selected, nil

	case consts.DialerSelectionPolicy_MinLastLatency,
		consts.DialerSelectionPolicy_MinAverage10Latencies,
		consts.DialerSelectionPolicy_MinMovingAverageLatencies:
		networkTypes, count := g.selectionNetworkTypes(networkType, policy)
		for i := range count {
			a := state.aliveDialerSets[networkTypes[i].Index()]
			d, latency := a.GetMinLatency(excluded)
			if d != nil {
				selected := preferAlternateSelectionNetworkType(d, &networkTypes[i])
				return d, latency, selected, nil
			}
		}
		return nil, time.Hour, nil, ErrNoAliveDialer

	default:
		return nil, 0, nil, fmt.Errorf("unsupported DialerSelectionPolicy: %v", policy)
	}
}

func (g *DialerGroup) selectionNetworkTypes(networkType *dialer.NetworkType, policy DialerSelectionPolicy) (networkTypes [3]dialer.NetworkType, count int) {
	networkTypes[0] = *networkType
	count = 1

	if policy.Policy == consts.DialerSelectionPolicy_Fixed ||
		networkType.L4Proto != consts.L4ProtoStr_UDP ||
		networkType.EffectiveUdpHealthDomain() != dialer.UdpHealthDomainData {
		return networkTypes, count
	}

	// If data-plane UDP has no alive dialer, retry selection against DNS UDP
	// first, then shared TCP health for the same IP family. A successful real
	// UDP flow will revive the data-UDP domain via ReportAvailableTraffic.
	networkTypes[count] = dialer.NetworkType{
		L4Proto:         consts.L4ProtoStr_UDP,
		IpVersion:       networkType.IpVersion,
		IsDns:           true,
		UdpHealthDomain: dialer.UdpHealthDomainDns,
	}
	count++
	networkTypes[count] = dialer.NetworkType{
		L4Proto:   consts.L4ProtoStr_TCP,
		IpVersion: networkType.IpVersion,
	}
	count++
	return networkTypes, count
}

func (g *DialerGroup) currentSelectionState() *dialerGroupSelectionState {
	state := g.selectionState.Load()
	if state == nil {
		return &dialerGroupSelectionState{}
	}
	return state
}

func (g *DialerGroup) buildSelectionState(policy DialerSelectionPolicy, setAlive bool) *dialerGroupSelectionState {
	state := &dialerGroupSelectionState{
		policy: policy,
	}
	if !policyNeedsAliveState(policy.Policy) {
		return state
	}

	specs := standardSelectionNetworkTypes()
	keys := dialer.StandardHealthKeys()

	for i, nt := range specs {
		networkType := *nt
		set := dialer.NewAliveDialerSet(
			g.log, g.Name, &networkType, g.checkTolerance, policy.Policy,
			g.Dialers, g.dialersAnnotations,
			func(networkType *dialer.NetworkType) func(alive bool) {
				return func(alive bool) { g.aliveChangeCallback(alive, networkType, false) }
			}(&networkType),
			false,
		)
		if setAlive {
			for _, d := range g.Dialers {
				set.NotifyLatencyChange(d, d.MustGetAlive(&networkType))
			}
		}
		state.aliveDialerSets[keys[i].CollectionIndex()] = set
		if networkType.L4Proto == consts.L4ProtoStr_TCP {
			if networkType.IpVersion == consts.IpVersionStr_4 {
				state.aliveDialerSets[dialer.IdxDnsTcp4] = set
			} else {
				state.aliveDialerSets[dialer.IdxDnsTcp6] = set
			}
		}
	}
	return state
}

func (g *DialerGroup) registerAliveDialerSets(aliveDialerSets [8]*dialer.AliveDialerSet) {
	for _, d := range g.Dialers {
		for _, a := range aliveDialerSets {
			d.RegisterAliveDialerSet(a)
		}
	}
}

func (g *DialerGroup) unregisterAliveDialerSets(aliveDialerSets [8]*dialer.AliveDialerSet) {
	for _, d := range g.Dialers {
		for _, a := range aliveDialerSets {
			d.UnregisterAliveDialerSet(a)
		}
	}
}

func policyNeedsAliveState(policy consts.DialerSelectionPolicy) bool {
	switch policy {
	case consts.DialerSelectionPolicy_Random,
		consts.DialerSelectionPolicy_MinLastLatency,
		consts.DialerSelectionPolicy_MinAverage10Latencies,
		consts.DialerSelectionPolicy_MinMovingAverageLatencies:
		return true
	case consts.DialerSelectionPolicy_Fixed,
		consts.DialerSelectionPolicy_Failover:
		return false
	default:
		panic(fmt.Sprintf("unexpected dialer selection policy: %v", policy))
	}
}

func uniqueAliveDialerSets(aliveDialerSets [8]*dialer.AliveDialerSet) []*dialer.AliveDialerSet {
	unique := make(map[*dialer.AliveDialerSet]struct{}, len(aliveDialerSets))
	var sets []*dialer.AliveDialerSet
	for _, set := range aliveDialerSets {
		if set == nil {
			continue
		}
		if _, ok := unique[set]; ok {
			continue
		}
		unique[set] = struct{}{}
		sets = append(sets, set)
	}
	return sets
}

func standardSelectionNetworkTypes() [6]*dialer.NetworkType {
	keys := dialer.StandardHealthKeys()
	var networkTypes [6]*dialer.NetworkType
	for i, key := range keys {
		networkTypes[i] = key.NetworkType()
	}
	return networkTypes
}

func preferAlternateSelectionNetworkType(d *dialer.Dialer, networkType *dialer.NetworkType) *dialer.NetworkType {
	if d == nil || networkType == nil {
		return networkType
	}
	if d.MustGetAlive(networkType) {
		return networkType
	}
	altType := alternateNetworkType(networkType)
	if altType == nil {
		return networkType
	}
	if d.MustGetAlive(altType) {
		return altType
	}
	return networkType
}

func alternateNetworkType(networkType *dialer.NetworkType) *dialer.NetworkType {
	if networkType == nil {
		return nil
	}
	switch networkType.IpVersion {
	case consts.IpVersionStr_4:
		alt := *networkType
		alt.IpVersion = consts.IpVersionStr_6
		return &alt
	case consts.IpVersionStr_6:
		alt := *networkType
		alt.IpVersion = consts.IpVersionStr_4
		return &alt
	default:
		return nil
	}
}
