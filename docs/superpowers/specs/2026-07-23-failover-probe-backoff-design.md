<!-- IMPLEMENTATION-SPEC-BEGIN -->

# Goal

Make failover recovery-probe pacing configurable without creating separate
recovery state machines. A failover group can choose either exponential or
fixed backoff:

```dae
proxy_failover {
    primary: name(A, B, C)
    fallback: name(X)
    policy: failover

    recovery_probe_backoff: fixed
    recovery_probe_initial: 15s
    primary_rotation_attempts: 5
}
```

`recovery_probe_backoff` accepts `exponential` and `fixed`. Its default is
`exponential`, preserving existing configurations. Fixed mode uses
`recovery_probe_initial` for every scheduled recovery or confirmation probe
and does not use `recovery_probe_max`.

Whenever recovery advances to a new Primary candidate, the next probe delay
resets to `recovery_probe_initial` in both modes. Candidate advancement does
not trigger an immediate probe: the controller waits one complete initial
interval before probing the new target.

# Non-Goals

- Do not add parallel candidate probing, background standby probing, or
  promotion based directly on a generic dialer `alive` transition.
- Do not change the fixed Fallback traffic contract. Fallback continues to
  carry production traffic until one recovery target completes confirmation.
- Do not make `recovery_probe_max` trigger candidate rotation. Rotation remains
  controlled only by `primary_rotation_attempts`.
- Do not add a separate fixed-interval field. Fixed mode deliberately reuses
  `recovery_probe_initial`.
- Do not change the 10-second network-probe timeout.
- Do not persist the current Primary, recovery target, counters, or timers.
- Do not add a strategy interface or separate controller implementation for
  the two simple delay calculations.

# Architecture

## Configuration model

`config.Group` gains a string-backed `recovery_probe_backoff` field with an
`exponential` default. The outbound failover layer converts it to a small
validated enum carried by `FailoverRecoveryConfig`:

```text
exponential
fixed
```

The enum belongs to the recovery configuration because it changes how the
controller interprets `currentDelay` and captured reload state.

## Unified delay calculation

The existing `FailoverController` remains the only recovery state machine.
One pure helper computes the delay scheduled after a failed probe from:

- configured backoff mode;
- current delay;
- initial delay;
- maximum delay; and
- whether the failure advanced to a new candidate.

Its semantic decision table is:

| Condition | Next delay |
| --- | --- |
| Candidate advanced | `recovery_probe_initial` |
| Fixed mode, same candidate | `recovery_probe_initial` |
| Exponential mode, same candidate | `min(currentDelay * 2, recovery_probe_max)` |

Candidate advancement has precedence over mode-specific same-candidate
calculation. The helper centralizes the only new runtime branch; timer,
probing, confirmation, promotion, notification, and snapshot ownership paths
remain shared.

## Reload identity

The normalized backoff mode is part of failover reload identity. A mode change
is `recovery_policy_changed` and must not inherit a timer, delay, failure
counter, confirmation state, or candidate cursor from the previous
controller.

`recovery_probe_max` is state-interpreting in exponential mode and is therefore
part of exponential identity. It has no meaning in fixed mode: two fixed-mode
configurations that differ only in `recovery_probe_max` are compatible and may
inherit state. Identity comparison must use effective recovery semantics
rather than raw struct equality for this case. In both modes, changes to
`recovery_probe_initial`, `recovery_successes`, `recovery_stable_time`, or
`primary_rotation_attempts` remain incompatible because they reinterpret a
captured timer, confirmation state, or failure count. Ordered Primary names
and the fixed Fallback name retain their existing identity rules.

# Detailed Design

## Configuration and validation

The accepted configuration values are case-sensitive `exponential` and
`fixed`. An absent field decodes to `exponential`. Empty, misspelled, or other
values fail configuration validation before control-plane cutover.

Validation rules are mode-aware:

1. `recovery_probe_initial` must be positive in both modes.
2. In exponential mode, `recovery_probe_max` must be positive and must be
   greater than or equal to `recovery_probe_initial`.
3. In fixed mode, `recovery_probe_max` is not read by delay calculation and
   does not participate in recovery-policy validation. The generic config
   parser must still decode an explicitly supplied value as a syntactically
   valid duration. Well-formed zero and negative durations are accepted and
   ignored in fixed mode; malformed duration text remains a parser error.
4. Existing validation for `recovery_successes`, `recovery_stable_time`, and
   non-negative `primary_rotation_attempts` remains unchanged.

Configuration descriptions and examples must state that
`recovery_probe_max` applies only to exponential mode and never causes
candidate advancement.

## Failure scheduling

A fresh failover episode initializes `currentDelay` to
`recovery_probe_initial` and schedules the first recovery probe after that
delay.

After a failed recovery probe, the controller:

1. increments the current target's consecutive failure count;
2. clears recovery confirmation state;
3. determines whether `primary_rotation_attempts > 0` and the failure count
   has reached the candidate threshold;
4. when the threshold is reached, advances `recoveryTarget` circularly and
   resets the failure count;
5. calculates the next delay with the unified decision table; and
6. schedules exactly one probe against the resulting recovery target.

When a candidate advances, the next probe waits one
`recovery_probe_initial`; it is not immediate. This reset applies on every
advance, including wraparound and the single-candidate modulo case.

When `primary_rotation_attempts == 0`, the candidate never advances. Fixed
mode continues probing it at the initial interval. Exponential mode doubles
the same target's delay until the maximum and then remains capped.

## Success and confirmation scheduling

A successful recovery probe retains the existing behavior:

- reset consecutive failure count;
- record or continue recovery confirmation;
- set `currentDelay` to `recovery_probe_initial`;
- schedule confirmation probes at the initial interval; and
- promote only after satisfying both `recovery_successes` and
  `recovery_stable_time`.

If a confirmation probe fails, confirmation state is cleared and the result
enters the same failure-scheduling path. Fixed mode schedules the next probe
at the initial interval. Exponential mode doubles from the confirmation
interval, subject to the maximum, unless that failure also advances the
candidate, in which case advancement resets the delay to the initial value.

## Single- and multi-candidate behavior

Candidate count does not introduce a separate code path. Advancement remains:

```text
(recoveryTarget + 1) % len(primaryCandidates)
```

For a single Primary and positive `primary_rotation_attempts`, reaching the
threshold advances back to the same array element, resets its failure budget,
and resets its probe delay to the initial interval. With attempts set to zero,
the same Primary remains selected without a logical advancement.

For multiple Primaries, every candidate receives an independent consecutive
failure budget. A candidate that produces a successful probe resets its
failure count and stays the recovery target during confirmation.

## Timing semantics

Configured delays are waits before a probe begins. Probe execution can add up
to the existing 10-second network timeout. With `initial: 15s`, `max: 5m`,
and `primary_rotation_attempts: 5`, a target whose failures consume the full
timeout has these approximate failure windows:

- fixed: five `(15s wait + 10s timeout)` probes, or 2m05s per candidate;
- exponential: waits of 15s, 30s, 1m, 2m, and 4m plus five timeouts, or 8m35s
  per candidate; and
- after either mode advances, the new candidate's first probe begins after
  15s rather than inheriting the previous candidate's capped delay.

The maximum is a delay cap, not a candidate deadline. For example,
`primary_rotation_attempts: 8` may produce several same-candidate probes at
the maximum interval before the eighth failure advances the cursor.

## Observability and documentation

Existing structured log event names remain unchanged. Their delay fields must
report the delay actually scheduled:

- `recovery probe failed, backing off` reports the fixed initial delay or the
  same-candidate exponential delay;
- `primary_rotation_started` and `recovery_target_advanced` report
  `next_probe_in=recovery_probe_initial` after every candidate advance.

Update comments and documentation that currently claim exponential backoff
stays capped across recovery targets. Add configuration examples for both
modes and explicitly describe the separation between delay capping and
attempt-count-based rotation.

## Production rollout boundary

The binary remains backward-compatible because an omitted field defaults to
exponential. Binary deployment must therefore occur without silently editing
live groups to fixed mode. Enabling fixed mode on production is a separate,
targeted operation that requires explicit authorization naming the affected
groups and the normal live-config backup, validation, activation, rollback,
and verification workflow.

# Error Handling

An invalid backoff value fails validation with an error naming
`recovery_probe_backoff`, the received value, and the accepted values. Invalid
exponential timing fails with the existing field-specific positive/range
errors. Fixed mode must not fail because of an unused
`recovery_probe_max` value after that value has been syntactically decoded as a
duration. Malformed duration text continues to fail in the generic parser and
is not made valid by fixed mode.

A reload-time validation error leaves the active control plane unchanged. A
valid reload that changes effective recovery policy does not inherit the old
controller's state; the existing incompatible-identity transaction behavior
keeps the old controller untouched until staged cutover succeeds.

No delay calculation may produce a negative duration or overflow into a
negative duration. Exponential calculation must cap safely at
`recovery_probe_max` without relying on an overflowing multiplication result.

# Testing Strategy

## Configuration tests

- Verify the absent field defaults to exponential.
- Verify explicit exponential and fixed values decode and build.
- Reject empty, misspelled, and unknown values with the required diagnostic.
- Verify exponential mode rejects non-positive maximum and `initial > max`.
- Verify fixed mode ignores well-formed positive, zero, and negative maximum
  durations during policy validation and scheduling while malformed duration
  text still fails generic parsing.
- Verify descriptions and examples distinguish maximum delay from rotation.

## Delay-calculation tests

Table-test the pure helper for fixed mode, exponential growth, exponential
cap, candidate advancement in both modes, and values near duration overflow.
The table must directly assert the scheduled duration, not infer it from a
controller field existing.

## State-machine tests

- Prove fixed mode schedules every same-candidate failure at the initial
  interval.
- Prove exponential mode doubles and caps while remaining on one candidate.
- Prove both modes reset to the initial interval on first rotation, later
  rotation, wraparound, and single-candidate modulo advancement.
- Prove attempts zero never advances while each mode continues its own delay
  behavior.
- Prove success confirmation remains at the initial interval and a
  confirmation failure returns through the selected failure strategy.
- Prove traffic remains on Fallback until promotion and notification event
  counts are unchanged.
- Assert structured rotation log fields contain the reset initial delay.

## Reload tests

- Preserve a nontrivial snapshot when effective recovery identity matches.
- Treat a backoff-mode change as `recovery_policy_changed` without pausing or
  mutating the old controller before cutover.
- Treat an exponential maximum change as incompatible.
- Treat a fixed-mode maximum-only change as compatible and preserve its timer,
  counter, target, and confirmation state.
- Treat changes to initial delay, required successes, stable time, and
  rotation attempts as incompatible in either mode.
- Stage invalid backoff values, malformed durations, and invalid exponential
  ranges through the production reload build path and prove the old pending or
  in-flight controller remains untouched and continues exactly once.
- Retain existing commit, rollback, stale-probe ownership, and race coverage.

## Validation commands

Run focused and complete tests in the repository's OrbStack Linux environment:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./config ./component/outbound ./control ./cmd -count=1
orb env GOCACHE=/tmp/dae-go-cache go test -race -tags dae_stub_ebpf ./component/outbound ./control -count=1
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./... -count=1
```

Run `gofmt` on modified Go files, `git diff --check`, and conflict-marker and
trailing-whitespace checks before commit.

<!-- IMPLEMENTATION-SPEC-END -->

<!-- ACCEPTANCE-BEGIN -->

# Completion Contract

Implementation is complete only when every Acceptance Criterion and every
Rollout Acceptance check is independently reported PASS with the required
evidence. Aggregate test success alone is insufficient.

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

### AC-01: Backoff configuration has two explicit modes

**Requirement:** `recovery_probe_backoff` accepts case-sensitive `exponential` and `fixed`; an absent field selects exponential, and any other value fails before control-plane cutover with a diagnostic naming the field, received value, and accepted values.

**Verification Steps:** Decode and build groups with the field absent, with each accepted value, and with empty, misspelled, and unknown values through the production configuration path.

**Pass Conditions:** The absent and explicit accepted configurations build with the required effective mode; every invalid configuration fails with the required diagnostic before replacing an active control plane.

**Fail Conditions:** An accepted value fails, absence selects fixed, an invalid value is normalized or ignored, or the diagnostic omits required context.

**Required Evidence:** Config field/default source, enum parsing and validation source, named production-path tests, and focused test output.

### AC-02: Fixed mode uses only the initial delay

**Requirement:** In fixed mode, the first recovery probe, every failed same-candidate probe, every confirmation probe, and every post-advance probe are scheduled after `recovery_probe_initial`; a syntactically valid `recovery_probe_max` neither changes scheduling nor causes recovery-policy validation failure, while malformed duration text remains a generic parser error.

**Verification Steps:** Exercise fixed controllers with multiple positive, zero, and negative maximum durations using a fake scheduler; drive initial failure, repeated failures, successful confirmation, confirmation failure, and candidate advancement; separately parse malformed maximum duration text.

**Pass Conditions:** Every scheduled delay equals the positive initial value, differing well-formed maximum values do not change policy validation or observed scheduling, and malformed text fails during generic duration parsing.

**Fail Conditions:** Any fixed-mode delay derives from a decoded maximum, a well-formed maximum rejects an otherwise valid fixed recovery policy, malformed text reaches runtime, or a candidate is probed immediately after advancement.

**Required Evidence:** Unified delay-helper source, fixed-mode validation source, named fake-scheduler tests covering every listed transition, and focused test output.

### AC-03: Exponential mode grows safely and caps

**Requirement:** In exponential mode, a fresh failover episode schedules its first recovery probe after `recovery_probe_initial`; later same-candidate failed probes double the current delay without exceeding `recovery_probe_max`; maximum must be positive and no smaller than initial, and duration arithmetic must not overflow to a negative or uncapped value.

**Verification Steps:** Trigger a fresh exponential failover episode with a fake scheduler, then table-test growth below the cap, growth crossing the cap, repeated capped probes, invalid maximum ranges, and current delays near duration overflow.

**Pass Conditions:** The fresh episode has one probe pending after exactly the initial interval; every later computed failure delay equals `min(current*2, max)` under safe arithmetic, remains at maximum after capping, and invalid ranges fail validation.

**Fail Conditions:** A fresh exponential episode probes immediately or uses a non-initial delay, a later delay exceeds maximum, wraps negative, resets without advancement, maximum triggers rotation, or an invalid exponential range builds.

**Required Evidence:** Delay-helper and exponential-validation source, named boundary tests including overflow, and focused test output.

### AC-04: Candidate advancement resets delay in both modes

**Requirement:** When `primary_rotation_attempts` advances the recovery target, fixed and exponential modes reset the scheduled delay to `recovery_probe_initial`, wait that interval before probing, reset the new target's failure count, and preserve serial circular candidate order.

**Verification Steps:** Drive first advancement, a later advancement, full wraparound, and single-candidate modulo advancement in each mode with non-initial pre-advance delay and a fake scheduler.

**Pass Conditions:** Every advance selects `(index+1)%len`, sets failure count to zero, has one pending probe after exactly the initial interval, and has no parallel or immediate candidate probe.

**Fail Conditions:** A new candidate inherits a capped delay, is probed immediately, receives an old failure count, is skipped, or overlaps another logical probe.

**Required Evidence:** Failure-transition source, named state-machine tests for each required edge, scheduler pending-count assertions, and focused test output.

### AC-05: Rotation remains attempt-controlled

**Requirement:** `recovery_probe_max` never advances a candidate; only reaching a positive `primary_rotation_attempts` consecutive-failure threshold advances, while zero keeps the current recovery target pinned in either mode.

**Verification Steps:** Run exponential mode at its cap below a positive attempt threshold, run additional failures until the threshold, and run fixed and exponential controllers with attempts zero across more failures than a normal threshold.

**Pass Conditions:** The capped target remains unchanged until the configured failure count is reached, advances on that failure, and attempts-zero targets never advance.

**Fail Conditions:** Maximum delay or elapsed time advances a target, an advance occurs early or late relative to the failure count, or attempts zero rotates.

**Required Evidence:** Advancement-condition source, named threshold/zero tests, target and counter assertions, and focused test output.

### AC-06: Success confirmation retains unified behavior

**Requirement:** A successful probe resets consecutive failures and schedules confirmation at the initial interval; promotion still requires both configured success count and stable time, and a confirmation failure re-enters the selected fixed or exponential failure calculation without moving production traffic before promotion.

**Verification Steps:** Drive partial confirmation, completed confirmation, and confirmation failure in both modes while observing target, delay, snapshot selection, and emitted transition events.

**Pass Conditions:** Confirmation scheduling and promotion gates match the requirement; fixed failure returns to initial delay, exponential failure doubles from initial unless it advances, and Fallback remains selected until one promotion event.

**Fail Conditions:** Success promotes early, confirmation uses maximum, failure bypasses the selected strategy, traffic leaves Fallback early, or notifications duplicate.

**Required Evidence:** Success/failure handler source, named confirmation tests in both modes, snapshot/event assertions, and focused test output.

### AC-07: Reload compares effective recovery policy

**Requirement:** A backoff-mode change, an exponential maximum change, or a change to initial delay, required successes, stable time, rotation attempts, ordered Primary names, or Fallback name is incompatible with state inheritance; fixed-mode configurations differing only in maximum are compatible because maximum is unused; incompatible preparation leaves the old timer, generation, and in-flight ownership untouched until cutover.

**Verification Steps:** Prepare reload transfers from nontrivial pending-timer and in-flight snapshots while changing each named identity field independently, including maximum once in each mode, then exercise compatible commit/rollback and incompatible staged-build failure.

**Pass Conditions:** Mode, exponential maximum, initial, successes, stable time, and rotation-attempt changes return `recovery_policy_changed`; Primary and Fallback changes return their existing deterministic mismatch reasons; none inherit state; fixed maximum-only change transfers complete state; failed incompatible cutover leaves old recovery work authoritative.

**Fail Conditions:** Raw struct equality resets fixed state for an unused maximum, an effective change inherits old delay semantics, or incompatible preparation pauses or detaches old work.

**Required Evidence:** Effective identity-comparison source, named reload tests for each case and ownership state, and focused race-test output.

### AC-08: Logs and documentation describe actual scheduling

**Requirement:** Rotation logs report `next_probe_in` equal to the initial delay after advancement, same-candidate failure logs report the selected strategy's actual delay, and source documentation no longer claims that capped delay persists across candidates or that maximum causes rotation.

**Verification Steps:** Capture structured logs from deterministic fixed and exponential transitions, then search configuration descriptions, examples, comments, and failover documentation for stale semantics.

**Pass Conditions:** Log fields equal scheduler-observed delays and documentation explicitly separates delay strategy, maximum cap, attempt threshold, and per-candidate reset.

**Fail Conditions:** Logs report the pre-reset delay, documentation is ambiguous about maximum-triggered rotation, or stale cross-candidate-cap claims remain.

**Required Evidence:** Logging source, named log-capture tests and output, and changed documentation locations with stale-description search results.

### AC-09: Both modes share one recovery state machine

**Requirement:** Fixed and exponential modes use the existing single `FailoverController` timer, probe, failure, confirmation, promotion, notification, snapshot, and reload-ownership paths; mode selection is confined to validated configuration and one pure delay calculation, with no second controller, strategy interface, duplicated probe loop, or mode-specific transition pipeline.

**Verification Steps:** Inspect the production construction and controller call graph, identify the single point where backoff mode affects delay, and compare fixed/exponential state-machine tests to confirm they drive the same transition handlers and scheduler ownership.

**Pass Conditions:** One controller implementation owns both modes, the mode-dependent runtime decision is limited to the pure delay helper, and every other transition path is shared.

**Fail Conditions:** Fixed mode has a separate controller or probe loop, introduces a strategy interface, bypasses shared confirmation/reload/notification handling, or duplicates timer ownership.

**Required Evidence:** Controller construction and transition source locations, delay-helper call site, repository search for alternative mode-specific controllers or loops, and named tests showing both modes traverse shared handlers.

### AC-10: Invalid reload configuration cannot disturb the active controller

**Requirement:** If a staged reload contains an invalid backoff value, malformed duration, or invalid exponential timing range, replacement construction fails before failover reload transfer preparation and leaves the active controller's generation, pending timer, in-flight probe ownership, recovery target, counters, and ability to continue recovery unchanged.

**Verification Steps:** Start an old controller with a nontrivial pending-timer snapshot and with an in-flight-probe snapshot, stage each invalid configuration category through the production reload build path, then release or fire the old work.

**Pass Conditions:** Every staged build fails before cutover, old ownership fields remain unchanged by the failed build, and exactly one old-generation recovery path continues afterward.

**Fail Conditions:** Invalid replacement input pauses, cancels, detaches, duplicates, or mutates old recovery work, or reaches control-plane cutover.

**Required Evidence:** Reload orchestration source showing build-before-transfer ordering, named production-path failure tests for pending and in-flight states, before/after ownership assertions, and focused race-test output.

### AC-11: Initial probe delay must be positive in both modes

**Requirement:** `recovery_probe_initial` must be greater than zero for fixed and exponential modes so fresh, confirmation, failed, and post-advance scheduling cannot receive a zero or negative configured interval.

**Verification Steps:** Validate and build fixed and exponential groups with positive, zero, and negative initial durations through the production configuration path.

**Pass Conditions:** Both modes accept positive initial duration and reject zero and negative values before controller construction with a field-specific diagnostic.

**Fail Conditions:** Either mode accepts zero or negative initial duration, rejects a valid positive duration, or reaches timer scheduling with an invalid interval.

**Required Evidence:** Mode-aware recovery validation source, named table tests covering both modes and three value classes, and focused test output.

# Rollout Acceptance

### RA-01: Backward-compatible binary activation

**Requirement:** The new binary must accept an existing configuration that omits `recovery_probe_backoff` and retain exponential behavior before any production configuration opts into fixed mode.

**Verification Steps:** Build and test the Linux target, validate the current live configuration with the candidate binary, record a real-client canary domain and its expected outbound before activation, deploy with binary rollback protection, and verify service, DNS listeners, failover BPF slots, and the recorded real LAN client path.

**Pass Conditions:** Validation succeeds without adding the field, the service runs the recorded candidate version and hash, all required BPF slots are alive, and real client traffic succeeds through the intended outbound.

**Fail Conditions:** Existing configuration is rejected, omission selects fixed behavior, activation loses listeners or BPF connectivity, or real client traffic fails.

**Required Evidence:** Linux test output, candidate version/hash, live validation output, binary backup path, recorded canary/expected outbound, service/listener/BPF checks, and matching real-client log evidence.

### RA-02: Source rollout does not silently opt production into fixed mode

**Requirement:** Implementing or deploying the backward-compatible binary must not add `recovery_probe_backoff: fixed` to any live production group without a separate explicit user authorization that names the target groups; omission must continue to mean exponential.

**Verification Steps:** Inspect the live configuration before and after binary rollout, verify selected behavior for groups that omit the field through candidate validation/default tests, and inspect the operation record for separate authorization of any live fixed-mode configuration change.

**Pass Conditions:** Binary rollout leaves live group configuration unchanged unless separately authorized, omitted fields retain exponential mode, and any later fixed-mode opt-in is treated as a new targeted production transaction rather than an implicit implementation step.

**Fail Conditions:** The implementation silently changes live groups to fixed, omission selects fixed, or a production configuration mutation is bundled without named authorization and its own backup/validation/rollback procedure.

**Required Evidence:** Redacted live configuration diff or hash comparison across binary rollout, default-mode test evidence, and operation log showing either no config mutation or a separately authorized targeted transaction.

<!-- ACCEPTANCE-END -->
