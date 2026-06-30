/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"testing"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/sirupsen/logrus"
)

func TestOutboundToIdResolvesReject(t *testing.T) {
	outboundName2Id := map[string]uint8{
		consts.OutboundDirect.String(): uint8(consts.OutboundDirect),
		consts.OutboundBlock.String():  uint8(consts.OutboundBlock),
		consts.OutboundReject.String(): uint8(consts.OutboundReject),
		"mygroup":                      uint8(consts.OutboundUserDefinedMin),
	}
	b, err := NewRoutingMatcherBuilder(logrus.New(), nil, outboundName2Id, nil, "direct")
	if err != nil {
		t.Fatalf("NewRoutingMatcherBuilder() error = %v", err)
	}
	got, err := b.outboundToId("reject")
	if err != nil {
		t.Fatalf("outboundToId(reject) err = %v", err)
	}
	if got != uint8(consts.OutboundReject) {
		t.Fatalf("outboundToId(reject) = %d, want %d", got, uint8(consts.OutboundReject))
	}
}

func TestRejectIsBelowUserDefinedMin(t *testing.T) {
	if uint8(consts.OutboundReject) >= uint8(consts.OutboundUserDefinedMin) {
		t.Fatalf("reject index %d must be below user-defined min %d",
			consts.OutboundReject, consts.OutboundUserDefinedMin)
	}
}

func TestRejectStringReturnsReject(t *testing.T) {
	if s := consts.OutboundReject.String(); s != "reject" {
		t.Fatalf("OutboundReject.String() = %q, want %q", s, "reject")
	}
}
