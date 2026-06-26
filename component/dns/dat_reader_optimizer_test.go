/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package dns

import (
	"testing"

	"github.com/daeuniverse/dae/component/routing"
	"github.com/sirupsen/logrus"
)

func TestDatReaderOptimizerForDNSUsesProvidedInstance(t *testing.T) {
	shared := &routing.DatReaderOptimizer{}

	got := datReaderOptimizerForDNS(&NewOption{
		Logger:             logrus.New(),
		DatReaderOptimizer: shared,
	})

	if got != shared {
		t.Fatal("expected DNS to use the caller-provided DatReaderOptimizer")
	}
}

func TestDatReaderOptimizerForDNSCreatesDefaultInstance(t *testing.T) {
	got := datReaderOptimizerForDNS(&NewOption{Logger: logrus.New()})

	if got == nil {
		t.Fatal("expected DNS to create a default DatReaderOptimizer")
	}
}
