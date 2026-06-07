# Priority Failover Policy Design

## Goal

Add an event-driven `failover` dialer selection policy for a single primary
node and a single fallback node.

The policy must:

- Use the primary node during normal operation.
- Switch both new TCP connections and new UDP sessions to the fallback node
  after dae confirms that the primary node's TCP path is unavailable.
- Continue serving traffic through the fallback node while probing only the
  primary node for recovery.
- Switch new TCP connections and new UDP sessions back to the primary only
  after stable TCP recovery is confirmed.
- Avoid periodic probes while the primary node is healthy.
- Avoid reloads, service restarts, configuration rewrites, and forced
  termination of established connections during failover or failback.

This feature does not attempt connection migration. Existing TCP connections
and UDP endpoints continue using the dialer selected when they were created.

## Non-goals

The first version does not support:

- More than one primary node or more than one fallback node.
- Nested dialer groups.
- More than two priority levels.
- UDP failures as a failover trigger.
- Persistent failover state across process restarts.
- Load balancing inside either priority level.
- Latency-based selection or moving-average comparison.
- Automatic routing between separate outbound groups.

## Configuration

Example:

```dae
group {
    proxy {
        filter: name(main-node) [priority: 0]
        filter: name(fallback-node) [priority: 1]

        policy: failover

        recovery_probe_initial: 15s
        recovery_probe_max: 5m
        recovery_successes: 3
        recovery_stable_time: 30s
    }
}
```

### Priority semantics

- `priority: 0` identifies the primary node.
- `priority: 1` identifies the fallback node.
- Priority determines the role. Node names and filter declaration order do
  not determine the role.

After all filters are evaluated, a `failover` group must contain exactly two
distinct dialers:

- Exactly one dialer annotated with `priority: 0`.
- Exactly one dialer annotated with `priority: 1`.

Configuration validation must fail when:

- The group contains fewer or more than two dialers.
- A selected dialer has no priority annotation.
- A priority is duplicated.
- A priority other than `0` or `1` is used.
- The same underlying dialer would occupy both roles.
- A duration is zero or negative.
- `recovery_probe_initial` is greater than `recovery_probe_max`.
- `recovery_successes` is less than one.

Suggested defaults:

```text
recovery_probe_initial = 15s
recovery_probe_max     = 5m
recovery_successes     = 3
recovery_stable_time   = 30s
```

`priority` is a filter annotation consumed by `policy: failover`. Existing
selection policies retain their current behavior.

## Health Authority

The failover controller must not parse log output and must not introduce a
second independent failure counter.

The authoritative failure signal is dae's existing TCP dialer health
transition:

```text
TCP alive -> TCP not alive
```

This transition already incorporates dae's existing active-check,
traffic-failure, forced-unavailable, and error-classification behavior.

UDP health transitions and UDP packet write errors must never switch the
group from primary to fallback.

The failover state is group-wide. Once the primary TCP path is confirmed
unavailable:

- New TCP connections select the fallback.
- New UDP sessions select the fallback.

TCP and UDP do not maintain separate active roles.

## State Machine

The group has four logical states.

### `primary_active`

- New TCP connections and UDP sessions use the primary.
- No failover recovery timer or periodic health-check ticker runs.
- The policy observes TCP availability transitions produced by real traffic
  and existing explicit failure reporting.
- Creating a failover group must not activate dae's ordinary periodic
  latency/health checking merely because the policy exists.
- A confirmed primary TCP transition to unavailable enters
  `fallback_active`.

### `fallback_active`

- New TCP connections and UDP sessions use the fallback.
- Existing primary connections and UDP endpoints are not terminated.
- One recovery probe schedule is started for the primary.
- The first recovery probe runs after `recovery_probe_initial`.
- A failed recovery probe resets recovery success state and exponentially
  increases the next probe delay, capped by `recovery_probe_max`.
- A successful recovery probe enters `recovering`.

### `recovering`

- New traffic continues using the fallback.
- Only the primary is probed for recovery.
- Successful probes increment a consecutive-success counter.
- The first successful probe records the recovery stability start time.
- Any failed probe resets the success counter and stability start time,
  increases the probe backoff, and returns to `fallback_active`.
- Failback occurs only when both conditions are true:
  - Consecutive successes are at least `recovery_successes`.
  - Time since the first success is at least `recovery_stable_time`.

When both conditions are met, the group enters `primary_active`.

### `fallback_unavailable`

This is an observable condition rather than an independently selected role:

- The primary remains the failed role.
- The fallback is selected because it is the configured active role.
- If dialing the fallback fails, the connection returns an error.
- The group must not loop indefinitely between primary and fallback.
- Primary recovery probing continues.

The fallback does not preemptively trigger a return to an unconfirmed primary.

## Selection and Retry Behavior

### Ordinary selection

```text
primary_active  -> select primary
fallback_active -> select fallback
recovering      -> select fallback
```

The policy must respect the existing `excluded` dialer argument.

### Failure of the selected primary

When an ordinary TCP dial attempt produces an error that dae already
classifies as a proxy dialer failure:

1. Existing dae logic updates the primary TCP health state.
2. Once the canonical TCP health state becomes unavailable, the group
   atomically enters `fallback_active`.
3. The current connection retry excludes the failed primary.
4. Selection returns the fallback.

Only one state transition and one recovery schedule may be created when many
connections fail concurrently.

The failover controller listens to the primary dialer's TCP availability
transition callback. It does not require an `AliveDialerSet` to select the
primary or fallback, and it must not use creation of an `AliveDialerSet` as a
side effect to activate periodic checks.

### Failure of the fallback

The fallback is not promoted to another level because no third level exists.
The current operation returns the actual dial error after the existing bounded
retry behavior. Recovery probing of the primary remains active.

### UDP behavior

UDP does not trigger failover. However, after TCP has switched the group role:

- Existing UDP endpoints remain attached to their original dialer.
- New UDP endpoints use the fallback.
- After failback, newly created UDP endpoints use the primary.

The failover policy must not use the existing data-UDP-to-DNS-UDP/TCP health
domain fallback as a signal to change the group role.

## Recovery Probing

Recovery probing starts only after entering `fallback_active`.

Probe delays follow an exponential sequence:

```text
initial, initial * 2, initial * 4, ... max
```

For the default configuration:

```text
15s, 30s, 60s, 120s, 240s, 300s, 300s, ...
```

Requirements:

- Reuse the primary dialer's existing TCP health-check implementation and
  configured TCP check target.
- Do not create a separate socket-check implementation.
- Expose or reuse a cancellable one-shot TCP probe operation. Calling it must
  not start the dialer's ordinary periodic health-check ticker.
- Do not probe the fallback periodically for this policy.
- Ensure at most one pending recovery timer and one active recovery probe per
  failover group.
- Group shutdown and control-plane replacement must cancel pending timers and
  probes.
- A stale probe from an old group generation must not change the new group's
  state.

After a failed probe, the next delay follows the exponential backoff. After
the first successful probe:

- The failure backoff resets.
- Confirmation probes run every `recovery_probe_initial`.
- A failure during confirmation starts failure backoff again at
  `recovery_probe_initial * 2`, capped by `recovery_probe_max`.

Successful recovery probes are evidence for failback, but the primary remains
ineligible for ordinary traffic until the recovery thresholds are satisfied.
This deliberately separates:

```text
TCP reachable
```

from:

```text
eligible to become the active primary again
```

## Reload and Restart Semantics

### Warm reload

When a reload contains the same primary and fallback dialer identities:

- Inherit the active role.
- Inherit primary and fallback health snapshots using the existing mechanism.
- Inherit the recovery success count and stability start time.
- Inherit the current recovery backoff level.
- Re-arm the next recovery probe with the remaining delay.

When either role resolves to a different dialer identity, initialize the new
group in `primary_active` and discard the previous failover controller state.

Reload inheritance must not temporarily select the primary when the old
generation was using the fallback.

### Process restart

A full process restart initializes the group in `primary_active`. Persistent
failover state is outside the first-version scope.

If the primary is still unavailable, existing dae health or the first real
connection failure will move the group back to `fallback_active`.

## Concurrency Model

Selection is on a hot path and should read an immutable or atomic state
snapshot without taking a long-lived mutex.

State transitions, recovery counters, timers, and generation changes may use
a narrowly scoped mutex. The design must guarantee:

- One winner for a concurrent primary-to-fallback transition.
- One recovery timer per group.
- No duplicate failback transition.
- No stale timer mutation after close or reload.
- No blocking network operation while holding the state mutex.

## Logging and Observability

Emit one structured informational log for each role transition:

```text
failover_switch
  group=proxy
  from=primary
  to=fallback
  trigger=tcp_unavailable
  primary=<node>
  fallback=<node>

failback_start
  group=proxy
  primary=<node>
  successes=1

failback_complete
  group=proxy
  from=fallback
  to=primary
  successes=3
  stable_for=30s
```

Recovery probe failures should be debug-level or rate-limited informational
logs. They must not produce one warning per connection.

No Bark-specific behavior belongs in dae. External monitoring may consume
these structured logs later.

## Compatibility

- Existing `fixed`, `random`, `min`, `min_avg10`, and `min_moving_avg`
  policies must remain unchanged.
- Existing configurations without `policy: failover` must parse and behave
  exactly as before.
- Routing `fallback:` remains unrelated and unchanged.
- DNS routing fallback behavior remains unrelated and unchanged.
- Existing eBPF routing contracts and outbound IDs remain unchanged.

## Implementation Boundaries

Expected areas of change:

- `config.Group`
  - Recovery policy fields.
- `dialer.Annotation`
  - Integer priority annotation.
- `DialerSelectionPolicy`
  - New `failover` policy value and validation.
- `DialerGroup`
  - Primary/fallback role mapping.
  - Atomic active-role selection.
  - Recovery state machine and lifecycle.
- Existing dialer health callbacks
  - Feed canonical primary TCP transitions into the group controller.
- Existing TCP connectivity check
  - Provide a cancellable one-shot probe path without activating the periodic
    ticker.
- Reload health inheritance
  - Copy failover-controller state when role identities match.

The design should not add group-to-group references.

## Test Strategy

### Parser and validation tests

- Valid priorities `0` and `1`.
- Missing priority.
- Duplicate priority.
- Unsupported priority.
- One or three selected nodes.
- Invalid recovery durations and success count.
- Existing policies parse unchanged.

### Selection unit tests

- Initial TCP and UDP selection uses primary.
- Confirmed primary TCP failure switches both TCP and UDP selection.
- UDP failure alone does not switch roles.
- Selected primary exclusion returns fallback.
- Fallback exclusion returns an error rather than primary while primary is
  unconfirmed.
- Concurrent failures create one transition and one recovery schedule.

### Recovery tests with a fake clock

- Probe delays follow exponential backoff and respect the maximum.
- One successful probe is insufficient.
- Consecutive successes without sufficient stable time are insufficient.
- Stable time without enough successes is insufficient.
- A failure during recovery resets success and stability state.
- Completing both conditions switches new TCP and UDP selections to primary.
- Sustained primary health causes no failover-specific periodic probes.
- Constructing and using a healthy failover group does not activate ordinary
  periodic latency checks.

### Lifecycle and reload tests

- Close cancels recovery timer and active probe context.
- Stale callbacks cannot mutate a replacement group.
- Reload with identical role identities preserves fallback state.
- Reload preserves recovery progress and remaining delay.
- Reload with changed identities initializes primary state.

### Integration tests

- A real primary dial failure retries the fallback within the existing bounded
  connection retry path.
- Established fallback TCP connections survive failback.
- Existing UDP endpoint remains on fallback while a new endpoint uses primary
  after failback.
- No reload or service restart is required for either transition.

## Acceptance Criteria

The feature is complete when:

1. A valid two-node priority configuration starts successfully.
2. Normal operation adds no failover-specific periodic probes.
   It also does not activate dae's ordinary periodic checker solely because
   the group uses `policy: failover`.
3. Confirmed primary TCP unavailability switches new TCP and UDP traffic to
   the fallback without reload or restart.
4. UDP failure alone never changes the active role.
5. While using the fallback, only the primary receives recovery probes with
   bounded exponential backoff.
6. A transient primary success does not trigger failback.
7. Stable recovery switches only new TCP and UDP traffic back to the primary.
8. Existing connections and UDP endpoints are not forcibly migrated.
9. Warm reload preserves a valid in-progress failover state.
10. All existing policy and routing tests remain green.
