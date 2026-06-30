// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>

package control

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"

	"github.com/cilium/ebpf/ringbuf"
)

// Go mirror of enum dae_event_type in control/kern/tproxy.c. Keep in sync with
// the C enum; the values are the canonical contract between kernel and user.
const (
	daeEventTypeBlocked         uint32 = 0
	daeEventTypeRejected        uint32 = 1
	daeEventTypeUDPConnOverflow uint32 = 2
	daeEventTypeTCPConnOverflow uint32 = 3
)

func eventTypeLabel(t uint32) string {
	switch t {
	case daeEventTypeBlocked:
		return "block"
	case daeEventTypeRejected:
		return "reject"
	case daeEventTypeUDPConnOverflow:
		return "udp_conn_overflow"
	case daeEventTypeTCPConnOverflow:
		return "tcp_conn_overflow"
	default:
		return fmt.Sprintf("unknown(%d)", t)
	}
}

// ipv4FromU32 renders only the first u32 (IPv4-mapped) of an event address.
// Correct for v1 IPv4-only reject; an IPv6 event would render truncated.
func ipv4FromU32(w uint32) string {
	b := make(net.IP, 4)
	binary.BigEndian.PutUint32(b, w)
	return b.String()
}

// renderDaeEvent formats a raw ringbuf record into a single log line. The label
// distinguishes reject from block so operators can tell the two apart. IPv4
// endpoints are rendered from the first u32 of the IPv6-mapped address.
func renderDaeEvent(raw []byte) string {
	var e bpfDaeEvent
	if err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, &e); err != nil {
		return fmt.Sprintf("dae_event: decode err %v", err)
	}
	l4 := "tcp"
	if e.L4proto != 1 {
		l4 = fmt.Sprintf("l4proto=%d", e.L4proto)
	}
	return fmt.Sprintf("dae_event type=%s outbound=0x%x %s sip=%s dip=%s sport=%d dport=%d",
		eventTypeLabel(e.Type), e.Outbound, l4,
		ipv4FromU32(e.Sip[0]), ipv4FromU32(e.Dip[0]),
		e.Sport, e.Dport)
}

// consumeDaeEvents reads the dae_event ringbuf until ctx is done, logging each
// event. This is the userspace rendering path for DAE_EVENT_BLOCKED and
// DAE_EVENT_REJECTED.
func (c *ControlPlane) consumeDaeEvents() {
	bpf := c.currentBpf()
	if bpf == nil || bpf.EventRingbuf == nil {
		c.log.Warn("dae_event ringbuf unavailable; event rendering disabled")
		return
	}
	rd, err := ringbuf.NewReader(bpf.EventRingbuf)
	if err != nil {
		c.log.Warnf("dae_event ringbuf reader creation failed: %v", err)
		return
	}
	defer func() { _ = rd.Close() }()
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		rec, err := rd.Read()
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			c.log.Warnf("dae_event ringbuf read err: %v", err)
			continue
		}
		c.log.Info(renderDaeEvent(rec.RawSample))
	}
}
