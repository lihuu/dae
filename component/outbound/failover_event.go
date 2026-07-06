/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// FailoverEventType identifies a failover state transition.
type FailoverEventType string

const (
	// FailoverEventSwitch is emitted when the active dialer switches from
	// primary to fallback because the primary became TCP-unavailable.
	FailoverEventSwitch FailoverEventType = "failover_switch"
	// FailoverEventFailbackComplete is emitted when the controller has
	// confirmed primary stability and switched the active dialer back to
	// primary.
	FailoverEventFailbackComplete FailoverEventType = "failback_complete"
)

// FailoverEvent describes a single failover state transition.
type FailoverEvent struct {
	Type         FailoverEventType
	Group        string
	From         string
	To           string
	Primary      string
	Fallback     string
	Trigger      string
	TransitionAt time.Time
	Successes    int
	StableFor    time.Duration
}

// FailoverEventCallback receives failover transition events. Implementations
// must be non-blocking: the controller invokes OnFailoverEvent while holding
// its mutex, so the callback must only enqueue and return immediately.
type FailoverEventCallback interface {
	OnFailoverEvent(event FailoverEvent)
}

// FailoverEventDispatcher is an asynchronous FailoverEventCallback. It accepts
// events through a bounded non-blocking queue and invokes a notifier from a
// worker goroutine. A full queue drops the event.
//
// The zero value is not usable; construct with NewFailoverEventDispatcher.
type FailoverEventDispatcher struct {
	log   *logrus.Logger
	group string

	queue    chan FailoverEvent
	stop     chan struct{}
	done     chan struct{}
	closeOnce sync.Once

	// processNext is the notifier invocation. Defaults to a no-op; set by
	// callers (e.g. the control plane) to wire in a BarkNotifier. Tests
	// override it to observe dispatch behavior.
	processNext func(ev FailoverEvent)
	// onDrop is invoked when an event is dropped due to a full queue.
	// Defaults to a debug log; tests override it to count drops.
	onDrop func()
}

// NewFailoverEventDispatcher creates a dispatcher with the given queue
// capacity. The dispatcher starts one worker goroutine. Call Close to stop the
// worker and release resources.
func NewFailoverEventDispatcher(log *logrus.Logger, group string, capacity int) *FailoverEventDispatcher {
	if capacity < 1 {
		capacity = 1
	}
	d := &FailoverEventDispatcher{
		log:         log,
		group:       group,
		queue:       make(chan FailoverEvent, capacity),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		processNext: func(ev FailoverEvent) {},
		onDrop:      func() {},
	}
	d.onDrop = func() {
		if log.IsLevelEnabled(logrus.DebugLevel) {
			log.WithFields(logrus.Fields{
				"provider": "dispatcher",
				"group":    group,
				"reason":   "queue_full",
			}).Debug("failover notify event dropped")
		}
	}
	go d.worker()
	return d
}

// OnFailoverEvent enqueues an event. It never blocks: if the queue is full the
// event is dropped and onDrop is called. After Close, events are dropped
// silently.
func (d *FailoverEventDispatcher) OnFailoverEvent(event FailoverEvent) {
	select {
	case <-d.stop:
		// Closed: drop silently.
		return
	default:
	}
	select {
	case d.queue <- event:
	default:
		d.onDrop()
	}
}

// Close stops the worker goroutine. In-flight queued events may be dropped.
// It is safe to call concurrently and more than once.
func (d *FailoverEventDispatcher) Close() {
	d.closeOnce.Do(func() {
		close(d.stop)
	})
	// Drain-stop: close the queue only after stop is closed so OnFailoverEvent
	// sees the stop signal first. Then wait for the worker to finish.
	// We cannot close `queue` here directly because OnFailoverEvent may still
	// race a send; the worker drains until stop is observed.
	<-d.done
}

func (d *FailoverEventDispatcher) worker() {
	defer close(d.done)
	for {
		select {
		case <-d.stop:
			// Drain remaining events without invoking notifiers (close path).
			for {
				select {
				case <-d.queue:
				default:
					return
				}
			}
		case ev := <-d.queue:
			// Re-check stop: an event may have been queued just before Close.
			select {
			case <-d.stop:
				return
			default:
			}
			d.processNext(ev)
		}
	}
}
