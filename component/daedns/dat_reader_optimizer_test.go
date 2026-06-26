/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package daedns

import (
	"testing"

	"github.com/daeuniverse/dae/component/routing"
	"github.com/sirupsen/logrus"
)

func TestDatReaderOptimizerForRouterUsesProvidedInstance(t *testing.T) {
	shared := &routing.DatReaderOptimizer{}

	got := datReaderOptimizerForRouter(logrus.New(), nil, &NewOption{
		DatReaderOptimizer: shared,
	})

	if got != shared {
		t.Fatal("expected router to use the caller-provided DatReaderOptimizer")
	}
}

func TestDatReaderOptimizerForRouterCreatesDefaultInstance(t *testing.T) {
	got := datReaderOptimizerForRouter(logrus.New(), nil, nil)

	if got == nil {
		t.Fatal("expected router to create a default DatReaderOptimizer")
	}
}
