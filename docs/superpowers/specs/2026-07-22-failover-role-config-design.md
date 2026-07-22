<!-- IMPLEMENTATION-SPEC-BEGIN -->

# Goal

Replace priority-annotation-based failover role configuration with explicit,
ordered `primary` and fixed `fallback` fields while keeping one runtime model
for both single-Primary and multi-Primary groups.

The target configuration is:

```dae
proxy_failover {
    primary: name(A, B, C)
    fallback: name(X)

    policy: failover
    primary_rotation_attempts: 5
}
```

`primary` parameter order is authoritative: `A` is the initial Primary after a
fresh process start, and failed recovery targets rotate circularly through
`A`, `B`, and `C`. `fallback` is a single fixed dialer and never participates
in Primary rotation.

The implementation must remove the old failover contract based on
`filter: ... [priority: N]`. Backward compatibility is intentionally out of
scope because this gateway is the only known consumer and retaining two role
models would add permanent parser, validation, and runtime complexity.

# Non-Goals

- Do not infer `policy: failover` from the presence of `primary` or
  `fallback`; the policy remains explicit.
- Do not support keyword, regex, `subtag`, negation, chained filter functions,
  or other general filtering expressions in failover role fields.
- Do not persist the current Primary index or recovery counters. A process
  restart always begins at `primary[0]`.
- Do not reject a reload merely because failover topology or recovery policy
  changed. Incompatible reloads reset failover state instead.
- Do not add partial reload-state migration rules for changed recovery
  thresholds or timing parameters.
- Do not change the fixed-fallback traffic contract: an unavailable current
  Primary sends traffic to `fallback`; recovery candidates do not directly
  carry production traffic until recovery confirmation promotes one.
- Do not generalize this role syntax to non-failover policies in this change.

# Architecture

## Configuration model

`config.Group` gains policy-specific `Primary` and `Fallback` values capable of
holding the existing function syntax. The only accepted semantic form is one
non-negated `name(...)` function per field:

```dae
primary: name(A, B, C)
fallback: name(X)
```

The generic `filter` path remains available to non-failover policies. A group
using `policy: failover` must use the role fields and must not contain any
parsed filter expression. Conversely, non-failover policies must reject
`primary` and `fallback` so the fields can never be silently ignored. A
syntactically empty `filter` that the parser rejects or normalizes to no parsed
expression does not create a separate failover semantic case.

## Ordered exact-name resolution

Failover roles use a dedicated resolver on `outbound.DialerSet`. The resolver
iterates the parameters in `primary: name(...)` order and resolves each exact
dialer name independently. It must not use `FilterAndAnnotate`, whose result
order follows the global dialer pool rather than function parameter order.

The resolver produces one ordered dialer slice containing all Primary
candidates followed by the fixed Fallback, plus a `FailoverConfig` whose
Primary candidate indexes cover the ordered prefix and whose Fallback index is
the final element. The runtime therefore remains index-based without retaining
priority annotations.

## Unified controller model

Every failover controller owns the same structural model regardless of
candidate count:

```text
primaryCandidates []Dialer
currentPrimary    int
recoveryTarget    int
fallback          Dialer
failedAttempts    int
```

The list must contain at least one Primary. Index advancement always uses:

```text
(index + 1) % len(primaryCandidates)
```

With one element, advancement naturally selects the same Primary. No
single-Primary controller variant or cardinality branch is permitted.

## Reload inheritance boundary

Warm reload keeps the existing transactional state-transfer boundary. Its
identity consists of:

- ordered Primary dialer names;
- fixed Fallback dialer name; and
- the complete `FailoverRecoveryConfig`, including rotation attempts and all
  probe/confirmation timing fields.

An exact identity match transfers the complete controller snapshot. Any
identity difference transfers nothing: the replacement controller remains in
its fresh state with `currentPrimary == 0` and the new configuration still
takes effect if the reload commits. A mismatch does not fail the reload.

# Detailed Design

## Role syntax and validation

For `policy: failover`:

1. `primary` is required and must be exactly one non-negated `name(...)`
   function with at least one unkeyed, non-empty parameter.
2. `fallback` is required and must be exactly one non-negated `name(...)`
   function with exactly one unkeyed, non-empty parameter.
3. Parameters with keys such as `keyword:` or `regex:` are invalid.
4. Function names other than `name`, multiple/chained functions, string-only
   values, and function negation are invalid.
5. Every requested exact name must resolve to exactly one dialer in the source
   pool. Zero matches and multiple matches are both configuration errors.
6. Primary names must be unique and must resolve to distinct dialer objects.
7. The Fallback must not duplicate a Primary name or resolve to the same
   dialer object as any Primary.
8. `filter` is forbidden in a failover group. Consequently the legacy
   priority-annotation form is rejected rather than translated.
9. `primary_rotation_attempts` must be non-negative. Candidate cardinality
   does not constrain it: zero or a positive value is valid with either one or
   many Primary candidates.

The generic `priority` filter annotation is removed from the active annotation
model because it no longer has a supported consumer. `add_latency` and generic
filter behavior for other policies remain unchanged.

## Construction flow

`control.NewControlPlane` parses the explicit policy before resolving nodes.
For a failover policy it must:

1. reject any parsed `filter` expression;
2. parse and validate the `primary` and `fallback` role functions;
3. resolve exact names with the dedicated ordered resolver;
4. apply any existing group-level dialer option cloning without changing the
   resolved order;
5. build `FailoverConfig` from the ordered role result; and
6. create the existing `DialerGroup` and `FailoverController` with that
   configuration.

For every other policy it follows the current `FilterAndAnnotate` flow and
rejects either failover role field if present.

## Recovery and rotation semantics

A fresh controller starts with:

```text
currentPrimary = 0
recoveryTarget = 0
failedAttempts = 0
state = primary_active
```

When the current Primary becomes unavailable, traffic switches immediately to
the fixed Fallback. The controller starts serial recovery probing against
`primaryCandidates[recoveryTarget]`, initially the failed current Primary.

For a failed recovery probe:

1. clear that target's recovery-success confirmation state;
2. increment `failedAttempts`;
3. maintain the existing exponential backoff behavior; and
4. when `primary_rotation_attempts > 0` and `failedAttempts` reaches the
   threshold, advance `recoveryTarget` by one modulo the candidate count and
   reset `failedAttempts` to zero.

The threshold is per candidate and counts consecutive failed probes. Each new
target receives a full attempt budget. This replaces the previous
episode-total behavior in which, after rotation first activated, every later
failure advanced immediately.

For a successful recovery probe, reset `failedAttempts` to zero because the
failure sequence is no longer consecutive. Continue the existing
`recovery_successes` and `recovery_stable_time` confirmation behavior against
the same target. After confirmation succeeds, promote `recoveryTarget` to
`currentPrimary`, return traffic from Fallback to that candidate, and reset the
episode state.

When `primary_rotation_attempts == 0`, the advancement condition is disabled.
The controller continues serially probing the current recovery target forever
using the existing backoff and confirmation rules. Zero is valid even with
multiple candidates; the remaining candidates simply cannot be selected by
rotation. With one candidate and a positive threshold, modulo advancement
selects that same candidate and starts a fresh per-candidate attempt budget.

Primary failure never directly sends production traffic to the next Primary
candidate. The fixed Fallback always carries traffic until a candidate passes
recovery confirmation.

## In-memory lifetime

`currentPrimary`, `recoveryTarget`, counters, confirmation state, timers, and
probe ownership remain memory-only. Normal operation can promote `B` or `C`
and continue from that candidate after later failures, but a full process
restart always initializes from the first configured Primary.

## Reload behavior

On an exact reload-identity match, the replacement controller inherits the
current Primary, recovery target, consecutive failure count, confirmation
state, remaining backoff, timer state, and in-flight-probe ownership through
the existing transactional transfer. Candidate identities are remapped by
name, never by old slice indexes or pointers.

On any identity mismatch, including a Primary addition, removal, reorder,
Fallback change, `primary_rotation_attempts` change, or recovery timing change:

- do not pause or mutate the old controller during inheritance preparation;
- return no transfer transaction;
- allow the reload to continue;
- leave the replacement controller at its new `primary[0]`; and
- retain the existing structured reset log with a deterministic mismatch
  reason.

If the staged reload later fails, the untouched old controller continues with
its original timer or in-flight probe. If an identity-compatible staged reload
fails, the existing rollback transaction invalidates replacement work and
resumes exactly one old-generation probe. Late results from detached probes
must continue to be ignored through generation and logical-ownership checks.

## Documentation and migration

Update the configuration descriptions, `example.dae`, failover tests, and
operator-facing failover documentation to use explicit roles. Examples must
show both a single-Primary configuration and an ordered multi-Primary
configuration. Remove claims that priority 0/1/2+ define failover roles.

Production migration is a coordinated binary-and-configuration restart, not a
reload. The new binary intentionally rejects the old configuration, and the
old binary does not understand the new role fields.

# Error Handling

Configuration errors must identify the group, role, and offending value where
available. Required error categories are:

- missing `primary` or `fallback` for `policy: failover`;
- `filter` used with `policy: failover`;
- `primary` or `fallback` used with a non-failover policy;
- wrong function name, negation, chained functions, keyed parameters, empty
  parameters, or incorrect role cardinality;
- an unknown exact dialer name;
- an exact name resolving to multiple dialers;
- a duplicate Primary or Primary/Fallback overlap; and
- a negative `primary_rotation_attempts`.

Errors occur while building the replacement control plane, before cutover. A
reload-time configuration error leaves the current control plane active.

Reload identity mismatch is not a configuration error. It is an observable
state reset, logged at info level, and must not mutate the old controller
before the new control plane commits.

# Testing Strategy

## Parser and validation tests

- Parse the documented single-Primary and multi-Primary examples.
- Prove `name(C, A, B)` resolves in exactly that order even when the global
  dialer pool is `[A, B, C, X]` or is generated in another map order.
- Table-test every invalid syntax and resolution category from Error Handling.
- Prove the old `filter + [priority: N]` failover syntax is rejected.
- Prove generic non-failover filters and `add_latency` annotations are
  unaffected.

## State-machine tests

- Exercise one candidate and multiple candidates through the same constructor
  and failure handler.
- Verify threshold zero never advances with one or many candidates.
- Verify each candidate receives the complete positive attempt budget, an
  advance resets the per-candidate counter, and the final candidate wraps to
  index zero.
- Verify a successful probe resets the consecutive failure counter before
  recovery confirmation completes.
- Verify traffic remains on the fixed Fallback throughout probing and changes
  only after promotion.
- Verify promotion updates the current Primary and a later failure begins from
  that candidate.
- Verify a new controller always starts at candidate zero.

## Reload and concurrency tests

- Preserve a nontrivial compatible snapshot, including a nonzero candidate
  index, pending timer, counter, and confirmation state.
- Change each reload-identity category independently and verify the new
  controller starts fresh while the old controller remains untouched until
  cutover.
- Exercise compatible reload commit and rollback with both pending and
  in-flight probes.
- Complete an old physical probe after reload and prove it cannot mutate the
  new controller, clear new ownership, or count as a recovery failure.

## Validation commands

Run Linux tests in the repository's OrbStack environment:

```bash
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./config ./component/outbound ./control ./cmd
orb env GOCACHE=/tmp/dae-go-cache go test -tags dae_stub_ebpf ./...
```

Run formatting and repository checks for all modified Go and documentation
files before commit.

<!-- IMPLEMENTATION-SPEC-END -->

<!-- ACCEPTANCE-BEGIN -->

# Completion Contract

Implementation is complete only when every Acceptance Criterion and every
Rollout Acceptance check is independently reported PASS with its required
evidence. Aggregate test success alone is insufficient. The old priority-based
failover configuration must not remain as a supported or documented path.

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

### AC-01: Explicit role syntax replaces priority failover

**Requirement:** A failover group accepts one explicit `primary: name(...)`, one explicit `fallback: name(...)`, and explicit `policy: failover`; it rejects the legacy `filter: ... [priority: N]` role format without translating it.

**Verification Steps:** Parse and build the documented role configuration; parse and build representative two-node and rotating legacy priority configurations; then test role fields with omitted policy and with an explicitly non-failover policy.

**Pass Conditions:** The role configuration builds successfully; both legacy configurations fail before control-plane cutover with a failover-specific configuration error; omitted or non-failover policy never causes failover policy inference.

**Fail Conditions:** The new syntax fails, policy is inferred, legacy priority configuration succeeds, or legacy roles are silently normalized.

**Required Evidence:** Config struct and control-plane source locations, named parser/build tests, and focused test output.

### AC-02: Primary parameter order is authoritative

**Requirement:** `primary: name(C, A, B)` resolves to candidates `[C, A, B]` regardless of global dialer-pool order; `C` is candidate index zero after a fresh controller construction.

**Verification Steps:** Build the same role expression against at least two differently ordered global dialer pools and inspect the resolved candidate names and initial controller snapshot.

**Pass Conditions:** Both cases produce `[C, A, B]` and start from `C`.

**Fail Conditions:** Either result follows pool order, sorting order, map iteration order, or starts from another candidate.

**Required Evidence:** Dedicated resolver source, named order tests, and output showing both input pool orders and resolved candidate order.

### AC-03: Role expressions are exact and unambiguous

**Requirement:** Primary and Fallback each use one non-negated `name(...)` function containing unkeyed non-empty exact names; every name resolves to one dialer, Primary entries are distinct, and Fallback is distinct from every Primary.

**Verification Steps:** Table-test wrong functions, negation, chained functions, keyword and regex keys, empty values, missing names, multiply resolved names, duplicate Primaries, and Primary/Fallback overlap.

**Pass Conditions:** Every invalid case fails with an error identifying its role and cause; valid exact-name cases succeed.

**Fail Conditions:** Any invalid case succeeds, produces a panic, silently drops a name, or emits an error that cannot identify the role and failure category.

**Required Evidence:** Validation source, table-test names/cases, and focused test output.

### AC-04: Failover role cardinality is enforced

**Requirement:** Primary contains one or more names and Fallback contains one name; candidate count does not impose additional restrictions on non-negative `primary_rotation_attempts`.

**Verification Steps:** Test empty Primary, one and multiple Primaries, empty Fallback, multiple Fallback names, rotation attempts zero and five for both one and multiple Primaries, and rotation attempts negative one.

**Pass Conditions:** Invalid cardinalities and negative rotation attempts fail; all four valid cardinality/attempt combinations build successfully.

**Fail Conditions:** An invalid cardinality or negative rotation attempt succeeds, or a valid single/multiple candidate configuration is rejected because of its rotation threshold.

**Required Evidence:** Named validation tests and focused output listing every cardinality/threshold combination, including the negative case.

### AC-05: One state machine handles every candidate count

**Requirement:** Controller construction and recovery use one Primary slice and modulo index advancement for one or many candidates, without a separate single-Primary controller or failure path.

**Verification Steps:** Inspect controller construction and advancement source, then run the same state-machine test harness with candidate counts one and three.

**Pass Conditions:** Both cases use the same functions and state fields; advancing the single candidate selects index zero and advancing three candidates follows the circular order.

**Fail Conditions:** Single-candidate behavior is dispatched to a separate controller/path, modulo advancement is absent, or either candidate count produces an invalid index.

**Required Evidence:** Controller source lines, shared test harness name, and snapshots showing index sequences.

### AC-06: Rotation uses per-candidate consecutive failure budgets

**Requirement:** With `primary_rotation_attempts: 5`, each recovery target advances only after five consecutive failed probes; advancement resets the failure counter, and a successful probe resets the counter even before final promotion.

**Verification Steps:** Drive A through four failures, one success, then five failures; drive B through five failures; record target and counter after every probe.

**Pass Conditions:** A remains selected through the first four failures and after success; the post-success failures count from one, A advances to B only on the fifth; B receives five failures before advancing; each advance resets the counter.

**Fail Conditions:** Episode-total failures trigger advancement, any post-activation failure advances immediately, success preserves the consecutive counter, or a target receives less than its full budget.

**Required Evidence:** Named deterministic scheduler test and probe-by-probe target/counter history.

### AC-07: Rotation threshold zero disables advancement

**Requirement:** With `primary_rotation_attempts: 0`, failed probes never change `recoveryTarget` for either one or multiple configured Primaries, while probing, backoff, and successful recovery remain active.

**Verification Steps:** Run more failed probes than a representative positive threshold against one- and three-candidate controllers, then provide successful confirmation probes.

**Pass Conditions:** The recovery target never changes during failures; backoff and scheduling continue; successful confirmation promotes the same target.

**Fail Conditions:** The target advances, probing stops, the controller leaves Fallback without confirmation, or zero is rejected during validation.

**Required Evidence:** Named state-machine tests, target histories, timer/backoff observations, and focused output.

### AC-08: Fixed Fallback carries traffic until promotion

**Requirement:** Current-Primary failure immediately selects the fixed Fallback; candidate probing and cursor movement never select an unconfirmed candidate for production traffic; only completed recovery confirmation promotes the target.

**Verification Steps:** Observe active selection after initial failure, after multiple candidate advances, after partial successes, and after final confirmation.

**Pass Conditions:** Active selection is Fallback in every intermediate state and changes to the confirmed target only at promotion.

**Fail Conditions:** Failure directly selects the next Primary, probing selects a candidate, partial confirmation changes traffic, or Fallback joins the rotation list.

**Required Evidence:** Named integration test, active-selection snapshots for every state, and source locations for selection publication.

### AC-09: Promotion determines the next recovery episode's starting target

**Requirement:** After candidate B or C completes recovery confirmation and becomes `currentPrimary`, a later failure begins recovery probing from that promoted current Primary rather than resetting `recoveryTarget` to configured index zero.

**Verification Steps:** Promote a nonzero candidate, trigger a new current-Primary failure, and inspect the active selection, `currentPrimary`, `recoveryTarget`, and first probe target for the new episode.

**Pass Conditions:** Traffic switches to the fixed Fallback, the promoted candidate remains `currentPrimary`, `recoveryTarget` is initialized from that index, and the first recovery probe targets that promoted candidate.

**Fail Conditions:** The new episode probes configured `primary[0]`, changes `currentPrimary` before confirmation, skips the fixed Fallback, or begins from any candidate other than the one that just failed.

**Required Evidence:** Named deterministic state-machine test, promotion and second-failure snapshots, and captured first-probe target.

### AC-10: Process lifetime state remains memory-only

**Requirement:** Promotion may change the in-memory current Primary, but constructing a fresh controller from the same configuration always starts at `primary[0]`; no persistent rotation state is read or written.

**Verification Steps:** Promote a nonzero candidate, construct a fresh controller from the same resolved roles, and search production code for persistence of failover indexes or counters.

**Pass Conditions:** The running controller uses the promoted candidate, the fresh controller uses candidate zero, and no persistence path exists.

**Fail Conditions:** A fresh controller resumes the promoted index, restart state depends on external storage, or the initial index is not zero.

**Required Evidence:** Named restart-semantics test, before/after snapshots, and persistence-path source audit.

### AC-11: Reload identity is all-or-nothing

**Requirement:** Ordered Primary names, Fallback name, and complete recovery configuration must all match to inherit failover state; any difference allows reload to continue with the replacement controller freshly initialized at its own `primary[0]` and no partial snapshot fields inherited.

**Verification Steps:** Transfer a nontrivial snapshot through an exact match, then independently change candidate membership, order, Fallback, rotation attempts, and each recovery timing/confirmation field.

**Pass Conditions:** The exact match preserves every snapshot field; every mismatch returns no transfer, starts the replacement from its first Primary with fresh counters/timers, and emits the deterministic reset reason.

**Fail Conditions:** A mismatch fails the reload solely because of identity, inherits any old failover field, ignores the new configuration, or mutates the old controller during preparation.

**Required Evidence:** Identity comparison and caller source, table-driven reload tests, structured log capture, and focused output.

### AC-12: Reload probe ownership remains transactional

**Requirement:** Compatible reload commit leaves only the replacement controller owning recovery work; compatible rollback invalidates replacement work and resumes one old probe; incompatible preparation leaves the old timer/probe untouched; detached late probe results mutate neither owner.

**Verification Steps:** Exercise pending-timer and in-flight-probe cases for compatible commit, compatible rollback, and incompatible reset, then complete a detached physical probe after each handoff.

**Pass Conditions:** Each terminal path has one logical probe owner and one expected schedule; stale results do not change target, counters, confirmation, timer ownership, or active selection.

**Fail Conditions:** Probes duplicate, disappear, count as failures after cancellation, clear new ownership, or an incompatible staged reload disables old recovery before commit.

**Required Evidence:** Named concurrency/reload tests, ordered ownership event history, final snapshots, and focused race-safe test output.

### AC-13: Non-failover filtering remains unchanged

**Requirement:** Generic policies continue using `filter` and `add_latency`; they reject failover-only `primary` or `fallback`, and removal of the priority role annotation does not alter ordinary filter matching or ordering behavior.

**Verification Steps:** Run existing filter/annotation tests, add non-failover rejection cases for role fields, and inspect the annotation model after priority removal.

**Pass Conditions:** Existing non-failover behavior passes unchanged, `add_latency` remains supported, and failover-only fields fail instead of being ignored.

**Fail Conditions:** Ordinary filters regress, `add_latency` is removed, priority remains an accepted but unused annotation, or non-failover role fields are silently accepted.

**Required Evidence:** Annotation and control-plane source, existing plus new named tests, and focused output.

### AC-14: Documentation describes only the new contract

**Requirement:** Active configuration descriptions, examples, and failover operator documentation use explicit Primary/Fallback roles, explain exact-name/order/rotation/restart/reload semantics, and do not advertise priority-based failover.

**Verification Steps:** Search active documentation and examples for failover priority claims while excluding only `docs/superpowers/specs/archive/**` and `docs/superpowers/plans/archive/**`, then inspect every new role example and semantic description.

**Pass Conditions:** Active docs contain valid single- and multi-Primary examples and no supported priority-role instructions; archived historical specs may retain old text when clearly archived.

**Fail Conditions:** Active docs instruct users to configure failover priorities, omit the fixed Fallback or ordering semantics, or describe threshold behavior as episode-total/immediate advancement.

**Required Evidence:** Changed documentation paths and line numbers plus repository search output excluding archive directories.

### AC-15: Invalid reload configuration leaves the old control plane active

**Requirement:** If new role syntax, name resolution, overlap, policy use, or rotation-threshold validation fails while building a reload generation, cutover does not begin and the existing control plane, failover controller, timer, probe ownership, counters, and active selection remain unchanged.

**Verification Steps:** Put the old controller into a nontrivial recovering state, attempt reloads containing representative parser and failover validation errors, then advance the old scheduler and complete any old in-flight probe.

**Pass Conditions:** Every invalid reload reports failure before cutover; old state is unchanged at failure time and continues making normal timer/probe progress afterward.

**Fail Conditions:** Invalid configuration reaches cutover, pauses or closes the old controller, loses or duplicates recovery work, changes old state, or leaves the service without an active control plane.

**Required Evidence:** Named command/control-plane reload tests, before/failure/after snapshots, ownership event history, and focused output.

### AC-16: Configuration diagnostics identify actionable context

**Requirement:** Every required configuration-error category identifies the group and cause; role-specific errors identify `primary` or `fallback`; errors involving a supplied function, key, name, duplicate, overlap, filter, policy, or numeric threshold include the offending value when available.

**Verification Steps:** Capture exact errors for missing roles, failover filter use, non-failover role use, wrong/chained/negated functions, keyed and empty parameters, bad cardinality, unknown and multiply resolved names, duplicates, overlap, and a negative rotation threshold.

**Pass Conditions:** Every error includes the group and unambiguous cause, every role-specific case includes the role, and every case with a supplied offending value includes that value; no case panics or reports only a generic build failure.

**Fail Conditions:** Any error omits required context, identifies the wrong role/value, silently succeeds, or panics.

**Required Evidence:** Table of input category to exact emitted error, source locations for wrapping and validation, named tests, and focused output.

# Rollout Acceptance

### RA-01: Coordinated migration is reversible

**Requirement:** The production rollout backs up the currently installed DAE binary and live configuration, validates a staged new-role configuration with the candidate new binary, and treats binary and configuration as one rollback unit.

**Verification Steps:** Record old binary version/hash, backup paths, candidate binary version/hash, staged validation command/output, and the paired rollback commands before activation; restore both backups to isolated verification paths and compare their hashes with the recorded originals without replacing the running artifacts.

**Pass Conditions:** Both backups exist, candidate validation succeeds, isolated restores of both artifacts match the recorded original hashes, and the paired production rollback commands are recorded.

**Fail Conditions:** A sanitized repository config is copied over production, either artifact lacks a backup, validation uses the old binary, either isolated restore differs from its original hash, or the recorded rollback restores only one artifact.

**Required Evidence:** Sanitized deployment transcript containing versions, original/backup/isolated-restore hashes, backup and isolated verification paths, validation result, and paired rollback procedure without credentials or node secrets.

### RA-02: First activation uses restart and starts at primary zero

**Requirement:** The incompatible syntax migration activates through a controlled DAE restart, and the new process resolves the configured ordered candidates with `primary[0]` active and the fixed Fallback correctly identified.

**Verification Steps:** Install the paired binary/configuration, restart DAE, inspect service state and structured startup/failover logs, and verify the resolved group order without inducing a production failover.

**Pass Conditions:** DAE remains active, listeners and BPF state are healthy, and runtime evidence identifies the configured first Primary and fixed Fallback.

**Fail Conditions:** Migration relies on reload, the service fails, candidate order differs, Fallback is missing/rotatable, or rollback is required but not executed.

**Required Evidence:** Service status, installed version, validation result, sanitized group-resolution evidence, and gateway health checks.

### RA-03: Real client path remains healthy

**Requirement:** After activation, a real LAN client can use at least one existing `proxy_failover` route, and gateway observability identifies the expected outbound without exposing credentials.

**Verification Steps:** Generate a request from a real LAN client to an existing failover allowlist destination, correlate it with DAE flow/routing evidence, and inspect recent errors.

**Pass Conditions:** The request succeeds through `proxy_failover`, the expected current Primary is selected, and no new failover-controller, BPF connectivity, DNS, or dial errors appear.

**Fail Conditions:** Only gateway-local curl is tested, the client route bypasses the group, the request fails, or new relevant errors remain unexplained.

**Required Evidence:** Sanitized client result, correlated gateway event/log entry, service status, and recent-error check.

<!-- ACCEPTANCE-END -->
