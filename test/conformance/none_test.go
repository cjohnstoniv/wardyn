// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance_test

// none_test.go runs the driver-agnostic conformance gate against an in-test
// "none"/stub runner under PLAIN `go test` — no //go:build tag, no Docker, no
// Postgres, no network. It is the honesty counterpart to the docker target:
// where conformance_docker_test.go proves the docker driver HONOURS its claims
// against the live daemon, this file proves a driver that makes NO claims is
// held to "make no claims" and a driver that LIES about its claims is caught by
// the same gate (invariant 5: fail closed — never claim a control that is not
// structurally enforced).
//
// It also exercises the recording-capability gate (conformance.CheckRecording-
// Capability): a driver that declares SessionRecording must produce a real
// artifact, and one that does not declare it must not be able to pretend.
//
// All runners here are hand-rolled fakes (no mocking library), matching the
// repo convention. They implement runner.Runner directly so they flow through
// exactly the same conformance assertions as the real drivers.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
	"github.com/google/uuid"
)

// errNoneNotImplemented is the typed sentinel the honest none runner returns
// from every lifecycle method: a stub declares empty capabilities AND fails
// closed on lifecycle.
var errNoneNotImplemented = errors.New("none runner: not implemented (stub)")

// noneRunner is the honest stub: it declares ZERO capabilities (no confinement
// classes, no structural egress, no recording) and returns ErrNotImplemented
// from every lifecycle method. This is the shape a driver MUST take before its
// substrate is wired — it claims nothing and does nothing.
type noneRunner struct{}

func (noneRunner) Name() string { return "none" }

func (noneRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	// Honest empty: every claim false / nil. No overclaim of confinement or
	// structural egress (invariant 5 fail-closed posture).
	return runner.Capabilities{
		Driver:             "none",
		ConfinementClasses: nil,
		StructuralEgress:   false,
		NetworkPolicy:      false,
		WarmPools:          false,
		SessionRecording:   false,
	}, nil
}

func (noneRunner) CreateSandbox(context.Context, runner.SandboxSpec) (runner.Sandbox, error) {
	return runner.Sandbox{}, errNoneNotImplemented
}
func (noneRunner) Exec(context.Context, string, []string) error { return errNoneNotImplemented }
func (noneRunner) Wait(context.Context, string) (int, error)    { return 0, errNoneNotImplemented }
func (noneRunner) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	return nil, errNoneNotImplemented
}
func (noneRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{}, errNoneNotImplemented
}
func (noneRunner) StopSandbox(context.Context, string) error { return errNoneNotImplemented }
func (noneRunner) KillSandbox(context.Context, string) error { return errNoneNotImplemented }

// downgradingRunner is a DISHONEST stub: it CLAIMS CC3 (the strongest class) but
// CreateSandbox silently enforces an unrecognised/empty class. It exists only to
// prove the conformance Capabilities gate catches a silent downgrade (invariant
// 5). It is never exposed as a real driver.
type downgradingRunner struct{}

func (downgradingRunner) Name() string { return "downgrading" }

func (downgradingRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{
		Driver:             "downgrading",
		ConfinementClasses: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}, // claims CC3
	}, nil
}

func (downgradingRunner) CreateSandbox(_ context.Context, _ runner.SandboxSpec) (runner.Sandbox, error) {
	// The lie: the caller asked for the strongest declared class (CC3) but the
	// sandbox is created with NO enforced class — a silent downgrade below CC1.
	return runner.Sandbox{Ref: "wardyn-downgrade-" + uuid.NewString(), Driver: "downgrading", EnforcedClass: ""}, nil
}
func (downgradingRunner) Exec(context.Context, string, []string) error { return nil }
func (downgradingRunner) Wait(context.Context, string) (int, error)    { return 0, nil }
func (downgradingRunner) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	return nil, errors.New("downgrading: no attach")
}
func (downgradingRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}
func (downgradingRunner) StopSandbox(context.Context, string) error { return nil }
func (downgradingRunner) KillSandbox(context.Context, string) error { return nil }

// recordingRunner is an HONEST recording driver fake: it declares CC1 + Session-
// Recording and, on Exec, marks the sandbox as having produced a recording
// artifact. Its companion RecordingProbe inspects that flag. It models the
// minimum a real recording driver must satisfy: declare AND deliver.
type recordingRunner struct {
	// recorded tracks, per sandbox ref, whether a recording artifact was
	// produced. A real driver would inspect the proxy upload / shared-mount cast;
	// the fake records the flag Exec sets so the probe has something honest to
	// assert against.
	recorded map[string]bool
}

func newRecordingRunner() *recordingRunner {
	return &recordingRunner{recorded: make(map[string]bool)}
}

func (*recordingRunner) Name() string { return "recording" }

func (*recordingRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{
		Driver:             "recording",
		ConfinementClasses: []types.ConfinementClass{types.CC1},
		StructuralEgress:   false,
		SessionRecording:   true, // declares recording — must honour it
	}, nil
}

func (*recordingRunner) CreateSandbox(_ context.Context, _ runner.SandboxSpec) (runner.Sandbox, error) {
	return runner.Sandbox{Ref: "wardyn-rec-" + uuid.NewString(), Driver: "recording", EnforcedClass: types.CC1}, nil
}

func (r *recordingRunner) Exec(_ context.Context, ref string, _ []string) error {
	// Honour the declared recording capability: a recorded exec produces an
	// artifact for ref. A real driver wraps argv with wardyn-rec here.
	r.recorded[ref] = true
	return nil
}
func (*recordingRunner) Wait(context.Context, string) (int, error) { return 0, nil }
func (*recordingRunner) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	return nil, errors.New("recording: no attach")
}
func (*recordingRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}
func (*recordingRunner) StopSandbox(context.Context, string) error { return nil }
func (*recordingRunner) KillSandbox(context.Context, string) error { return nil }

// pretendingRecordingRunner DECLARES SessionRecording but its Exec produces NO
// artifact — the "declares but does not deliver" lie. The recording gate must
// catch it via the probe. Never a real driver.
type pretendingRecordingRunner struct{}

func (pretendingRecordingRunner) Name() string { return "pretending" }

func (pretendingRecordingRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{
		Driver:             "pretending",
		ConfinementClasses: []types.ConfinementClass{types.CC1},
		SessionRecording:   true, // claims recording...
	}, nil
}
func (pretendingRecordingRunner) CreateSandbox(context.Context, runner.SandboxSpec) (runner.Sandbox, error) {
	return runner.Sandbox{Ref: "wardyn-pretend-" + uuid.NewString(), Driver: "pretending", EnforcedClass: types.CC1}, nil
}
func (pretendingRecordingRunner) Exec(context.Context, string, []string) error { return nil } // ...but records nothing
func (pretendingRecordingRunner) Wait(context.Context, string) (int, error)    { return 0, nil }
func (pretendingRecordingRunner) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	return nil, errors.New("pretending: no attach")
}
func (pretendingRecordingRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}
func (pretendingRecordingRunner) StopSandbox(context.Context, string) error { return nil }
func (pretendingRecordingRunner) KillSandbox(context.Context, string) error { return nil }

// compile-time proof every fake satisfies the contract under test.
var (
	_ runner.Runner = noneRunner{}
	_ runner.Runner = downgradingRunner{}
	_ runner.Runner = (*recordingRunner)(nil)
	_ runner.Runner = pretendingRecordingRunner{}
)

// TestConformanceNoneStub runs the FULL driver-agnostic suite against the honest
// none runner under plain `go test`. The stub declares empty capabilities, so
// the create/kill/L0/Wait/Attach sub-tests skip gracefully (a genuinely absent
// substrate is the only allowed skip) and the Capabilities-honesty case runs to
// completion and must PASS. This is the no-Docker none target the lane requires.
func TestConformanceNoneStub(t *testing.T) {
	r := noneRunner{}

	if got := r.Name(); got != "none" {
		t.Errorf("Name() = %q, want %q", got, "none")
	}

	conformance.Run(t, r, conformance.Options{
		Timeout: 5 * time.Second,
	})
}

// TestNoneStubCapabilitiesHonest verifies the honest-empty Capabilities directly:
// a stub must declare NO confinement classes and NO structural egress before its
// substrate is wired — the invariant-5 fail-closed posture (never claim a
// control that is not structurally enforced).
func TestNoneStubCapabilitiesHonest(t *testing.T) {
	r := noneRunner{}
	ctx := context.Background()

	caps, err := r.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities returned error: %v", err)
	}

	t.Run("driver matches Name", func(t *testing.T) {
		if caps.Driver != r.Name() {
			t.Errorf("Capabilities.Driver = %q, want %q", caps.Driver, r.Name())
		}
	})
	t.Run("no confinement classes", func(t *testing.T) {
		if len(caps.ConfinementClasses) != 0 {
			t.Errorf("stub must not declare ConfinementClasses, got %v", caps.ConfinementClasses)
		}
	})
	t.Run("no structural egress", func(t *testing.T) {
		if caps.StructuralEgress {
			t.Error("stub must not declare StructuralEgress (invariant 5: no overclaim)")
		}
	})
	t.Run("no other claims", func(t *testing.T) {
		if caps.NetworkPolicy || caps.WarmPools || caps.SessionRecording {
			t.Errorf("stub must not declare NetworkPolicy/WarmPools/SessionRecording, got %+v", caps)
		}
	})
}

// TestNoneStubLifecycleNotImplemented verifies every lifecycle method on the
// honest stub returns (or wraps) its typed not-implemented sentinel — the stub
// fails closed on lifecycle just as it claims nothing in Capabilities.
func TestNoneStubLifecycleNotImplemented(t *testing.T) {
	r := noneRunner{}
	ctx := context.Background()
	spec := runner.SandboxSpec{RunID: uuid.New(), Image: "scratch"}

	cases := []struct {
		name string
		call func() error
	}{
		{"CreateSandbox", func() error { _, err := r.CreateSandbox(ctx, spec); return err }},
		{"Exec", func() error { return r.Exec(ctx, "any-ref", []string{"echo", "hi"}) }},
		{"Wait", func() error { _, err := r.Wait(ctx, "any-ref"); return err }},
		{"Attach", func() error { _, err := r.Attach(ctx, "any-ref", runner.AttachOptions{}); return err }},
		{"Status", func() error { _, err := r.Status(ctx, "any-ref"); return err }},
		{"StopSandbox", func() error { return r.StopSandbox(ctx, "any-ref") }},
		{"KillSandbox", func() error { return r.KillSandbox(ctx, "any-ref") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("expected ErrNotImplemented, got nil")
			}
			if !errors.Is(err, errNoneNotImplemented) {
				t.Errorf("expected errors.Is(err, errNoneNotImplemented); got %v", err)
			}
		})
	}
}

// TestRecordingCapabilityHonest: an honest recording driver (declares AND
// delivers) passes the recording gate with a probe that confirms the artifact.
func TestRecordingCapabilityHonest(t *testing.T) {
	r := newRecordingRunner()

	conformance.CheckRecordingCapability(t, r, conformance.RecordingOptions{
		Timeout:  5 * time.Second,
		ExitArgv: func(code int) []string { return []string{"sh", "-c", "exit 0"} },
		RecordingProbe: func(t *testing.T, ref string) {
			t.Helper()
			if !r.recorded[ref] {
				t.Errorf("recording driver declared SessionRecording but produced no artifact for ref %q", ref)
			}
		},
	})
}

// TestRecordingCapabilityNoneStub: the honest none runner declares NO recording,
// so the recording gate's honest-empty half runs and passes (no probe supplied,
// no pretending). This is the "doesn't pretend" leg of the contract.
func TestRecordingCapabilityNoneStub(t *testing.T) {
	conformance.CheckRecordingCapability(t, noneRunner{}, conformance.RecordingOptions{
		Timeout: 5 * time.Second,
	})
}

// ---------------------------------------------------------------------------
// Negative controls (the gate is NOT vacuous).
//
// A failing subtest ALWAYS marks its parent failed in Go's testing framework,
// and the conformance helpers take a concrete *testing.T (not an interface we
// could fake), so we cannot observe a real failure in-process without turning it
// into our own failure. We use the standard subprocess re-exec idiom instead:
// each negative control invokes a guarded helper test (which legitimately FAILS)
// in a fresh `go test` subprocess and asserts the subprocess exited non-zero.
// The guarded helpers are no-ops unless WARDYN_NEGCTL names them, so they pass
// trivially in a normal run and only FAIL when re-exec'd as the control's child.
// ---------------------------------------------------------------------------

const negctlEnv = "WARDYN_NEGCTL"

// TestConformanceCatchesSilentDowngrade proves the Capabilities conformance gate
// catches a dishonest driver that declares CC3 but silently enforces a weaker
// (empty/unrecognised) class — the positive control for invariant 5 (fail
// closed). The honest none runner passes the same gate because it claims
// nothing; this liar must fail it.
func TestConformanceCatchesSilentDowngrade(t *testing.T) {
	assertNegativeControlFails(t, "TestNegCtl_SilentDowngrade",
		"conformance Capabilities gate did NOT catch a silent confinement-class downgrade; invariant 5 not enforced")
}

// TestRecordingCapabilityCatchesPretender proves the recording gate catches a
// driver that DECLARES SessionRecording but delivers no artifact.
func TestRecordingCapabilityCatchesPretender(t *testing.T) {
	assertNegativeControlFails(t, "TestNegCtl_RecordingPretender",
		"recording gate did NOT catch a driver that declares SessionRecording but produces no artifact")
}

// TestRecordingCapabilityRejectsProbedNonRecorder proves the fail-closed posture
// on the false-declaring half: a non-recording driver must not be probed as if
// it records — the gate refuses to let a SessionRecording=false driver be
// treated as a recorder.
func TestRecordingCapabilityRejectsProbedNonRecorder(t *testing.T) {
	assertNegativeControlFails(t, "TestNegCtl_ProbedNonRecorder",
		"recording gate accepted a probe against a SessionRecording=false driver; a non-recorder must not be treated as a recorder")
}

// TestNegCtl_SilentDowngrade is the guarded child of
// TestConformanceCatchesSilentDowngrade. It runs the conformance suite against
// the dishonest downgradingRunner, which MUST fail the Capabilities check. It is
// a no-op unless re-exec'd with WARDYN_NEGCTL set to its name.
func TestNegCtl_SilentDowngrade(t *testing.T) {
	if os.Getenv(negctlEnv) != "TestNegCtl_SilentDowngrade" {
		return // not the targeted child; pass trivially in a normal run
	}
	conformance.Run(t, downgradingRunner{}, conformance.Options{Timeout: 5 * time.Second})
}

// TestNegCtl_RecordingPretender is the guarded child of
// TestRecordingCapabilityCatchesPretender. It runs the recording gate against a
// driver that declares SessionRecording but produces no artifact; the probe MUST
// fail it.
func TestNegCtl_RecordingPretender(t *testing.T) {
	if os.Getenv(negctlEnv) != "TestNegCtl_RecordingPretender" {
		return
	}
	conformance.CheckRecordingCapability(t, pretendingRecordingRunner{}, conformance.RecordingOptions{
		Timeout:  5 * time.Second,
		ExitArgv: func(code int) []string { return []string{"sh", "-c", "exit 0"} },
		RecordingProbe: func(t *testing.T, ref string) {
			t.Helper()
			// The pretender records nothing, so the artifact never exists; a real
			// probe would find no cast uploaded / written.
			t.Errorf("no recording artifact found for ref %q (driver declared SessionRecording but did not deliver)", ref)
		},
	})
}

// TestNegCtl_ProbedNonRecorder is the guarded child of
// TestRecordingCapabilityRejectsProbedNonRecorder. It supplies a probe to a
// driver that declares SessionRecording=false; the gate MUST reject the misuse.
func TestNegCtl_ProbedNonRecorder(t *testing.T) {
	if os.Getenv(negctlEnv) != "TestNegCtl_ProbedNonRecorder" {
		return
	}
	conformance.CheckRecordingCapability(t, noneRunner{}, conformance.RecordingOptions{
		Timeout:        5 * time.Second,
		RecordingProbe: func(t *testing.T, ref string) { t.Helper() }, // misuse: probe a non-recorder
	})
}

// assertNegativeControlFails re-runs THIS test binary, targeting only the named
// guarded helper test with WARDYN_NEGCTL set so it actually executes. The helper
// is expected to FAIL (the gate fires), so the subprocess must exit non-zero. A
// zero exit means the gate did not catch the dishonest driver — wantFailMsg is
// reported. Using -count=1 avoids a cached pass masking a regression.
func assertNegativeControlFails(t *testing.T, child, wantFailMsg string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^"+child+"$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), negctlEnv+"="+child)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Errorf("%s\nnegative-control subprocess %q unexpectedly PASSED; output:\n%s", wantFailMsg, child, out)
		return
	}
	// Sanity: a non-zero exit must be a test failure (exit error), not a build
	// or harness error, so the control actually exercised the gate.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("negative-control subprocess %q did not run cleanly: %v\noutput:\n%s", child, err, out)
	}
}
