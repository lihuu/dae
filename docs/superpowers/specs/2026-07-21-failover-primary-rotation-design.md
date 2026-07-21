# Failover Current-Primary Rotation Design

<!-- IMPLEMENTATION-SPEC-BEGIN -->

# Goal

Extend the existing `policy: failover` implementation with an opt-in recovery
compensation mechanism for deployments where the configured primary node can
remain offline for a long time after failover.

The policy continues to have exactly two active roles:

- one current primary that serves new traffic during normal operation; and
- one fixed fallback that serves new traffic while the current primary is
  unavailable.

When the current primary exhausts a configured number of failed recovery
probes, the controller may probe an ordered list of standby primary candidates.
The first candidate that satisfies the existing stable-recovery requirements
becomes the single current primary. This is rotation of the primary identity,
not load balancing and not a multi-primary group.

For the production configuration that motivated this design:

```text
ordered primary candidates = A, B, C
initial current primary     = A
fixed fallback              = xray_local
rotation attempt threshold  = 5 failed recovery probes
```

The feature must preserve the existing failover hot path, fixed fallback role,
stable recovery confirmation, in-memory warm-reload inheritance, and
notification behavior.

# Non-Goals

This design does not add:

- concurrent or parallel probing of primary candidates;
- load balancing, latency comparison, or per-connection selection among A, B,
  and C;
- automatic preemption by a higher-priority candidate after another candidate
  has become the current primary;
- rotation of the fallback role;
- persistent primary identity, probe cursor, counters, or timers across process
  restarts;
- configuration-file rewriting when the primary changes;
- migration or termination of existing TCP connections or UDP endpoints;
- UDP-triggered failover;
- nested outbound groups;
- an external health watcher, systemd timer, or reload-driven rotation loop; or
- extra Bark notifications for routine recovery-target movement.

# Architecture

The existing `FailoverController` remains the sole authority for failover and
stable recovery. Its fixed `primary` reference becomes an ordered collection
plus an in-memory current-primary identity:

```text
orderedPrimaryCandidates = [A, B, C]
currentPrimary            = A
fallback                  = xray_local
```

At any moment, ordinary selection has exactly two outcomes:

```text
primary_active                    -> currentPrimary
fallback_active or recovering     -> fallback
```

The recovery controller maintains one `recoveryTarget`. Before rotation is
activated, that target is the failed current primary. After rotation is
activated, it advances through the ordered primary candidates one at a time.

The selection hot path must continue to read an immutable atomic snapshot. It
must not traverse candidates, acquire the controller mutex, or perform health
work. Candidate traversal and network probes remain off the traffic hot path.

Every primary candidate may publish TCP health transitions to the controller,
but a transition affects failover only when it belongs to `currentPrimary`.
Membership as a standby candidate must not independently enable ordinary
timer-driven periodic probes. Recovery probes are created only by the existing
failover recovery schedule, with at most one pending timer and one in-flight
probe for the group.

# Detailed Design

## Configuration

The feature is configured locally in a failover group:

```dae
group {
    proxy_failover {
        filter: name(A) [priority: 0]
        filter: name(xray_local) [priority: 1]
        filter: name(B) [priority: 2]
        filter: name(C) [priority: 3]

        policy: failover
        primary_rotation_attempts: 5

        recovery_probe_initial: 15s
        recovery_probe_max: 5m
        recovery_successes: 3
        recovery_stable_time: 30s
    }
}
```

Priority semantics for `policy: failover` are:

- `priority: 0` is the initial primary selected on every fresh process start.
- `priority: 1` is the fixed fallback and never participates in primary
  rotation.
- Each unique `priority` greater than or equal to `2` is a standby primary
  candidate.
- Standby candidates are sorted by numeric priority. Filter evaluation or
  subscription iteration order must not determine rotation order.
- Gaps are valid. For example, priorities `2`, `4`, and `8` are probed in that
  order.

`primary_rotation_attempts` is an integer with default value `0`:

- `0` disables primary rotation and preserves the legacy two-dialer failover
  contract.
- A positive value enables primary rotation and is the number of failed
  recovery probes the failed current primary receives before the recovery
  target advances to the next candidate.

Validation must reject the configuration when:

- rotation is disabled and the group does not resolve to exactly one
  `priority: 0` dialer and one `priority: 1` dialer;
- rotation is enabled without exactly one initial primary, exactly one fixed
  fallback, and at least one resolved standby primary candidate;
- a selected dialer has no priority annotation;
- a priority is duplicated, including when one broad filter applies the same
  annotation to multiple dialers;
- two roles resolve to the same underlying dialer;
- `primary_rotation_attempts` is negative; or
- `primary_rotation_attempts` is used with a policy other than `failover`.

Existing recovery-duration and recovery-success validation remains unchanged.

## Controller State

In addition to the existing state, success counter, stability timestamp,
backoff, timer, cancellation, and generation fields, the controller records:

```text
orderedPrimaryCandidates
currentPrimary
recoveryTarget
rotationActive
failedRecoveryProbes
```

Candidate identities are represented by stable dialer names when matching
state across a warm reload. Temporary slice indexes may be used internally in
one controller generation, but must not be used to match identities between
generations.

`failedRecoveryProbes` is the monotonically increasing total failed
recovery-probe count during the current failover episode. Before rotation is
active, every failure is necessarily against the current primary, so this same
counter is compared with `primary_rotation_attempts`. After rotation begins it
continues increasing across B, C, and circular wraps and supplies the
`failed_attempts` log field. A successful probe does not reset the count. It
resets only when a candidate completes stable recovery and becomes the active
current primary, or when a new controller starts without an inherited
snapshot.

## Failure Transition

Only a confirmed TCP health transition of `currentPrimary` from available to
unavailable triggers failover. UDP transitions and transitions from standby
candidates are ignored.

On the transition:

1. Atomically select the fixed fallback for new TCP connections and new UDP
   sessions.
2. Keep existing connections and endpoints on their already-selected dialer.
3. Set `recoveryTarget` to the failed current primary.
4. Set `rotationActive` to false and
   `failedRecoveryProbes` to zero.
5. Reset the recovery backoff to `recovery_probe_initial` and schedule the
   first targeted probe.
6. Emit one existing `failover_switch` event using the actual dynamic current
   primary name and the fixed fallback name.

Many concurrent failures must still create one logical transition and one
recovery schedule.

## Recovery Attempt Timing

Before rotation is active, only the failed current primary is probed. Failed
probes use the existing exponential backoff:

```text
initial, initial * 2, initial * 4, ... recovery_probe_max
```

With the production values `15s` initial, `5m` maximum, and a rotation
threshold of five failures, failed probes against A occur at approximately:

```text
failure 1: 00:15
failure 2: 00:45
failure 3: 01:45
failure 4: 03:45
failure 5: 07:45
```

The fifth failure activates rotation, advances `recoveryTarget` from A to B,
and preserves the existing capped backoff. B therefore receives the next probe
at approximately `12:45` after the original failover.

The configured attempt threshold is based on failed network probe results, not
wall-clock time. There is no separate ten-minute deadline or timer.

## Recovery Confirmation Before Rotation

A successful probe always starts or continues recovery confirmation for the
current `recoveryTarget`:

- The controller remains on the fixed fallback while confirming recovery.
- The first success records `stableSince`.
- Confirmation probes run at `recovery_probe_initial`.
- Promotion requires both `recovery_successes` consecutive successes and
  `recovery_stable_time` elapsed since the first success.

The attempt threshold never interrupts a target that is currently producing
successful confirmation probes. Because only failed probes consume the
attempt budget, reaching the threshold necessarily follows a failed probe.
If the current primary has accumulated four failures, begins confirmation, and
then fails confirmation, that failure becomes failure five and rotation starts.

A success does not erase earlier failures in the same failover episode. This
prevents an intermittently reachable current primary from indefinitely
blocking standby-candidate recovery.

## Ordered Candidate Rotation

After `rotationActive` becomes true:

- A failed probe clears the target's recovery-success count and stability
  timestamp.
- The next target is the next primary candidate in numeric-priority order.
- The order wraps circularly after the final candidate.
- The existing recovery backoff continues across failed targets and remains
  capped by `recovery_probe_max`.
- One target failure is enough to advance; candidates do not each receive a
  fresh five-attempt exclusive window during the same failover episode.
- A target's first successful probe stops cursor movement while that target
  completes the existing stable-recovery confirmation.
- If confirmation fails, the next scheduled probe targets the following
  candidate.

For `[A, B, C]`, the rotation sequence after A exhausts its attempt budget is:

```text
B -> C -> A -> B -> ...
```

The fixed fallback continues carrying new traffic throughout this sequence.
If every candidate is unavailable, the controller remains on the fallback and
continues the bounded-backoff circular scan indefinitely.

## Promotion and Future Failures

When a recovery target satisfies both stable-recovery conditions:

1. Set that target as `currentPrimary`.
2. Atomically select it for new traffic.
3. Reset `rotationActive`, `failedRecoveryProbes`, recovery counters,
   stability time, and backoff.
4. Stop the recovery schedule.
5. Emit the existing `failback_complete` event from the fixed fallback to the
   promoted node.

If B is promoted, A becoming healthy later does not preempt B and does not
start background promotion work. B remains current primary until B itself
fails. On a later B failure, the controller immediately selects the fixed
fallback, gives B five failed recovery attempts, and then scans in circular
order beginning with C.

## Selection and Existing Connections

Only new selections observe a failover or promotion snapshot. Existing TCP
connections and UDP endpoints stay attached to the dialer chosen when they
were created. The existing bounded retry and excluded-dialer behavior remains
authoritative for an individual connection attempt. Rotation must not add an
extra connection-level retry, clear the excluded dialer, or use an unconfirmed
standby candidate as an opportunistic retry target. If the current atomic role
selects a dialer that is excluded for that connection attempt, selection must
follow the existing failover-policy exclusion/error behavior rather than scan
the ordered candidate list.

If the fixed fallback cannot dial while the controller is in fallback or
recovering state, the operation returns the real dial error. Fallback failure
does not select an unconfirmed primary candidate.

## Warm Reload and Process Restart

The rotation state is memory-only. DAE must not write it to the configuration,
a state file, or another persistent store.

An in-process warm reload may inherit rotation state only when both generations
have the same fixed-fallback identity, the same ordered primary-candidate
identity list, and identical values for all state-interpreting policy fields:

```text
primary_rotation_attempts
recovery_probe_initial
recovery_probe_max
recovery_successes
recovery_stable_time
```

The snapshot includes:

```text
currentPrimary
recoveryTarget
rotationActive
failedRecoveryProbes
recoverySuccesses
stableSince
currentDelay
nextProbeAt
probeInFlight
```

Identity fields are remapped by node name in the new controller. Remaining
probe delay is preserved. If capture occurs while a probe is in flight, the
old probe is cancelled or invalidated and the replacement controller schedules
exactly one immediate probe of the same `recoveryTarget`, preserving counters,
confirmation state, and `currentDelay`. The replacement must neither wait on a
past `nextProbeAt` nor run both the old and new probes. Old timers and probe
results are invalidated by generation before the new controller can act.

If the fixed fallback, ordered candidate topology, or any listed policy field
differs, the new generation does not inherit or normalize the old rotation
snapshot. It initializes from the new configuration's `priority: 0` primary
and emits a structured
`failover_rotation_state_reset` log with `group`, `reason`,
`old_current_primary`, and `new_initial_primary`.

`reason` is `fallback_changed`, `primary_candidates_changed`, or
`recovery_policy_changed`. If multiple categories change in one reload, the
reason uses that precedence order. Requiring exact policy compatibility avoids
carrying a delay above a new maximum, applying an old attempt count to a new
threshold, or interpreting old confirmation progress under new success/stable
requirements.

A complete process stop and restart has no in-memory predecessor from which to
inherit. It always starts with `priority: 0` as current primary, even if B or C
was current before shutdown.

## Observability and Notifications

The existing transition event names remain stable:

- `failover_switch` reports the actual dynamic current primary and fixed
  fallback.
- `failback_complete` reports the fixed fallback and the candidate actually
  promoted to current primary.

Existing Bark templates and tokens remain compatible. Routine candidate
movement does not emit Bark events.

Structured logs are added for:

```text
primary_rotation_started
recovery_target_advanced
failover_rotation_state_reset
```

`primary_rotation_started` and `recovery_target_advanced` include these fields:

```text
group
failed_primary
from_target
to_target
failed_attempts
next_probe_in
```

For `primary_rotation_started`, `failed_primary` and `from_target` are the
current primary that triggered failover, `to_target` is the first standby
candidate, and `failed_attempts` is `5` for the approved configuration. For
every later `recovery_target_advanced`, `failed_primary` remains the primary
that triggered the current failover episode, `from_target` is the target whose
probe just failed, `to_target` is the next circular target, and
`failed_attempts` is the monotonically increasing total failed recovery-probe
count in that failover episode. `next_probe_in` is the actual scheduled delay
before probing `to_target`.

`primary_rotation_started`, `recovery_target_advanced`, ordinary probe errors,
and candidate advancement are debug-level diagnostic events. The topology
reset log is info-level because it explains an operator-visible warm-reload
state reset. None of these logs or a notifier failure may block failover,
change counters, change timers, or affect promotion.

# Error Handling

- A probe error is a failed result for the current recovery target.
- Failure of every primary candidate leaves the fixed fallback active and the
  circular recovery schedule running.
- Failure of the fixed fallback is returned to the caller and does not promote
  an unconfirmed primary.
- Cancellation during shutdown or reload must not count as a recovery failure
  or advance the candidate cursor in a replacement controller.
- A callback from a non-current candidate must not change active selection.
- A stale timer or probe result from an older generation must be ignored.
- Invalid rotation configuration must fail validation before activation.
- Rotation and notification failures must remain isolated: a notifier error
  cannot change failover state, timers, counters, or the selected dialer.

# Testing Strategy

## Configuration Tests

Add decoding and validation coverage for:

- legacy two-node failover with rotation omitted or zero;
- valid priorities `0`, `1`, and ordered candidates `2+`;
- candidate priority gaps;
- missing roles or candidates;
- duplicate priorities and duplicate underlying dialers;
- one broad filter resolving multiple dialers with the same priority;
- negative rotation attempts; and
- use of rotation attempts with a non-failover policy.

## Controller Tests

Use injected probe outcomes and short or controlled timer intervals to prove:

- immediate fallback on current-primary TCP failure;
- five failed current-primary probes before target advancement;
- exact exponential-delay progression and capped delay preservation;
- success confirmation is not interrupted by the attempt threshold;
- successes do not erase the accumulated current-primary failure count;
- sufficient successes before stable time does not promote;
- sufficient stable time with too few consecutive successes does not promote;
- a confirmation failure resets consecutive successes and stability time;
- serial `A -> B -> C -> A` target movement;
- one target's success holds the cursor for stable confirmation;
- promotion changes only the current primary and new-flow selection;
- no preemption when an earlier candidate later becomes healthy;
- a newly promoted primary receives its own five-attempt window after a future
  failure;
- all candidates unavailable leaves fallback selected; and
- fallback failure returns an error without selecting an unconfirmed target.

Selection tests must also prove that rotation-enabled ordinary selection uses
one atomic snapshot read without controller locking, candidate traversal, or
health work; that connection-level exclusion remains bounded; and that healthy
operation creates no recovery timer, standby periodic probe, or candidate
selection work.

## Lifecycle and Concurrency Tests

Prove that:

- concurrent primary failures create one state transition and recovery timer;
- at most one recovery probe is in flight;
- close cancels timers and probes;
- stale generation callbacks cannot mutate the new controller;
- a topology-compatible warm reload preserves current primary, target, failure
  count, confirmation state, backoff, and remaining delay;
- reload during an in-flight probe cancels or invalidates the old probe and
  schedules exactly one immediate replacement probe of the same target;
- a topology- or recovery-policy-changing reload resets to the new initial
  primary and emits the specified state-reset log and reason; and
- a fresh controller representing a process restart starts at `priority: 0`
  without reading or writing persistent rotation state.

Run focused package tests first, then the Linux stub-eBPF suite in the project
OrbStack environment:

```bash
go test -tags dae_stub_ebpf ./component/outbound ./config ./control ./cmd -count=1
go test -tags dae_stub_ebpf ./... -count=1
```

Production rollout validation must include config validation, service health,
TCP and UDP port 53 ownership, DNS smoke tests, generated routing consistency,
and the existing failover BPF connectivity checks. A deliberate production
primary outage is not part of automatic rollout acceptance and requires
separate user authorization.

<!-- IMPLEMENTATION-SPEC-END -->

<!-- ACCEPTANCE-BEGIN -->

# Completion Contract

Implementation is complete only when every Acceptance Criterion and every
Rollout Acceptance check is independently verified as PASS with its required
evidence. Aggregate test success cannot replace semantic source inspection or
scenario-specific evidence.

# Verification Protocol

- Verify each Acceptance Criterion independently; do not approve from aggregate test results alone.
- When a criterion references another spec definition, read and compare the complete definition.
- Report PASS, FAIL, or NOT VERIFIED for every criterion.
- A PASS must include all Required Evidence named by the criterion.
- Missing required evidence means the criterion is NOT VERIFIED, not PASS.
- Test success does not replace required source, boundary, or runtime semantic checks.
- Execute Rollout Acceptance checks with the same evidence rules.
- Only when every criterion and every Rollout Acceptance check is PASS may the task and automated loop stop.

# Acceptance Criteria

### AC-01: Legacy two-node failover remains compatible

**Requirement:** With `primary_rotation_attempts` omitted or set to `0`, a failover group containing one `priority: 0` primary and one `priority: 1` fallback must validate and retain the pre-existing failure, recovery, reload, selection, and notification behavior; a standby priority must not be accepted in this disabled mode.

**Verification Steps:**
1. Run named configuration and controller regression tests for omitted and zero-valued rotation settings.
2. Inspect the validation branch that distinguishes disabled rotation from enabled rotation.
3. Run a negative test containing `priority: 2` while rotation is disabled.

**Pass Conditions:** Both legacy configurations validate and exercise the original two-dialer behavior, while the disabled configuration containing a standby priority fails validation.

**Fail Conditions:** Legacy configuration changes behavior or fails validation, rotation activates implicitly, or a standby candidate is silently accepted while rotation is disabled.

**Required Evidence:** Test names and output, plus source locations for the default value and disabled-mode validation branch.

### AC-02: Enabled configuration builds one ordered primary list and one fixed fallback

**Requirement:** A rotation-enabled group must resolve exactly one initial primary at priority `0`, exactly one fixed fallback at priority `1`, and one or more distinct standby candidates at unique priorities `2+`, sorted numerically with gaps allowed.

**Verification Steps:**
1. Run validation tests for valid contiguous and gapped candidate priorities.
2. Run negative tests for missing roles, no candidate, duplicate priorities, duplicate dialers, unannotated dialers, broad-filter duplicate priority, negative attempts, and non-failover policy use.
3. Inspect construction code to confirm it sorts candidates by numeric priority rather than filter or subscription iteration order.

**Pass Conditions:** Valid inputs create the specified ordered identities and fixed fallback; every listed invalid input is rejected before activation.

**Fail Conditions:** Ordering depends on incidental iteration order, the fallback enters the primary list, a listed invalid configuration activates, or a valid gapped order is rejected.

**Required Evidence:** Configuration fixtures, named test output, and source locations for parsing, sorting, and validation.

### AC-03: Current-primary TCP failure selects the fixed fallback once

**Requirement:** A confirmed TCP-unavailable transition from the dynamic current primary must atomically select the fixed fallback for new TCP connections and UDP sessions, initialize one recovery schedule against that failed primary, preserve existing flows, and emit one `failover_switch`; UDP or standby-candidate transitions must not trigger this behavior.

**Verification Steps:**
1. Exercise current-primary TCP failure, UDP failure, standby TCP failure, and concurrent current-primary failures.
2. Inspect the callback identity check and atomic selection path.

**Pass Conditions:** The TCP failure produces one transition, one timer, and one event; new flows use fallback; existing flows are not migrated; the other transitions do not switch the group.

**Fail Conditions:** Duplicate schedules or events occur, a non-current candidate or UDP transition switches the group, fallback is not selected, or existing flows are terminated or reassigned.

**Required Evidence:** Named scenario tests and output, event assertions, timer/probe counts, and source locations for callback filtering and snapshot publication.

### AC-04: Five failed probes activate rotation with the existing backoff

**Requirement:** With a controlled logical clock, `15s` initial delay, `5m` maximum delay, and `primary_rotation_attempts: 5`, failed current-primary probes must occur at exact offsets `00:15`, `00:45`, `01:45`, `03:45`, and `07:45`; failure five advances the target to the next candidate, whose probe is scheduled at exact offset `12:45`, without a separate wall-clock deadline.

**Verification Steps:**
1. Run a controlled-clock or recorded-delay test with five failed probe results.
2. Inspect the failure counter, backoff update, threshold comparison, target advancement, and next scheduling calculation.

**Pass Conditions:** The controlled-clock sequence exactly matches every listed logical offset and target transition, and no ten-minute timer or parallel probe exists. Real-time tests may supplement this evidence but cannot replace the exact logical-clock assertion.

**Fail Conditions:** Rotation is time-deadline based, begins before or after the fifth failure, resets backoff before B, checks B concurrently, or produces a different delay sequence.

**Required Evidence:** Named test output containing target identities and scheduled delays, plus source locations for threshold and scheduling logic.

### AC-05: Recovery confirmation is stable and does not erase the attempt history

**Requirement:** A successful target probe must hold the cursor on that target and require the configured consecutive successes and stable time before promotion; success must not clear accumulated current-primary failures, and a subsequent failure that exhausts the attempt budget must advance to the next candidate.

**Verification Steps:**
1. Exercise four current-primary failures, one or more successful confirmation probes, and a final failed confirmation.
2. Exercise three consecutive successes completed before 30 seconds have elapsed since the first success.
3. Exercise at least 30 seconds elapsed since the first success with fewer than three consecutive successes.
4. Exercise a confirmation failure, then verify the next success starts a new stability window and success sequence.
5. Exercise three consecutive successes spanning at least 30 seconds after prior failures.

**Pass Conditions:** The failed confirmation becomes failure five and advances the target; neither single-condition boundary promotes; confirmation failure clears both confirmation fields; only the sequence satisfying both three consecutive successes and 30 seconds of stability promotes the target.

**Fail Conditions:** The attempt count resets on success, the cursor advances during successful confirmation, success count alone promotes, elapsed time alone promotes, confirmation failure leaves either confirmation field intact, or a fully qualified target is not promoted.

**Required Evidence:** Named transition tests with probe sequences, counter/state assertions, and source locations for success and failure handlers.

### AC-06: Candidate scanning is serial, circular, and one failure per candidate

**Requirement:** Once rotation is active for `[A, B, C]`, failed targets must advance `B -> C -> A -> B` on successive scheduled probes, with one timer and at most one in-flight probe; candidates must not each receive a new five-attempt window in the same failover episode.

**Verification Steps:**
1. Run a probe sequence in which all candidates fail through more than one wrap.
2. Instrument maximum concurrent probe count and recorded target order.

**Pass Conditions:** Target order is circular and exact, concurrency never exceeds one, fallback remains selected, and each failure advances one position.

**Fail Conditions:** Probes overlap, a target is skipped or probed out of order, candidate-specific five-attempt windows reappear, the cursor stops after one wrap, or an unavailable target becomes active.

**Required Evidence:** Named test output with ordered target history, concurrency maximum, selected-dialer assertions, and source locations for cursor advancement.

### AC-07: A qualified candidate becomes the sole sticky current primary

**Requirement:** A candidate satisfying the configured success count and stable time must become the only current primary for new traffic, reset the failover episode, and remain current even if an earlier-priority candidate becomes healthy; existing flows must not migrate.

**Verification Steps:**
1. Qualify B after A fails and assert new-flow selection.
2. Publish a healthy transition from A and reassert selection.
3. Inspect promotion reset logic and non-current callback filtering.

**Pass Conditions:** B remains selected for new flows, prior flows remain untouched, counters and rotation state reset, and A does not preempt B.

**Fail Conditions:** Multiple primaries participate in selection, A preempts B, promotion leaves stale rotation state, or existing flows move.

**Required Evidence:** Named test output, before/after snapshots, flow-selection assertions, and source locations for promotion and callback filtering.

### AC-08: A promoted primary receives a fresh attempt budget on its later failure

**Requirement:** After B is promoted, a later confirmed TCP failure of B must immediately select the fixed fallback, probe B for five failed attempts, and then begin candidate rotation at C rather than A or B.

**Verification Steps:**
1. Promote B, trigger its failure, and feed five failed B probe results.
2. Record active selections and the next recovery target.

**Pass Conditions:** Fallback is selected immediately, all five initial recovery targets are B, and the following target is C.

**Fail Conditions:** The controller remembers A as failed primary, begins rotation before five B failures, starts at the wrong candidate, or changes the fixed fallback.

**Required Evidence:** Named end-to-end controller test output with target history and state snapshots.

### AC-09: Unavailable candidates never displace the fixed fallback

**Requirement:** If every primary candidate remains unavailable, the controller must keep the fixed fallback selected, continue the capped-backoff circular scan, and return real fallback dial errors without selecting an unconfirmed candidate.

**Verification Steps:**
1. Run multiple full failed candidate cycles while fallback succeeds.
2. Repeat with a fallback dial error.

**Pass Conditions:** Selection remains fallback in both cases; scanning continues; the second case returns the fallback error; no candidate is promoted.

**Fail Conditions:** The controller exits recovery, selects a failed candidate, loops ordinary connection retries indefinitely, suppresses the fallback error, or changes fallback identity.

**Required Evidence:** Named tests and output showing snapshots, target history, and returned error identity.

### AC-10: Warm reload continues compatible work and resets incompatible topology

**Requirement:** When fixed-fallback identity, ordered candidate names, `primary_rotation_attempts`, `recovery_probe_initial`, `recovery_probe_max`, `recovery_successes`, and `recovery_stable_time` all match, a warm reload must remap and inherit current primary, recovery target, rotation state, monotonic episode-total `failedRecoveryProbes`, success count, stability timestamp, backoff, and pending remaining delay while invalidating the old generation; an in-flight old probe must become exactly one immediate replacement probe of the same target. When any identity or listed policy value differs, the new controller must reject partial inheritance, initialize from the new `priority: 0`, and emit info-level `failover_rotation_state_reset` with `group`, the deterministic `reason` category defined in Warm Reload, `old_current_primary`, and `new_initial_primary`.

**Verification Steps:**
1. Capture a nontrivial rotating or recovering snapshot with B current or targeted.
2. Restore it into a new controller built from equivalent identities with new object addresses.
3. Repeat capture while a probe is in flight, then complete or cancel the old probe after restoration and record replacement probe count, target, and scheduling time.
4. Restore into controllers with a changed fallback and with a changed ordered candidate identity list.
5. In table-driven cases, change each of `primary_rotation_attempts`, `recovery_probe_initial`, `recovery_probe_max`, `recovery_successes`, and `recovery_stable_time` while keeping topology fixed.
6. Exercise one reload changing fallback, candidates, and policy simultaneously to verify reset-reason precedence.
7. Capture every reset log level and field.

**Pass Conditions:** Fully compatible pending-timer state preserves every required field, the exact total failed-probe count, and remaining delay; compatible in-flight state produces one immediate same-target replacement and no old-state mutation; every identity or policy mismatch starts at its new initial primary, inherits none of the old rotating/recovering fields, and emits the required info-level reset log with the correct reason, including precedence when multiple categories differ.

**Fail Conditions:** Restoration relies on old slice indexes or pointers, loses or resets `failedRecoveryProbes`, loses another named field, resets despite full compatibility, stalls or duplicates an in-flight probe, accepts stale mutation, normalizes or partially inherits state after any listed mismatch, starts from a noninitial candidate, or omits/misstates the reset log, reason precedence, or info level.

**Required Evidence:** Named compatible, in-flight, fallback-changed, candidate-list-changed, five policy-field-changed, and multi-category reload tests; old/new object identity assertions; before/after `failedRecoveryProbes`; timer/probe counts and targets; captured reset log levels, reasons, and fields; and source locations for capture, compatibility matching, restoration, reset, and generation guards.

### AC-11: Restart resets to the configured initial primary without persistence

**Requirement:** A fresh process/controller generation without a warm-reload snapshot must always choose `priority: 0` as current primary and must not read, write, or modify persistent state or configuration to remember a rotated primary.

**Verification Steps:**
1. Promote B, close the controller, then construct a fresh controller from the same configuration without passing a snapshot.
2. Search the implementation for file or configuration writes related to rotation state.

**Pass Conditions:** The fresh controller selects A, no rotation persistence path exists, and the configuration remains unchanged.

**Fail Conditions:** B survives a full restart, a state file is created/read, configuration priority is rewritten, or startup selection depends on prior process memory.

**Required Evidence:** Named restart-simulation test output, selected identity assertions, and source-search results covering persistence and config writes.

### AC-12: Logs and notifications report dynamic identities without extra Bark noise

**Requirement:** `failover_switch` must name the actual current primary and fixed fallback, `failback_complete` must name the promoted target, candidate movement must emit debug-level structured `primary_rotation_started` and `recovery_target_advanced` logs without Bark, and ordinary probe errors must remain debug-level. In a failover triggered by A, `failed_primary` remains A until promotion; `from_target` and `to_target` identify the actual cursor edge; `failed_attempts` equals the monotonic episode-total `failedRecoveryProbes`; and `next_probe_in` equals the delay scheduled for `to_target`. Notifier failure must not change selection, counters, timers, cursor, or event progression.

**Verification Steps:**
1. Promote B, fail B, advance through C and A, and capture events/log entries.
2. Continue for more than one circular target advance and assert every field value and debug level at each edge.
3. Inject an ordinary probe error and capture its log level.
4. Inject a notifier failure during `failover_switch` and `failback_complete`, then compare controller snapshots, timer counts, cursor, and subsequent transition events with a notifier-success control run.
5. Inspect notifier dispatch boundaries and template data construction.

**Pass Conditions:** Transition events contain dynamic B/C identities as applicable; rotation log level and every field value follow the requirement across wraparound; the probe-error log is debug-level; Bark event count changes only for failover switch and completed failback; injected notifier failures leave controller and scheduling evidence identical to the control run except for notifier diagnostics.

**Fail Conditions:** Notifications retain the initial A identity, cursor movement emits Bark, a rotation/reset/probe-error log has the wrong required level or field meaning, `failed_primary` changes during one episode, failure count is not monotonic or differs from `failedRecoveryProbes`, scheduled delay is misstated, or notifier failure affects controller state, timers, cursor, or later events.

**Required Evidence:** Named log/event and notifier-failure tests; captured levels, fields, event counts, before/after snapshots, timer counts, and target history; plus source locations for event construction and dispatcher calls.

### AC-13: Controller lifecycle remains single-owner and cancellation-safe

**Requirement:** Each failover group must have at most one pending recovery timer and one in-flight recovery probe; close and warm replacement must cancel or invalidate both without counting cancellation as failure or advancing a candidate.

**Verification Steps:**
1. Exercise concurrent failure callbacks, close during a scheduled timer, close during an active probe, and generation replacement during probe completion.
2. Run relevant tests with Go's race detector where supported by the Linux test environment.

**Pass Conditions:** Timer/probe maxima are one, no post-close transition occurs, cancellation does not alter counters/cursor, and race detection reports no failure.

**Fail Conditions:** Duplicate timers/probes appear, cancelled work advances state, post-close logs/events occur, stale mutation occurs, or a data race is reported.

**Required Evidence:** Named lifecycle tests, concurrency counters, race-test output, and source locations for locking, cancellation, and generation checks.

### AC-14: Rotation preserves bounded connection retry and exclusion semantics

**Requirement:** In rotation-enabled mode, an ordinary connection attempt must retain the existing bounded retry and excluded-dialer behavior; rotation must not add connection-level retries, clear an exclusion, traverse standby candidates as opportunistic retry targets, or select an unconfirmed candidate when the atomic active role is excluded or fails.

**Verification Steps:**
1. Exercise active-current-primary and active-fallback selection with each active dialer passed as excluded.
2. Exercise a selected-dialer failure while standby candidates are configured and record connection-level selection attempts.
3. Inspect the selection branch to confirm candidate traversal occurs only in recovery-controller work, not connection retry.

**Pass Conditions:** Attempt counts remain at the existing bound, exclusions are honored, no standby candidate is selected through ordinary retry, and the existing error/fallback result is preserved for each scenario.

**Fail Conditions:** Rotation adds a retry, clears or ignores exclusion, scans B/C from the connection path, hides the selected dialer's real error, or selects a candidate before stable recovery promotion.

**Required Evidence:** Named selection/retry tests with attempt counts, excluded identities, selected-target history, returned errors, and source locations for selection and recovery traversal.

### AC-15: Healthy hot path stays atomic and standby candidates stay idle

**Requirement:** Rotation-enabled ordinary selection while the group is healthy must use the immutable atomic snapshot without controller mutex acquisition, candidate traversal, timer creation, or network health work; merely configuring standby candidates must not enable periodic probes or health timers for them.

**Verification Steps:**
1. Inspect and exercise the ordinary TCP and UDP selection paths with A active and B/C configured.
2. Instrument controller-lock acquisition, candidate-iteration count, recovery-timer count, and probe calls during repeated healthy selections and an idle interval longer than the configured check interval.
3. Inspect candidate health-worker activation to distinguish event callback registration from periodic network checking.

**Pass Conditions:** Selection performs one atomic active-role read, lock/iteration/timer/probe counts remain zero for healthy ordinary selection and the idle interval, and B/C produce no periodic network probes solely because of membership.

**Fail Conditions:** Selection locks the controller, traverses candidates, performs health work, creates recovery timers while healthy, or standby membership activates periodic probing.

**Required Evidence:** Source locations for the atomic hot path and candidate registration, named instrumentation tests, counter values, and idle-interval probe history.

### AC-16: Probe errors follow the same state transition as failed results

**Requirement:** A non-cancellation error returned by the one-shot probe must be treated as a failed result for the current recovery target: increment `failedRecoveryProbes`, clear consecutive successes and `stableSince`, update the existing exponential backoff, and, when rotation is active, advance exactly one candidate. Shutdown or reload cancellation remains governed by AC-13 and must not perform these mutations.

**Verification Steps:**
1. Before rotation, inject four ordinary failed results followed by a probe error against A and record counter, target, and delay.
2. During B recovery confirmation after rotation is active, inject a probe error and record confirmation fields, target, and delay.
3. Compare both cases with equivalent `ok == false, err == nil` control sequences and separately exercise cancellation through AC-13.

**Pass Conditions:** The A error becomes failed probe five, sets the total to five, advances the next target to B, and preserves the required capped schedule; the B error clears both confirmation fields, increments the total, advances exactly to C, and schedules the same delay as its ordinary-failure control; cancellation performs none of these mutations.

**Fail Conditions:** A probe error is ignored, fails to consume the attempt budget, leaves confirmation state intact, advances more than one target, changes selection away from fallback, schedules a different delay than ordinary failure, or cancellation is counted as failure.

**Required Evidence:** Named pre-rotation, rotating-confirmation, ordinary-failure-control, and cancellation tests with returned errors, before/after counters, confirmation fields, targets, selected dialer, and scheduled delays; plus source locations for error classification and failure handling.

### AC-17: Linux source validation passes

**Requirement:** The focused failover/config/control tests and the complete stub-eBPF Go test suite must pass in the configured OrbStack Linux environment after the implementation and documentation changes.

**Verification Steps:**
1. Run `go test -tags dae_stub_ebpf ./component/outbound ./config ./control ./cmd -count=1`.
2. Run `go test -tags dae_stub_ebpf ./... -count=1`.

**Pass Conditions:** Both commands exit successfully with no failing package.

**Fail Conditions:** Either command fails, is skipped, is run only on unsupported macOS, or omits implementation-relevant packages.

**Required Evidence:** Complete command lines, OrbStack machine identity, exit codes, and package test output summaries.

# Rollout Acceptance

### RA-01: Production configuration validates before activation

**Requirement:** Before any live edit, the rollout record must contain a user-approved mapping from every configured priority to an exact live dialer name: priority `0` is the existing live primary, priority `1` is `xray_local`, and every priority `2+` is an explicitly named standby candidate in approved order. The live gateway configuration must implement that mapping with `primary_rotation_attempts: 5` and pass DAE validation before reload. This design deliberately does not guess the production B/C identities.

**Verification Steps:**
1. Record the redacted priority-to-dialer mapping and the user's explicit approval of the exact ordered candidate list.
2. Read and back up the live `/etc/dae/config.dae`.
3. Compare every live filter and priority against the approved mapping.
4. Apply the smallest targeted group edit.
5. Run `/usr/bin/dae validate -c /etc/dae/config.dae` before reload.

**Pass Conditions:** The approval record names every production role, the live group exactly matches that mapping and the attempt threshold, and validation succeeds without exposing secrets.

**Fail Conditions:** Any B/C identity or ordering is guessed or unapproved, a mapped node is absent, validation fails, a sanitized repository config replaces the live file, roles are ambiguous, or activation occurs before approval and validation.

**Required Evidence:** User approval record, redacted exact priority mapping, backup path, redacted live group excerpt, comparison result, validation command, and exit status.

### RA-02: Gateway service and network ownership remain healthy

**Requirement:** Successful feature activation is required for PASS. After activation, DAE must remain active, own TCP and UDP port 53, return a FakeIP answer for `wikipedia.org` through DAE DNS, retain source/generated routing consistency, and report every expected `proxy_failover` BPF connectivity slot as `1`. Any failed activation or health check must restore the timestamped live-config backup, validate it, restart DAE, and repeat the same service, listener, DNS, routing, and BPF checks as safety evidence; a healthy rollback leaves this rollout criterion FAIL or NOT VERIFIED because the feature is inactive.

**Verification Steps:**
1. Reload DAE using the production-safe workflow, then run `ssh vm-ubuntu-agent 'sudo systemctl is-active dae'` and require `active`.
2. Run `ssh vm-ubuntu-agent "sudo ss -lntup | grep -E ':(53)[[:space:]]'"` and identify both TCP and UDP DAE listeners.
3. Run `ssh vm-ubuntu-agent 'dig @127.0.0.1 -p 53 wikipedia.org A +short'` and require at least one IPv4 answer in `198.18.0.0/15`.
4. Run `/Users/lihu/git/dae-config/skills/dae-gateway-operations/scripts/dae-ops routing` and require no source/generated inconsistency.
5. Run `/Users/lihu/git/dae-config/skills/dae-gateway-operations/scripts/dae-ops failover-bpf` and require every expected `proxy_failover` slot to be `1`.
6. Run `/Users/lihu/git/dae-config/skills/dae-gateway-operations/scripts/dae-ops logs 10` and inspect for validation, startup, reload, DNS, routing, failover-controller, or BPF errors introduced by activation.
7. If any preceding step fails, restore the exact backup recorded by RA-01, run `/usr/bin/dae validate -c /etc/dae/config.dae`, restart DAE, and repeat steps 1 through 6 against the restored configuration.

**Pass Conditions:** The feature remains active and every named post-activation command meets its stated expected result without requiring rollback.

**Fail Conditions:** DAE is inactive; either listener protocol is missing; the DNS answer is empty, invalid, or outside `198.18.0.0/15`; routing differs; an expected BPF slot is absent or not `1`; introduced errors remain unexplained; a failed activation is not rolled back and reverified; or rollback succeeds but the inactive feature is reported as PASS.

**Required Evidence:** Exact commands and exit statuses; service/listener output; DNS answers; routing and BPF output; relevant recent logs; and, when rollback occurs, the backup path, restore/validate/restart commands, repeated health evidence, explicit inactive-deployment status, and an RA-02 result of FAIL or NOT VERIFIED.

### RA-03: No unapproved production outage is used as proof

**Requirement:** Production rollout must not deliberately disable or corrupt the live primary merely to trigger rotation unless the user separately authorizes a controlled outage test.

**Verification Steps:**
1. Review the rollout command history and verification record.
2. If no controlled outage was authorized, classify real production rotation as observation-pending rather than fabricating end-to-end evidence.

**Pass Conditions:** Rollout uses safe non-destructive checks, or a separately authorized controlled test is documented with rollback and real-client verification.

**Fail Conditions:** The primary is intentionally disrupted without authorization, localhost-only reachability is presented as client-path proof, or unobserved production rotation is claimed as verified.

**Required Evidence:** Rollout command record, authorization record if applicable, and an explicit statement of whether a real rotation event was observed.

<!-- ACCEPTANCE-END -->
