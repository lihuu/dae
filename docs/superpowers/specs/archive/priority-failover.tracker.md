# Priority Failover Policy — Implementation Tracker

**Spec:** `docs/superpowers/specs/2026-06-07-priority-failover-policy-design.md`
**Goal:** Add an event-driven `failover` dialer selection policy for a single primary + single fallback node.

## Checklist

### Config & Parsing
- [x] Add `priority` filter annotation (integer) to `dialer.Annotation`
- [x] Add recovery policy fields to `config.Group` (`recovery_probe_initial`, `recovery_probe_max`, `recovery_successes`, `recovery_stable_time`)
- [x] Add `failover` value to `DialerSelectionPolicy`
- [x] Implement config validation (exactly 2 dialers, valid priorities 0/1, no duplicates, valid durations, etc.)

### State Machine
- [x] Implement `primary_active` state — select primary, no timer-driven network probes; event worker may remain active
- [x] Implement `fallback_active` state — select fallback, start recovery probe schedule
- [x] Implement `recovering` state — select fallback, probe primary, count consecutive successes + stable time
- [x] Implement `fallback_unavailable` — fallback dial error returns error, no infinite loop

### DialerGroup Changes
- [x] Primary/fallback role mapping from priority annotations
- [x] Atomic active-role selection (immutable/atomic snapshot, no long-lived mutex on hot path)
- [x] Recovery state machine lifecycle (timer, probe, backoff)

### Health & Probing
- [x] Wire primary TCP health transitions (`alive -> not alive`) into failover controller
- [x] Implement cancellable one-shot TCP probe (reuse existing health-check, no recurring periodic network probes)
- [x] Exponential backoff: `initial, initial*2, initial*4, ... max`
- [x] After first success: reset backoff, probe at `initial` interval, require `recovery_successes` + `recovery_stable_time`

### Selection & Retry
- [x] Ordinary selection respects current state (`primary_active`→primary, else→fallback)
- [x] Respect existing `excluded` dialer argument
- [x] Concurrent primary failures → one transition + one recovery schedule
- [x] UDP does not trigger failover; new UDP sessions follow group role
- [x] Fallback failure returns dial error (no promotion to third level)

### Reload & Lifecycle
- [x] Warm reload: inherit state when primary/fallback identities match
- [x] Warm reload: reinitialize to `primary_active` when identities change
- [x] Close: cancel recovery timer and active probe context
- [x] Stale callbacks from old generation must not mutate new group

### Logging & Observability
- [x] Structured log: `failover_switch` (group, from, to, trigger, primary, fallback)
- [x] Structured log: `failback_start` (group, primary, successes)
- [x] Structured log: `failback_complete` (group, from, to, successes, stable_for)
- [x] Recovery probe failures: debug-level or rate-limited

### Compatibility
- [x] Existing policies (`fixed`, `random`, `min`, `min_avg10`, `min_moving_avg`) unchanged
- [x] Existing configs without `policy: failover` parse and behave as before
- [x] Routing `fallback:` unrelated and unchanged
- [x] DNS routing fallback unrelated and unchanged
- [x] Existing eBPF routing contracts and outbound IDs unchanged

## Verification Log

### 2026-06-07 — Config & Parsing
- **Task**: Add `priority` annotation, failover constant, recovery config fields, validation
- **Command**: `GOOS=linux go vet ./component/outbound/ ./config/ ./common/consts/`
- **Result**: Clean compilation, no errors

### 2026-06-07 — State Machine & DialerGroup
- **Task**: Implement failover controller with state machine, integrate into DialerGroup
- **Command**: `GOOS=linux go test -c ./component/outbound/ -o /dev/null`
- **Result**: Tests compile cleanly (cannot run on macOS due to Linux-only syscalls)

### 2026-06-07 — Reload & Lifecycle
- **Task**: Add failover state inheritance to InheritDialerHealthFrom
- **Command**: `GOOS=linux go vet ./control/`
- **Result**: Clean compilation (pre-existing BPF errors on macOS only)

### 2026-06-07 — Tests
- **Task**: Write unit tests for failover controller and validation
- **Files**: `component/outbound/failover_controller_test.go`
- **Result**: 12 test functions covering initial state, failover trigger, UDP ignore, recovery ignore, close, stale callbacks, snapshot capture/restore, validation
