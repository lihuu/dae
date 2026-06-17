/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"net/netip"

	dnsmessage "github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

const fakeipEventVersion = 1

func fakeipEventBase(event string) logrus.Fields {
	return logrus.Fields{
		"component":     "fakeip",
		"event":         event,
		"event_version": fakeipEventVersion,
	}
}

// --- fakeip_dns_answer ---

func (c *DnsController) logFakeIPDNSAnswerOK(domain string, fakeip netip.Addr, ttl int, allocated bool) {
	if c.log == nil || !c.log.IsLevelEnabled(logrus.DebugLevel) {
		return
	}
	fields := fakeipEventBase("fakeip_dns_answer")
	fields["domain"] = domain
	fields["qtype"] = "A"
	fields["result"] = "ok"
	fields["fakeip"] = fakeip.String()
	fields["ttl"] = ttl
	fields["allocated"] = allocated
	c.log.WithFields(fields).Debug("fakeip_event")
}

func (c *DnsController) logFakeIPDNSAnswerNODATA(domain string, qtype uint16) {
	if c.log == nil || !c.log.IsLevelEnabled(logrus.DebugLevel) {
		return
	}
	fields := fakeipEventBase("fakeip_dns_answer")
	fields["domain"] = domain
	fields["qtype"] = dnsmessage.Type(qtype).String()
	fields["result"] = "ok"
	fields["answer"] = "nodata"
	c.log.WithFields(fields).Debug("fakeip_event")
}

func (c *DnsController) logFakeIPDNSAnswerFailed(domain string, qtype uint16, errorClass string, err error) {
	if c.log == nil || !c.log.IsLevelEnabled(logrus.WarnLevel) {
		return
	}
	fields := fakeipEventBase("fakeip_dns_answer")
	fields["domain"] = domain
	fields["qtype"] = dnsmessage.Type(qtype).String()
	fields["result"] = "failed"
	fields["error_class"] = errorClass
	if err != nil {
		fields["error"] = err.Error()
	}
	c.log.WithFields(fields).Warn("fakeip_event")
}

// --- fakeip_flow ---

func (c *ControlPlane) logFakeIPFlow(domain string, fakeip netip.Addr, client netip.AddrPort, network string, port uint16, outbound string, policy string, dialTarget string) {
	if c.log == nil || !c.log.IsLevelEnabled(logrus.InfoLevel) {
		return
	}
	fields := fakeipEventBase("fakeip_flow")
	fields["domain"] = domain
	fields["fakeip"] = fakeip.String()
	fields["client"] = client.Addr().String()
	fields["network"] = network
	fields["port"] = port
	fields["outbound"] = outbound
	fields["result"] = "ok"
	if policy != "" {
		fields["policy"] = policy
	}
	if dialTarget != "" {
		fields["dial_target"] = dialTarget
	}
	c.log.WithFields(fields).Info("fakeip_event")
}

// --- fakeip_unknown ---

func (c *ControlPlane) logFakeIPUnknown(fakeip netip.Addr, client netip.AddrPort, network string, port uint16) {
	if c.log == nil || !c.log.IsLevelEnabled(logrus.WarnLevel) {
		return
	}
	fields := fakeipEventBase("fakeip_unknown")
	fields["fakeip"] = fakeip.String()
	fields["client"] = client.Addr().String()
	fields["network"] = network
	fields["port"] = port
	fields["result"] = "rejected"
	fields["error_class"] = "fakeip_unknown_mapping"
	c.log.WithFields(fields).Warn("fakeip_event")
}

// --- fakeip_direct_resolve ---

func (c *ControlPlane) logFakeIPDirectResolveOK(domain string, fakeip netip.Addr, upstream string, network string, port uint16, resolvedIP netip.Addr) {
	if c.log == nil || !c.log.IsLevelEnabled(logrus.InfoLevel) {
		return
	}
	fields := fakeipEventBase("fakeip_direct_resolve")
	fields["domain"] = domain
	fields["fakeip"] = fakeip.String()
	fields["upstream"] = upstream
	fields["network"] = network
	fields["port"] = port
	fields["result"] = "ok"
	fields["resolved_ip"] = resolvedIP.String()
	c.log.WithFields(fields).Info("fakeip_event")
}

func (c *ControlPlane) logFakeIPDirectResolveFailed(domain string, fakeip netip.Addr, upstream string, network string, port uint16, err error) {
	if c.log == nil || !c.log.IsLevelEnabled(logrus.WarnLevel) {
		return
	}
	fields := fakeipEventBase("fakeip_direct_resolve")
	fields["domain"] = domain
	fields["fakeip"] = fakeip.String()
	fields["upstream"] = upstream
	fields["network"] = network
	fields["port"] = port
	fields["result"] = "failed"
	fields["error_class"] = "direct_resolve_failed"
	if err != nil {
		fields["error"] = err.Error()
	}
	c.log.WithFields(fields).Warn("fakeip_event")
}

// --- fakeip_dial_error ---

func (c *ControlPlane) logFakeIPDialError(domain string, fakeip netip.Addr, client netip.AddrPort, network string, port uint16, outbound string, dialTarget string, errorClass string, err error) {
	if c.log == nil || !c.log.IsLevelEnabled(logrus.WarnLevel) {
		return
	}
	fields := fakeipEventBase("fakeip_dial_error")
	fields["domain"] = domain
	fields["fakeip"] = fakeip.String()
	fields["client"] = client.Addr().String()
	fields["network"] = network
	fields["port"] = port
	fields["outbound"] = outbound
	fields["dial_target"] = dialTarget
	fields["result"] = "failed"
	fields["error_class"] = errorClass
	if err != nil {
		fields["error"] = err.Error()
	}
	c.log.WithFields(fields).Warn("fakeip_event")
}

// --- fakeip_reload ---

func (c *ControlPlane) logFakeIPReloadOK(replayed int, store string, cidr string) {
	if c.log == nil || !c.log.IsLevelEnabled(logrus.InfoLevel) {
		return
	}
	fields := fakeipEventBase("fakeip_reload")
	fields["result"] = "ok"
	fields["replayed"] = replayed
	fields["store"] = store
	fields["cidr"] = cidr
	c.log.WithFields(fields).Info("fakeip_event")
}

func (c *ControlPlane) logFakeIPReloadFailed(errorClass string, err error) {
	if c.log == nil || !c.log.IsLevelEnabled(logrus.WarnLevel) {
		return
	}
	fields := fakeipEventBase("fakeip_reload")
	fields["result"] = "failed"
	fields["error_class"] = errorClass
	if err != nil {
		fields["error"] = err.Error()
	}
	c.log.WithFields(fields).Warn("fakeip_event")
}
