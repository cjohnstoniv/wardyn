// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package conformance is the driver-agnostic parity gate for runner.Runner
// implementations. Call Run(t, r) from driver-specific test files.
//
// Contract assertions enforced here:
//  1. Capabilities sanity: if ConfinementClasses is non-empty, CreateSandbox
//     must honour the strongest class and must never silently downgrade it.
//  2. Create → Status → Stop idempotency: a second StopSandbox on a stopped
//     sandbox must return nil.
//  3. KillSandbox on a missing/unknown ref must return nil.
//  4. L0 assertion hook: if StructuralEgress is declared, an injectable probe
//     (DefaultRouteProbe) asserts that no default route is reachable from the
//     sandbox.
//  5. Wait exit-code propagation: Exec'ing a short-lived command, Wait must
//     block until it exits and return its exit code (incl. a non-zero code).
//  6. Loopback relay parity: ExecStream reaches a port the sandbox is already
//     listening on inside its own netns, full-duplex, with stdin still open —
//     the transport the UI-sandbox gateway rides.
package conformance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// DefaultRouteProbe is called by Run when the driver declares StructuralEgress.
// Implementations should verify that no default-route connectivity exists from
// within the sandbox identified by ref, and call t.Errorf if one is found.
// The probe is injected so that the conformance library does not itself
// require network access or a running sandbox substrate.
type DefaultRouteProbe func(t *testing.T, ref string)

// Options controls optional behaviour of the conformance suite.
type Options struct {
	// DefaultRouteProbe is called when StructuralEgress is declared. If nil
	// and StructuralEgress is true the suite marks the L0 check as skipped.
	DefaultRouteProbe DefaultRouteProbe
	// SandboxImage is the OCI image used for CreateSandbox calls. If empty,
	// "scratch" is used (drivers that cannot pull scratch should substitute
	// their own minimal image).
	SandboxImage string
	// Timeout is applied to each individual operation. Defaults to 30s.
	Timeout time.Duration
	// ExitArgv, when non-nil, builds the argv for a short-lived in-sandbox
	// command that exits with the given code. It gates the Wait exit-code
	// conformance case: when nil (or the driver declares no ConfinementClasses)
	// the case is skipped, because the suite cannot otherwise run a real process
	// inside SandboxImage. Drivers supply something like
	// {"sh", "-c", fmt.Sprintf("exit %d", code)} for their minimal image.
	ExitArgv func(code int) []string
}

func (o Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return 30 * time.Second
}

func (o Options) image() string {
	if o.SandboxImage != "" {
		return o.SandboxImage
	}
	return "scratch"
}

// Run executes the full conformance suite against r. It is the single
// entry-point called from driver-specific _test files.
func Run(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	t.Run("Capabilities", func(t *testing.T) { testCapabilities(t, r, opts) })
	t.Run("CreateStatusStop", func(t *testing.T) { testCreateStatusStop(t, r, opts) })
	t.Run("KillMissingRef", func(t *testing.T) { testKillMissingRef(t, r, opts) })
	t.Run("L0StructuralEgress", func(t *testing.T) { testL0StructuralEgress(t, r, opts) })
	t.Run("WaitExitCode", func(t *testing.T) { testWaitExitCode(t, r, opts) })
	t.Run("InteractiveAttach", func(t *testing.T) { testInteractiveAttach(t, r, opts) })
	t.Run("ExecStream", func(t *testing.T) { testExecStream(t, r, opts) })
	t.Run("ExecStreamLoopbackRelay", func(t *testing.T) { testExecStreamLoopbackRelay(t, r, opts) })
}

// testCapabilities asserts Capabilities invariants.
//
//   - Driver field must equal r.Name().
//   - If ConfinementClasses is non-empty, CreateSandbox requesting the
//     strongest (last) class must produce a sandbox whose EnforcedClass is >=
//     the requested class (not silently downgraded).
func testCapabilities(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities returned error: %v", err)
	}

	if caps.Driver != r.Name() {
		t.Errorf("Capabilities.Driver = %q, want %q (must match runner.Name())", caps.Driver, r.Name())
	}

	if len(caps.ConfinementClasses) == 0 {
		// Honest empty — no further checks needed.
		t.Logf("ConfinementClasses is empty; driver %q makes no confinement claims (ok for stubs)", r.Name())
		return
	}

	// At least one class declared: CreateSandbox must honour the strongest.
	strongest := caps.ConfinementClasses[len(caps.ConfinementClasses)-1]
	spec := minimalSpec(opts.image())
	spec.ConfinementClass = strongest

	sb, err := r.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox with strongest class %q failed: %v", strongest, err)
	}
	defer func() {
		// Best-effort cleanup; ignore error (StopSandbox idempotency tested separately).
		_ = r.StopSandbox(context.Background(), sb.Ref)
	}()

	if sb.EnforcedClass.Rank() < strongest.Rank() {
		t.Errorf("CreateSandbox silently downgraded: requested %q, enforced %q", strongest, sb.EnforcedClass)
	}
}

// testCreateStatusStop asserts the create → status → stop idempotency contract.
//
// If the driver declares no ConfinementClasses (honest stub) it returns not-implemented
// for CreateSandbox; the test skips gracefully in that case.
func testCreateStatusStop(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	spec := minimalSpec(opts.image())
	if len(caps.ConfinementClasses) > 0 {
		spec.ConfinementClass = caps.ConfinementClasses[len(caps.ConfinementClasses)-1]
	}

	sb, err := r.CreateSandbox(ctx, spec)
	if err != nil {
		// Drivers that declare empty capabilities are expected to return
		// ErrNotImplemented here; skip rather than fail.
		t.Skipf("CreateSandbox not available (driver %q): %v", r.Name(), err)
	}
	ref := sb.Ref

	// Status must be queryable immediately after creation.
	_, err = r.Status(ctx, ref)
	if err != nil {
		t.Errorf("Status after CreateSandbox returned error: %v", err)
	}

	// First StopSandbox must succeed.
	if err := r.StopSandbox(ctx, ref); err != nil {
		t.Fatalf("StopSandbox returned error: %v", err)
	}

	// Second StopSandbox on the same (now stopped) ref must be idempotent.
	if err := r.StopSandbox(ctx, ref); err != nil {
		t.Errorf("StopSandbox idempotency violated: second call returned %v", err)
	}
}

// testKillMissingRef asserts that KillSandbox on an unknown ref returns nil.
// This is the kill-switch idempotency contract: a double-kill or a kill
// after the sandbox was already reaped by another path must not error.
func testKillMissingRef(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	if len(caps.ConfinementClasses) == 0 {
		// Stub driver with honest empty caps; skip rather than fail.
		t.Skipf("driver %q declares no confinement classes; KillSandbox idempotency not testable without a sandbox substrate", r.Name())
	}

	// Use a UUID that was never a real sandbox ref.
	ghost := "wardyn-conformance-ghost-" + uuid.New().String()
	if err := r.KillSandbox(ctx, ghost); err != nil {
		t.Errorf("KillSandbox on missing ref %q returned %v; want nil (idempotent)", ghost, err)
	}
}

// testL0StructuralEgress checks the L0 invariant when StructuralEgress is
// declared. If the driver does not declare StructuralEgress the test is a
// no-op (the class only applies once declared). If opts.DefaultRouteProbe is
// nil and StructuralEgress is declared, the test is skipped with a notice.
func testL0StructuralEgress(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	if !caps.StructuralEgress {
		t.Skipf("driver %q does not declare StructuralEgress; L0 probe not required", r.Name())
	}

	if opts.DefaultRouteProbe == nil {
		t.Skipf("StructuralEgress declared by %q but no DefaultRouteProbe injected; mark the test as pending in driver CI", r.Name())
	}

	if len(caps.ConfinementClasses) == 0 {
		t.Skipf("driver %q declares StructuralEgress but no ConfinementClasses; cannot create sandbox for L0 probe", r.Name())
	}

	sb := createStrongestSandbox(t, ctx, r, caps, opts, "L0 probe")

	// Delegate to the injected probe; it calls t.Errorf if a default route exists.
	opts.DefaultRouteProbe(t, sb.Ref)
}

// testWaitExitCode asserts the Wait exit-code propagation contract: after Exec
// starts a short-lived command in the sandbox, Wait blocks until it exits and
// returns its exit code — including a non-zero code, which the control plane
// maps to FAILED (vs COMPLETED on 0).
//
// The case is skipped when the driver declares no ConfinementClasses (honest
// stub: it cannot create a sandbox or run a process) or when opts.ExitArgv is
// nil (the suite has no command that exits with a known code in SandboxImage).
func testWaitExitCode(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skipf("driver %q declares no confinement classes; Wait not testable without a sandbox substrate", r.Name())
	}
	if opts.ExitArgv == nil {
		t.Skipf("no ExitArgv supplied for driver %q; Wait exit-code case skipped", r.Name())
	}

	sb := createStrongestSandbox(t, ctx, r, caps, opts, "Wait exit-code case")

	// A non-zero exit code is the interesting case: it is what distinguishes
	// FAILED from COMPLETED. Use a distinctive code so a spurious 0/1 cannot
	// pass by accident.
	const wantCode = 42
	if _, err := r.Exec(ctx, sb.Ref, opts.ExitArgv(wantCode)); err != nil {
		t.Fatalf("Exec(exit %d): %v", wantCode, err)
	}

	got, err := r.Wait(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
	if got != wantCode {
		t.Errorf("Wait exit code = %d, want %d", got, wantCode)
	}
}

// testInteractiveAttach asserts the interactive-attach contract: Attach opens a
// live PTY shell inside a RUNNING sandbox, keystrokes written to the Session are
// echoed/processed and read back, Resize succeeds, and Close tears down ONLY the
// exec stream (Status still reports the sandbox RUNNING afterwards).
//
// The case is skipped when the driver declares no ConfinementClasses (honest
// stub: it cannot create a sandbox) — drivers that return ErrNotImplemented from
// Attach are also skipped gracefully. The shell is assumed to be /bin/sh (the
// driver's documented default), present in every minimal SandboxImage.
func testInteractiveAttach(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skipf("driver %q declares no confinement classes; Attach not testable without a sandbox substrate", r.Name())
	}

	sb := createStrongestSandbox(t, ctx, r, caps, opts, "Attach")

	sess, err := r.Attach(ctx, sb.Ref, runner.AttachOptions{Cols: 80, Rows: 24})
	if err != nil {
		// A driver that has not implemented Attach yet skips rather than fails.
		t.Skipf("Attach not available (driver %q): %v", r.Name(), err)
	}

	// Read the PTY in the background so the shell's output (incl. our echoed
	// command) is drained while we type.
	got := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 4096)
		for {
			n, rerr := sess.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				if bytes.Contains(buf.Bytes(), []byte("wardyn-attach-ok")) {
					got <- buf.String()
					return
				}
			}
			if rerr != nil {
				got <- buf.String()
				return
			}
		}
	}()

	// Type a command whose output is a unique marker so we can confirm the PTY
	// is genuinely interactive (input -> shell -> output round-trips).
	if _, err := sess.Write([]byte("echo wardyn-attach-ok\n")); err != nil {
		t.Fatalf("Session.Write: %v", err)
	}

	// Resize the PTY mid-session; it must not error.
	if err := sess.Resize(ctx, 120, 40); err != nil {
		t.Errorf("Session.Resize: %v", err)
	}

	select {
	case out := <-got:
		if !bytes.Contains([]byte(out), []byte("wardyn-attach-ok")) {
			t.Errorf("attach PTY did not echo the marker; got:\n%s", out)
		}
	case <-time.After(15 * time.Second):
		t.Error("timed out waiting for attach PTY to echo the marker")
	}

	// Close tears down ONLY the exec stream — the sandbox must still be RUNNING.
	if err := sess.Close(); err != nil {
		t.Errorf("Session.Close: %v", err)
	}
	st, err := r.Status(ctx, sb.Ref)
	if err != nil {
		t.Fatalf("Status after detach: %v", err)
	}
	if st.State != types.RunRunning {
		t.Errorf("after Session.Close the sandbox must still be RUNNING (detach must not stop it), got %q", st.State)
	}
}

// testExecStream asserts the ExecStream primitive contract: (a) a Stdin write
// followed by a half-close (Stdin.Close) flushes to the exec and terminates
// its read side, (b) Stdout delivers the exec's output, (c) Wait propagates a
// non-zero exit code, and (d) with TTY=false a stderr write lands on Stderr,
// NOT interleaved into Stdout (stdout/stderr must be separate streams so a
// binary protocol riding stdout is never corrupted).
//
// The case is skipped when the driver declares no ConfinementClasses (honest
// stub: it cannot create a sandbox) or when ExecStream returns
// runner.ErrExecStreamUnsupported (not implemented yet). Any OTHER error from
// ExecStream FAILS the case — unlike a bare "returns an error => skip" check,
// this means an implemented-but-broken ExecStream cannot skip green by
// returning some unrelated error. The exec shell is assumed to be /bin/sh
// (the driver's documented default), present in every minimal SandboxImage —
// the same assumption testInteractiveAttach makes.
func testExecStream(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skipf("driver %q declares no confinement classes; ExecStream not testable without a sandbox substrate", r.Name())
	}

	sb := createStrongestSandbox(t, ctx, r, caps, opts, "ExecStream")

	const wantCode = 7
	spec := runner.ExecSpec{
		Argv: []string{"sh", "-c", "cat; echo wardyn-execstream-stderr >&2; exit 7"},
	}
	sess, err := r.ExecStream(ctx, sb.Ref, spec)
	if errors.Is(err, runner.ErrExecStreamUnsupported) {
		t.Skipf("ExecStream not implemented (driver %q): %v", r.Name(), err)
	}
	if err != nil {
		t.Fatalf("ExecStream: %v", err)
	}

	// Drain Stdout/Stderr concurrently so neither pipe can block the exec while
	// we write + half-close Stdin below.
	stdoutCh := make(chan string, 1)
	stderrCh := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(sess.Stdout)
		stdoutCh <- string(b)
	}()
	go func() {
		b, _ := io.ReadAll(sess.Stderr)
		stderrCh <- string(b)
	}()

	if _, err := sess.Stdin.Write([]byte("wardyn-execstream-ok\n")); err != nil {
		t.Fatalf("Stdin.Write: %v", err)
	}
	// Half-close: `cat` must see EOF on its stdin and finish, without tearing
	// down the still-live Stdout/Stderr streams.
	if err := sess.Stdin.Close(); err != nil {
		t.Fatalf("Stdin.Close (half-close): %v", err)
	}

	var stdout, stderr string
	select {
	case stdout = <-stdoutCh:
	case <-time.After(opts.timeout()):
		t.Fatal("timed out reading Stdout")
	}
	select {
	case stderr = <-stderrCh:
	case <-time.After(opts.timeout()):
		t.Fatal("timed out reading Stderr")
	}

	if !strings.Contains(stdout, "wardyn-execstream-ok") {
		t.Errorf("Stdout = %q, want it to contain the echoed stdin marker (stdin write + half-close must flush cat's output)", stdout)
	}
	if strings.Contains(stdout, "wardyn-execstream-stderr") {
		t.Errorf("Stdout = %q, must NOT contain the stderr write — stdout/stderr must be separate streams when TTY=false", stdout)
	}
	if !strings.Contains(stderr, "wardyn-execstream-stderr") {
		t.Errorf("Stderr = %q, want it to contain the stderr marker", stderr)
	}

	code, err := sess.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if code != wantCode {
		t.Errorf("Wait exit code = %d, want %d", code, wantCode)
	}

	if err := sess.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// RecordingProbe is called by CheckRecordingCapability when the driver declares
// SessionRecording. Implementations should verify that the driver actually
// honoured the recording contract for the sandbox identified by ref — i.e. a
// cast/recording artifact was genuinely produced (uploaded through the proxy's
// brokered route or written to the shared-mount fallback) — and call t.Errorf if
// it was not. Like DefaultRouteProbe, the probe is injected so the conformance
// library itself needs no recorder substrate.
type RecordingProbe func(t *testing.T, ref string)

// RecordingOptions controls CheckRecordingCapability. It embeds Options for
// the SandboxImage/Timeout/ExitArgv fields the recording case shares with
// Run's suite; it is intentionally a SEPARATE type from Options so that
// adding the recording gate does not change the signature or behaviour of
// Run / the existing driver suites — only RecordingOptions literal
// construction (embedding changes neither).
type RecordingOptions struct {
	Options
	// RecordingProbe is invoked when the driver declares SessionRecording. If
	// the driver declares recording but RecordingProbe is nil, the check is
	// marked pending (skipped with a notice) — the same fail-soft posture
	// DefaultRouteProbe uses, so a driver CI without a recorder substrate does
	// not hard-fail, but a driver CI that wires the probe verifies the contract.
	RecordingProbe RecordingProbe
}

// CheckRecordingCapability is the recording-capability honesty gate. It is an
// ADDITIVE, standalone helper (NOT part of Run) so existing driver suites are
// unaffected: a driver opts in by calling it explicitly.
//
// The contract it enforces, fail-closed both ways:
//
//   - A driver that declares SessionRecording=true MUST honour the recording
//     contract. When opts.RecordingProbe is supplied, the suite creates a
//     sandbox, execs a (recorded) command, and delegates to the probe, which
//     asserts a real recording artifact exists. A driver that "declares but does
//     not produce" therefore fails. If no probe is injected the check is marked
//     pending (skipped with a notice) rather than passing silently.
//
//   - A driver that declares SessionRecording=false MUST NOT pretend to record.
//     The suite asserts the honest-empty posture: the driver makes no recording
//     claim, and (when a probe IS supplied for a non-recording driver, which is
//     a misuse) refuses to run it — a false-declaring driver cannot smuggle a
//     "recording happened" result past the gate.
//
// Drivers that declare no ConfinementClasses (honest stubs) cannot create a
// sandbox, so the artifact-producing half is necessarily skipped for them; the
// declaration-honesty assertion still runs and must pass.
func CheckRecordingCapability(t *testing.T, r runner.Runner, opts RecordingOptions) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}

	if !caps.SessionRecording {
		// Honest-empty: the driver makes no recording claim. It must not be
		// possible to obtain a "recording happened" result from a driver that
		// declares it cannot record — a probe here would be a misuse, so we
		// fail rather than run it. This is the fail-closed half: no pretending.
		if opts.RecordingProbe != nil {
			t.Errorf("driver %q declares SessionRecording=false but a RecordingProbe was supplied; a non-recording driver must not be probed as if it records (fail-closed)", r.Name())
		}
		t.Logf("driver %q declares SessionRecording=false; makes no recording claim (ok for stubs)", r.Name())
		return
	}

	// SessionRecording declared. Without an injected probe the suite cannot
	// verify a real artifact; mark pending rather than pass silently.
	if opts.RecordingProbe == nil {
		t.Skipf("driver %q declares SessionRecording but no RecordingProbe injected; mark the recording check pending in driver CI", r.Name())
	}

	// A stub that declares recording but cannot create a sandbox cannot produce
	// an artifact; the declaration is honest only if it can also run. Honest
	// stubs declare no classes AND no recording, so reaching here with empty
	// classes means an inconsistent driver — fail closed.
	if len(caps.ConfinementClasses) == 0 {
		t.Errorf("driver %q declares SessionRecording=true but no ConfinementClasses; it cannot create a sandbox to record into (overclaim, fail-closed)", r.Name())
		return
	}

	spec := minimalSpec(opts.image())
	spec.ConfinementClass = caps.ConfinementClasses[len(caps.ConfinementClasses)-1]

	sb, err := r.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox for recording probe: %v", err)
	}
	defer func() { _ = r.StopSandbox(context.Background(), sb.Ref) }()

	// Run a short-lived recorded command if the driver supplied an argv builder,
	// so the recorder has something to capture before the probe inspects it.
	if opts.ExitArgv != nil {
		if _, err := r.Exec(ctx, sb.Ref, opts.ExitArgv(0)); err != nil {
			t.Fatalf("Exec(recorded command): %v", err)
		}
		if _, err := r.Wait(ctx, sb.Ref); err != nil {
			t.Fatalf("Wait after recorded command: %v", err)
		}
	}

	// Delegate to the probe; it calls t.Errorf if no recording artifact exists.
	opts.RecordingProbe(t, sb.Ref)
}

// createStrongestSandbox creates a sandbox at the strongest confinement class
// declared in caps and registers its teardown via t.Cleanup. Callers must have
// already confirmed caps.ConfinementClasses is non-empty: the no-classes
// handling differs by KIND across call sites (Skipf/Logf/fail-closed Errorf)
// and is intentionally left to each caller rather than folded in here.
func createStrongestSandbox(t *testing.T, ctx context.Context, r runner.Runner, caps runner.Capabilities, opts Options, caseName string) runner.Sandbox {
	t.Helper()
	spec := minimalSpec(opts.image())
	spec.ConfinementClass = caps.ConfinementClasses[len(caps.ConfinementClasses)-1]

	sb, err := r.CreateSandbox(ctx, spec)
	if err != nil {
		t.Fatalf("CreateSandbox for %s: %v", caseName, err)
	}
	t.Cleanup(func() { _ = r.StopSandbox(context.Background(), sb.Ref) })
	return sb
}

// minimalSpec returns a SandboxSpec with a unique RunID suitable for
// conformance testing. No secrets, no proxy config, no resource limits.
func minimalSpec(image string) runner.SandboxSpec {
	return runner.SandboxSpec{
		RunID: uuid.New(),
		Image: image,
		// ConfinementClass deliberately left empty; callers set it.
		Labels: map[string]string{"wardyn.conformance": "true"},
	}
}

// loopbackRelayPort is the in-sandbox port the loopback-relay case uses. It is
// fixed (not random) because it appears in both halves of the case — the
// listener script and the dial argv — and every sandbox here is freshly
// created for this one case, so nothing else is on it.
const loopbackRelayPort = "39099"

// loopbackRelayMarker is served by the in-sandbox listener and must come back
// through the exec lane byte for byte.
const loopbackRelayMarker = "wardyn-loopback-relay-ok"

// loopbackRelayListenScript starts a loopback-only HTTP listener inside the
// sandbox the same way the UI gateway's launcher does (uiEnsureApp): a
// detaching start followed by a readiness poll, so the exec that started it
// EXITS and the listener stays. Distinct exit codes separate "this image
// cannot run the case" from "the case ran and failed" — the same coded-probe
// rule conformance_k8s_test.go's apiServerProbeScript follows:
//
//	90  no httpd applet   — image contract, not a substrate verdict
//	91  no nc applet      — image contract, not a substrate verdict
//	92  could not write the document root
//	93  httpd refused to start
//	94  the port never came up
//	0   the listener is up on 127.0.0.1:<port>
const loopbackRelayListenScript = `command -v httpd >/dev/null 2>&1 || exit 90
command -v nc >/dev/null 2>&1 || exit 91
mkdir -p /tmp/wardyn-relay && echo ` + loopbackRelayMarker + ` > /tmp/wardyn-relay/probe.txt || exit 92
httpd -p 127.0.0.1:` + loopbackRelayPort + ` -h /tmp/wardyn-relay || exit 93
i=0
while [ $i -lt 30 ]; do
  nc -z 127.0.0.1 ` + loopbackRelayPort + ` && exit 0
  i=$((i+1))
  sleep 1
done
exit 94`

// testExecStreamLoopbackRelay is the substrate-parity gate for the UI-sandbox
// relay (internal/api/uigateway.go). The relay is not a new network path: it
// is ExecStream carrying bytes to a port the sandbox is ALREADY listening on
// inside its own netns — the same lane the SSH gateway's `-L` forward uses.
// This case proves that lane behaves identically on every substrate, without
// importing any of the gateway's own code.
//
// It asserts the one thing testExecStream does not: FULL DUPLEX with stdin
// still open. testExecStream's round-trip only works because it half-closes
// stdin (`cat` needs EOF to finish); a relayed HTTP connection — and every
// WebSocket a code editor opens over it — must instead receive the response
// while stdin stays open for the next request. A substrate whose ExecStream
// only flushes on half-close would pass testExecStream and hang every relay.
//
// Skipped when the driver declares no ConfinementClasses (honest stub: no
// sandbox to listen in), when ExecStream is unsupported, or when the image
// carries no httpd/nc applet — the last is an IMAGE contract, not a substrate
// one (docs/UI-SANDBOXES.md's "Image contract (BYOI)"), and it says so out
// loud rather than passing silently.
func testExecStreamLoopbackRelay(t *testing.T, r runner.Runner, opts Options) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout())
	defer cancel()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.ConfinementClasses) == 0 {
		t.Skipf("driver %q declares no confinement classes; the loopback relay is not testable without a sandbox substrate", r.Name())
	}

	sb := createStrongestSandbox(t, ctx, r, caps, opts, "ExecStreamLoopbackRelay")

	// ── 1. start the in-sandbox listener over its own ExecStream ────────────
	starter, err := r.ExecStream(ctx, sb.Ref, runner.ExecSpec{Argv: []string{"sh", "-c", loopbackRelayListenScript}})
	if errors.Is(err, runner.ErrExecStreamUnsupported) {
		t.Skipf("ExecStream not implemented (driver %q): %v", r.Name(), err)
	}
	if err != nil {
		t.Fatalf("ExecStream(listener): %v", err)
	}
	startOut := drainBoth(starter)
	if err := starter.Stdin.Close(); err != nil {
		t.Fatalf("listener Stdin.Close: %v", err)
	}
	code, err := starter.Wait()
	if err != nil {
		t.Fatalf("listener Wait: %v", err)
	}
	_ = starter.Close()
	switch code {
	case 0:
	case 90, 91:
		t.Skipf("sandbox image %q has no httpd/nc applet (exit %d); the loopback-relay case needs a listener it can start in-image — an IMAGE contract, not a substrate verdict", opts.image(), code)
	default:
		t.Fatalf("in-sandbox listener did not come up (exit %d; 92=doc root, 93=httpd refused, 94=port never opened). Output:\n%s", code, startOut())
	}

	// ── 2. dial it over a SECOND ExecStream, stdin left OPEN ────────────────
	sess, err := r.ExecStream(ctx, sb.Ref, runner.ExecSpec{Argv: []string{"nc", "127.0.0.1", loopbackRelayPort}})
	if err != nil {
		t.Fatalf("ExecStream(dial): %v", err)
	}
	defer func() { _ = sess.Close() }()

	// Read INCREMENTALLY, never io.ReadAll: the dialer does not exit while its
	// stdin is open (that is the whole point of this case), so waiting for EOF
	// would wait forever even after the response has landed. Stderr is drained
	// alongside it — a substrate that demultiplexes stdout/stderr through a
	// pipe pair (docker's stdcopy) stalls BOTH streams if only one is read.
	respCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 4096)
		for {
			n, rerr := sess.Stdout.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				if strings.Contains(buf.String(), loopbackRelayMarker) {
					respCh <- buf.String()
					return
				}
			}
			if rerr != nil {
				respCh <- buf.String()
				return
			}
		}
	}()
	go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()

	// HTTP/1.0: the listener answers and closes its side, so the response is
	// complete without a Content-Length walk. Stdin is deliberately NEVER
	// closed — that is the assertion.
	req := "GET /probe.txt HTTP/1.0\r\nHost: 127.0.0.1:" + loopbackRelayPort + "\r\n\r\n"
	if _, err := sess.Stdin.Write([]byte(req)); err != nil {
		t.Fatalf("dial Stdin.Write: %v", err)
	}

	var resp string
	select {
	case resp = <-respCh:
	case <-time.After(opts.timeout()):
		t.Fatal("timed out reading the relayed response — the exec lane did not deliver bytes back while stdin was still open (full-duplex requirement of the UI relay)")
	}

	if !strings.Contains(resp, "200 OK") {
		t.Errorf("relayed response = %q, want an HTTP 200 from the sandbox's own loopback listener", resp)
	}
	if !strings.Contains(resp, loopbackRelayMarker) {
		t.Errorf("relayed response = %q, want it to carry the listener's body %q byte for byte", resp, loopbackRelayMarker)
	}
}

// drainBoth reads an ExecSession's stdout and stderr concurrently so neither
// pipe can block the exec, and returns a func that yields the combined text
// once both are done. Used for the loopback-relay listener, whose only output
// is diagnostic.
func drainBoth(sess *runner.ExecSession) func() string {
	var wg sync.WaitGroup
	var mu sync.Mutex
	var buf bytes.Buffer
	wg.Add(2)
	for _, rd := range []io.Reader{sess.Stdout, sess.Stderr} {
		go func(rd io.Reader) {
			defer wg.Done()
			b, _ := io.ReadAll(rd)
			mu.Lock()
			buf.Write(b)
			mu.Unlock()
		}(rd)
	}
	return func() string {
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		return buf.String()
	}
}
