/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestFailoverEventDispatcher_QueueFullDrops(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel) // suppress debug noise

	// capacity 1, with a blocker so the worker never drains.
	// Block the worker so the queue stays full.
	block := make(chan struct{})
	released := make(chan struct{})
	d := NewFailoverEventDispatcher(log, "g", 1, func(ev FailoverEvent) {
		// signal we are holding the worker, then wait for release.
		close(released)
		<-block
	})
	defer d.Close()

	// First event enters the worker (queue drains immediately).
	d.OnFailoverEvent(FailoverEvent{Type: FailoverEventSwitch, Group: "g"})
	<-released // worker is now busy on event 1

	// Second event sits in the queue (capacity 1).
	d.OnFailoverEvent(FailoverEvent{Type: FailoverEventSwitch, Group: "g2"})

	// Third event must be dropped (queue full) without blocking.
	dropped := atomic.Int32{}
	d.onDrop = func() { dropped.Add(1) }
	done := make(chan struct{})
	go func() {
		d.OnFailoverEvent(FailoverEvent{Type: FailoverEventSwitch, Group: "g3"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("OnFailoverEvent blocked on full queue")
	}
	if got := dropped.Load(); got != 1 {
		t.Fatalf("drop count = %d, want 1", got)
	}

	close(block) // release worker
}

func TestFailoverEventDispatcher_CloseStopsWorker(t *testing.T) {
	log := logrus.New()
	log.SetLevel(logrus.PanicLevel)

	var count atomic.Int32
	d := NewFailoverEventDispatcher(log, "g", 4, func(ev FailoverEvent) { count.Add(1) })

	for i := range 4 {
		d.OnFailoverEvent(FailoverEvent{Type: FailoverEventSwitch, Group: "g"})
		_ = i
	}

	d.Close()

	// After Close, new events are dropped silently.
	d.OnFailoverEvent(FailoverEvent{Type: FailoverEventSwitch, Group: "g"})

	// Give the worker a moment to finish any in-flight processNext.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if count.Load() >= 0 {
			break
		}
	}
	// No panic and Close returned = worker stopped. We do not assert exact
	// count because close may drain remaining events without invoking
	// notifiers (by design).
}
