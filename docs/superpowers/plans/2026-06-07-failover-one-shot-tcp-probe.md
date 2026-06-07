# Failover One-Shot TCP Probe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every failover recovery probe return its own TCP result so consecutive successful probes can reliably reach `recovery_successes`.

**Architecture:** Add a cancellable `Dialer.ProbeTCPOnce(ctx)` API that directly executes the existing TCP check target and `HttpCheck` implementation for IPv4 and IPv6, returning success when either applicable family succeeds. `FailoverController` consumes this result directly instead of inferring it from alive-transition callbacks. Because the one-shot API does not require `aliveBackground`, remove the failover-group reference counter so healthy failover groups do not activate ordinary periodic checks.

**Tech Stack:** Go 1.26, contexts, existing dae dialer connectivity checks, Go tests, Linux test execution.

---

## File Map

- Modify `component/outbound/dialer/connectivity_check.go`
  - Extract construction of TCP check options.
  - Add the cancellable one-shot TCP probe API.
  - Remove failover-group goroutine retention.
- Modify `component/outbound/dialer/dialer.go`
  - Remove `activeFailoverGroups`.
- Modify `component/outbound/failover_controller.go`
  - Remove `probeResultCh`.
  - Call `primary.ProbeTCPOnce(ctx)` directly.
  - Keep alive-transition callbacks only as the primary failure trigger.
- Modify `component/outbound/dialer_group.go`
  - Remove failover-group registration and unregistration.
- Create `component/outbound/dialer/tcp_probe_once_test.go`
  - Test fresh per-call results, cancellation, dual-stack aggregation, and no health-state mutation.
- Modify `component/outbound/failover_controller_test.go`
  - Test consecutive real probe results, reset after failure, and failback.

### Task 1: Add failing tests for the one-shot dialer API

**Files:**
- Create: `component/outbound/dialer/tcp_probe_once_test.go`

- [ ] **Step 1: Write a test showing every invocation returns a fresh result**

Use an `httptest.Server` and configure the dialer's TCP check option to resolve directly to the server address. Call the future API twice while the server is healthy:

```go
func TestProbeTCPOnceReturnsSuccessForEveryInvocation(t *testing.T) {
	d, server := newOneShotTCPProbeTestDialer(t)
	defer server.Close()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	for i := 0; i < 2; i++ {
		ok, err := d.ProbeTCPOnce(ctx)
		if err != nil {
			t.Fatalf("probe %d returned error: %v", i+1, err)
		}
		if !ok {
			t.Fatalf("probe %d returned false, want true", i+1)
		}
	}
}
```

The helper must use an HTTP server returning `204`, a direct dialer, and a `TcpCheckOptionRaw` whose URL host and explicit IPv4 address point at that server.

- [ ] **Step 2: Write cancellation and failure tests**

```go
func TestProbeTCPOnceHonorsContextCancellation(t *testing.T) {
	d, server := newBlockingOneShotTCPProbeTestDialer(t)
	defer server.Close()
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	ok, err := d.ProbeTCPOnce(ctx)
	if ok {
		t.Fatal("cancelled probe returned success")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline exceeded", err)
	}
}

func TestProbeTCPOnceReturnsFailureForUnreachableTarget(t *testing.T) {
	d := newUnreachableOneShotTCPProbeTestDialer(t)
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ok, err := d.ProbeTCPOnce(ctx)
	if ok {
		t.Fatal("unreachable target returned success")
	}
	if err == nil {
		t.Fatal("unreachable target returned nil error")
	}
}
```

- [ ] **Step 3: Write tests for dual-stack aggregation and health isolation**

```go
func TestProbeTCPOnceSucceedsWhenOneApplicableFamilySucceeds(t *testing.T) {
	d, server := newOneShotTCPProbeTestDialer(t)
	defer server.Close()
	defer d.Close()

	tcp4 := &NetworkType{L4Proto: consts.L4ProtoStr_TCP, IpVersion: consts.IpVersionStr_4}
	d.ReportUnavailableForced(tcp4, errors.New("keep canonical health dead"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ok, err := d.ProbeTCPOnce(ctx)
	if err != nil || !ok {
		t.Fatalf("ProbeTCPOnce() = (%v, %v), want (true, nil)", ok, err)
	}
	if d.MustGetAlive(tcp4) {
		t.Fatal("one-shot recovery probe must not mutate canonical health state")
	}
}
```

The test helper should configure only IPv4 as applicable. The implementation must treat an unavailable address family as skipped, not as a failure overriding a successful applicable family.

- [ ] **Step 4: Run the new tests and verify RED**

Run:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c \
  -o /tmp/dae-dialer-probe-red.test ./component/outbound/dialer
scp /tmp/dae-dialer-probe-red.test vm-ubuntu-agent:/tmp/
ssh vm-ubuntu-agent \
  "/tmp/dae-dialer-probe-red.test -test.run 'TestProbeTCPOnce' -test.v"
```

Expected: compilation fails because `(*Dialer).ProbeTCPOnce` does not exist.

- [ ] **Step 5: Commit the failing tests**

```bash
git add component/outbound/dialer/tcp_probe_once_test.go
git commit -m "test(failover): specify one-shot TCP probe behavior"
```

### Task 2: Implement the cancellable one-shot TCP probe

**Files:**
- Modify: `component/outbound/dialer/connectivity_check.go`
- Test: `component/outbound/dialer/tcp_probe_once_test.go`

- [ ] **Step 1: Extract shared TCP check-option construction**

Move the existing TCP mark/MPTCP parsing and the `tcp4CheckOpt`/`tcp6CheckOpt` construction from `aliveBackground()` into:

```go
func (d *Dialer) tcpCheckOptions() [2]*CheckOption {
	var tcpSomark uint32
	var mptcp bool
	if network, err := netproxy.ParseMagicNetwork(d.TcpCheckOptionRaw.ResolverNetwork); err == nil {
		tcpSomark = network.Mark
		mptcp = network.Mptcp
	}

	newOption := func(ipVersion consts.IpVersionStr) *CheckOption {
		networkType := &NetworkType{
			L4Proto:   consts.L4ProtoStr_TCP,
			IpVersion: ipVersion,
			IsDns:     false,
		}
		return &CheckOption{
			networkType: networkType,
			CheckFunc: func(ctx context.Context, typ *NetworkType) (bool, error) {
				opt, err := d.TcpCheckOptionRaw.Option()
				if err != nil {
					return false, err
				}
				ip := opt.Ip4
				if typ.IpVersion == consts.IpVersionStr_6 {
					ip = opt.Ip6
				}
				if !ip.IsValid() {
					return false, nil
				}
				return d.HttpCheck(ctx, typ.Index(), opt.Url, ip, opt.Method, tcpSomark, mptcp)
			},
		}
	}

	return [2]*CheckOption{
		newOption(consts.IpVersionStr_4),
		newOption(consts.IpVersionStr_6),
	}
}
```

Use the repository's exact IP-version type if it differs from the snippet. `aliveBackground()` must consume these returned options so periodic and one-shot checks use identical targets and `HttpCheck` behavior.

- [ ] **Step 2: Implement `ProbeTCPOnce`**

```go
func (d *Dialer) ProbeTCPOnce(ctx context.Context) (bool, error) {
	if ctx == nil {
		return false, errors.New("nil probe context")
	}

	d.TcpCheckOptionRaw.Reset()
	options := d.tcpCheckOptions()

	type result struct {
		ok         bool
		applicable bool
		err        error
	}
	results := make(chan result, len(options))

	for _, option := range options {
		option := option
		go func() {
			ok, err := option.CheckFunc(ctx, option.networkType)
			results <- result{
				ok:         ok && err == nil,
				applicable: ok || err != nil,
				err:        err,
			}
		}()
	}

	var firstErr error
	applicable := false
	for range options {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case result := <-results:
			if result.ok {
				return true, nil
			}
			if result.applicable {
				applicable = true
			}
			if firstErr == nil && result.err != nil {
				firstErr = result.err
			}
		}
	}

	if firstErr != nil {
		return false, firstErr
	}
	if !applicable {
		return false, errors.New("TCP probe has no applicable IP address")
	}
	return false, errors.New("TCP probe failed")
}
```

Before finalizing, ensure worker goroutines cannot block after an early successful return. The buffered channel capacity must equal the number of options. Do not call `markAvailable`, `markUnavailable`, or `notifyAliveTransition`; this API reports evidence to the failover controller without changing canonical dialer health.

- [ ] **Step 3: Run the one-shot probe tests and verify GREEN**

Run the same cross-compiled Linux test command from Task 1.

Expected: all `TestProbeTCPOnce*` tests pass.

- [ ] **Step 4: Run existing dialer tests**

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c \
  -o /tmp/dae-dialer-probe.test ./component/outbound/dialer
scp /tmp/dae-dialer-probe.test vm-ubuntu-agent:/tmp/
ssh vm-ubuntu-agent "/tmp/dae-dialer-probe.test -test.timeout 120s"
```

Expected: `PASS`.

- [ ] **Step 5: Commit the implementation**

```bash
git add component/outbound/dialer/connectivity_check.go \
  component/outbound/dialer/tcp_probe_once_test.go
git commit -m "feat(dialer): add cancellable one-shot TCP probe"
```

### Task 3: Make the failover controller consume per-probe results

**Files:**
- Modify: `component/outbound/failover_controller.go`
- Modify: `component/outbound/failover_controller_test.go`

- [ ] **Step 1: Write a failing controller test for three consecutive real results**

Add a test seam to `FailoverController`:

```go
probeTCP func(context.Context) (bool, error)
```

`NewFailoverController` initializes it to `primary.ProbeTCPOnce`. The test replaces it with a deterministic sequence:

```go
func TestFailoverControllerThreeSuccessfulProbesCompleteFailback(t *testing.T) {
	fc := newTestFailoverController(t, FailoverRecoveryConfig{
		ProbeInitial: time.Second,
		ProbeMax:     8 * time.Second,
		Successes:    3,
		StableTime:   time.Second,
	})
	fc.onPrimaryHealthChange(tcp4NetworkType(), false)

	fc.probeTCP = func(context.Context) (bool, error) {
		return true, nil
	}

	fc.mu.Lock()
	fc.stableSince = time.Now().Add(-time.Second)
	fc.mu.Unlock()

	for i := 0; i < 3; i++ {
		fc.runProbe(fc.generation)
	}

	if got := fc.State(); got != statePrimaryActive {
		t.Fatalf("state = %v, want primary_active", got)
	}
	if got := fc.ActiveDialerIndex(); got != 0 {
		t.Fatalf("active role = %d, want primary", got)
	}
}
```

Avoid real timers in this test by stopping the timer under `fc.mu` before manually invoking `runProbe`.

- [ ] **Step 2: Write a failure-reset test**

```go
func TestFailoverControllerProbeFailureResetsConsecutiveSuccesses(t *testing.T) {
	results := []bool{true, true, false, true}
	// Install probeTCP that returns and removes results[0].
	// Invoke runProbe four times.
	// Assert recoverySuccesses == 1 and state remains recovering.
}
```

This proves the controller counts invocation results, not health transitions.

- [ ] **Step 3: Verify the new tests fail before controller changes**

Run:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c \
  -o /tmp/dae-outbound-controller-red.test ./component/outbound
scp /tmp/dae-outbound-controller-red.test vm-ubuntu-agent:/tmp/
ssh vm-ubuntu-agent \
  "/tmp/dae-outbound-controller-red.test -test.run 'TestFailoverControllerThreeSuccessfulProbes|TestFailoverControllerProbeFailureResets' -test.v"
```

Expected: FAIL because the controller has no `probeTCP` result seam and still waits for transition callbacks.

- [ ] **Step 4: Replace callback-based result collection**

In `FailoverController`:

```go
probeTCP func(context.Context) (bool, error)
```

Initialize it:

```go
probeTCP: primary.ProbeTCPOnce,
```

Replace `probePrimaryTCP` with:

```go
func (fc *FailoverController) probePrimaryTCP(ctx context.Context) bool {
	ok, err := fc.probeTCP(ctx)
	if err != nil && fc.log.IsLevelEnabled(logrus.DebugLevel) {
		fc.log.WithError(err).
			WithField("group", fc.groupName).
			WithField("primary", dialerName(fc.primary)).
			Debug("recovery TCP probe failed")
	}
	return ok && err == nil
}
```

Delete `probeResultCh` and all callback branches that send recovery results. `onPrimaryHealthChange` must return early for `alive == true` and retain only the canonical `alive -> not alive` failover trigger.

- [ ] **Step 5: Run controller tests**

Use the command from Step 3.

Expected: both tests pass.

- [ ] **Step 6: Commit the controller change**

```bash
git add component/outbound/failover_controller.go \
  component/outbound/failover_controller_test.go
git commit -m "fix(failover): count explicit TCP probe results"
```

### Task 4: Remove periodic-check activation caused by failover

**Files:**
- Modify: `component/outbound/dialer/dialer.go`
- Modify: `component/outbound/dialer/connectivity_check.go`
- Modify: `component/outbound/dialer_group.go`
- Modify: `component/outbound/failover_controller_test.go`

- [ ] **Step 1: Add a failing test proving failover construction does not activate checks**

Use the existing reflection helper or add a package-level test helper to inspect `checkActivated`:

```go
func TestFailoverGroupDoesNotActivateOrdinaryPeriodicChecks(t *testing.T) {
	group, primary, fallback := newFailoverGroupForTest(t)
	defer group.Close()

	if dialerCheckActivated(primary) {
		t.Fatal("primary periodic checker activated by failover group")
	}
	if dialerCheckActivated(fallback) {
		t.Fatal("fallback periodic checker activated by failover group")
	}
}
```

Also assert that invoking `primary.ProbeTCPOnce(ctx)` works without calling `ActivateCheck()`.

- [ ] **Step 2: Remove failover goroutine-retention state**

Delete:

```go
activeFailoverGroups atomic.Int32
RegisterFailoverGroup()
UnregisterFailoverGroup()
```

Remove the `activeFailoverGroups` branch from `checkUnused()`.

Remove these calls from `NewDialerGroup` and `DialerGroup.Close()`:

```go
RegisterFailoverGroup()
UnregisterFailoverGroup()
```

- [ ] **Step 3: Run the no-periodic-check test**

Run:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c \
  -o /tmp/dae-outbound-no-periodic.test ./component/outbound
scp /tmp/dae-outbound-no-periodic.test vm-ubuntu-agent:/tmp/
ssh vm-ubuntu-agent \
  "/tmp/dae-outbound-no-periodic.test -test.run 'TestFailoverGroupDoesNotActivateOrdinaryPeriodicChecks' -test.v"
```

Expected: `PASS`.

- [ ] **Step 4: Commit the cleanup**

```bash
git add component/outbound/dialer/dialer.go \
  component/outbound/dialer/connectivity_check.go \
  component/outbound/dialer_group.go \
  component/outbound/failover_controller_test.go
git commit -m "fix(failover): avoid ordinary periodic checks"
```

### Task 5: End-to-end recovery state-machine verification

**Files:**
- Modify: `component/outbound/failover_controller_test.go`

- [ ] **Step 1: Add a complete deterministic state-machine test**

Use a short configuration and a scripted `probeTCP`:

```go
func TestFailoverControllerRecoverySequence(t *testing.T) {
	// Sequence: failure, success, success, success.
	// Assert:
	// 1. primary failure selects fallback;
	// 2. recovery failure doubles delay;
	// 3. first success enters recovering and resets delay;
	// 4. second success increments without timeout;
	// 5. third success plus stable time selects primary;
	// 6. TCP and UDP selections both observe the new role.
}
```

Use controller methods and group selection directly. Do not use `time.Sleep`; set `stableSince` under the controller mutex or introduce a package-level `now func() time.Time` seam if production timing must be tested.

- [ ] **Step 2: Add a cancellation lifecycle test**

```go
func TestFailoverControllerCloseCancelsOneShotProbe(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	fc.probeTCP = func(ctx context.Context) (bool, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return false, ctx.Err()
	}

	go fc.runProbe(fc.generation)
	<-started
	fc.Close()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel active one-shot probe")
	}
}
```

- [ ] **Step 3: Run all failover tests**

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c \
  -o /tmp/dae-outbound-failover.test ./component/outbound
scp /tmp/dae-outbound-failover.test vm-ubuntu-agent:/tmp/
ssh vm-ubuntu-agent \
  "/tmp/dae-outbound-failover.test -test.run 'TestFailover|TestValidateFailover' -test.timeout 60s"
```

Expected: `PASS`.

- [ ] **Step 4: Commit the state-machine tests**

```bash
git add component/outbound/failover_controller_test.go
git commit -m "test(failover): cover complete recovery sequence"
```

### Task 6: Full regression and acceptance verification

**Files:**
- Verify only.

- [ ] **Step 1: Run formatting and static checks**

```bash
gofmt -w component/outbound/dialer/connectivity_check.go \
  component/outbound/dialer/tcp_probe_once_test.go \
  component/outbound/failover_controller.go \
  component/outbound/failover_controller_test.go \
  component/outbound/dialer_group.go
git diff --check
go vet ./component/outbound/...
```

Expected: no formatting errors, whitespace errors, or vet findings. Run `go vet` on Linux if macOS cannot compile Linux-only packages.

- [ ] **Step 2: Run full Linux package tests**

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c \
  -o /tmp/dae-outbound-final.test ./component/outbound
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go test -c \
  -o /tmp/dae-dialer-final.test ./component/outbound/dialer
scp /tmp/dae-outbound-final.test /tmp/dae-dialer-final.test \
  vm-ubuntu-agent:/tmp/
ssh vm-ubuntu-agent "/tmp/dae-outbound-final.test -test.timeout 120s"
ssh vm-ubuntu-agent "/tmp/dae-dialer-final.test -test.timeout 120s"
```

Expected: both binaries print `PASS` and exit zero.

- [ ] **Step 3: Run race tests on a Linux build host**

```bash
go test -race ./component/outbound/... -count=1
```

Expected: `PASS` with no race reports. This must run on a Linux host with Go 1.26 because the package uses Linux-only networking APIs.

- [ ] **Step 4: Verify the relevant spec requirements**

Confirm with tests and source inspection:

```text
- Every recovery invocation returns a fresh success/failure result.
- Three healthy probes can produce three consecutive successes.
- One failure resets recovery progress.
- Probe cancellation follows FailoverController.Close().
- Healthy failover groups do not start periodic dialer checks.
- Only the primary is probed after failover.
- TCP and UDP new-session selection fail back together.
- Existing connections remain untouched.
```

- [ ] **Step 5: Remove temporary VM artifacts**

```bash
ssh vm-ubuntu-agent \
  "rm -f /tmp/dae-*-probe*.test /tmp/dae-*-failover.test /tmp/dae-*-final.test"
```

- [ ] **Step 6: Commit any final formatting-only changes**

```bash
git add component/outbound
git commit -m "chore(failover): finalize one-shot probe implementation"
```

