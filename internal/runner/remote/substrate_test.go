// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const rid = "11111111-2222-3333-4444-555555555555"

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func create(t *testing.T, rig *runnertest.Rig, mut func(*runner.SandboxSpec)) (runner.Sandbox, runner.SandboxSpec) {
	t.Helper()
	spec := runner.SandboxSpec{RunID: uuid.New(), Image: "img", ConfinementClass: types.CC1}
	if mut != nil {
		mut(&spec)
	}
	sb, err := rig.Sub.CreateSandbox(ctxT(t), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	return sb, spec
}

func TestNameAndClassesServedFromCaps(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	if got := rig.Sub.Name(); got != "runner:"+rid || !substrate.IsRemote(rig.Sub) {
		t.Fatalf("Name = %q", got)
	}
	cs, err := rig.Sub.Classes(ctxT(t))
	if err != nil || len(cs.Classes) != 2 || cs.Classes[0] != types.CC1 {
		t.Fatalf("Classes = %+v, %v", cs, err)
	}
	caps, _ := rig.Sub.Caps()
	if caps.CPUMillisMax != 4000 || caps.MemoryMiBMax != 8192 || caps.AllowedRoots[0] != "/work" || caps.Version != "test" {
		t.Fatalf("caps = %+v", caps)
	}
}

func TestCreateSandboxRoundTripWaitingEventsBeforeReturn(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	var details []string
	sb, spec := create(t, rig, func(s *runner.SandboxSpec) {
		s.OnWaiting = func(d string) { details = append(details, d) } // unsynchronised: the caller's goroutine, under -race
		s.Labels = map[string]string{"k": "v"}
		s.Resources = runner.Resources{CPUMillis: 1500}
		s.SecretEnv = map[string]string{"TOKEN": placement.ResidentPlaceholder(uuid.New())}
	})
	if !strings.HasPrefix(sb.Ref, "runner:"+rid+"/") {
		t.Fatalf("ref %q is not prefixed with the runner", sb.Ref)
	}
	if len(details) != 2 || details[0] != "pulling image" || details[1] != "starting sandbox" {
		t.Fatalf("waiting reports = %v", details)
	}
	got := rig.Fake.Created[0]
	if got.RunID != spec.RunID || got.Labels["k"] != "v" || got.Resources.CPUMillis != 1500 || got.SecretEnv["TOKEN"] != spec.SecretEnv["TOKEN"] {
		t.Fatalf("the spec did not round-trip: %+v", got)
	}
}

func TestCreateSandboxRefusesAClassTheRunnerDoesNotOffer(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	_, err := rig.Sub.CreateSandbox(ctxT(t), runner.SandboxSpec{RunID: uuid.New(), ConfinementClass: types.CC3})
	if err == nil || len(rig.Fake.Created) != 0 {
		t.Fatalf("CC3 create = %v, created %d", err, len(rig.Fake.Created))
	}
}

func TestExecWaitStatusRoundTripAndTypedErrors(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	rig.Fake.ExitCode = 42
	sb, _ := create(t, rig, nil)
	ctx := ctxT(t)

	if _, err := rig.Sub.Wait(ctx, sb.Ref); !errors.Is(err, runner.ErrExecNeverStarted) {
		t.Fatalf("Wait before Exec = %v, want ErrExecNeverStarted", err)
	}
	id, err := rig.Sub.Exec(ctx, sb.Ref, []string{"agent"})
	if err != nil || !strings.HasPrefix(id, "exec-") {
		t.Fatalf("Exec = %q, %v", id, err)
	}
	if code, err := rig.Sub.Wait(ctx, sb.Ref); err != nil || code != 42 {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	st, err := rig.Sub.Status(ctx, sb.Ref)
	if err != nil || st.State != types.RunRunning {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	if st, err = rig.Sub.AgentStatus(ctx, sb.Ref, id); err != nil || st.State != types.RunRunning {
		t.Fatalf("AgentStatus = %+v, %v", st, err)
	}
	if err := rig.Sub.KillSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := rig.Sub.Status(ctx, sb.Ref); !errors.Is(err, runner.ErrSandboxGone) {
		t.Fatalf("Status after kill = %v, want ErrSandboxGone", err)
	}
}

func TestAttachRoundTrip(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	sess, err := rig.Sub.Attach(ctxT(t), sb.Ref, runner.AttachOptions{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Write([]byte("ls\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, err := io.ReadAtLeast(sess, buf, 3)
	if err != nil || string(buf[:n]) != "ls\n" {
		t.Fatalf("pty echo = %q, %v", buf[:n], err)
	}
	if err := sess.Resize(ctxT(t), 120, 40); err != nil {
		t.Fatal(err)
	}
	if len(rig.Fake.Resizes) != 1 || rig.Fake.Resizes[0] != [2]uint16{120, 40} {
		t.Fatalf("resizes = %v", rig.Fake.Resizes)
	}
	// Close resets the stream only: the sandbox is still there.
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.Sub.Status(ctxT(t), sb.Ref); err != nil {
		t.Fatalf("the sandbox did not survive the session's Close: %v", err)
	}
	if _, err := rig.Sub.Attach(ctxT(t), "runner:other/x", runner.AttachOptions{}); !errors.Is(err, remote.ErrForeignRef) {
		t.Fatalf("a foreign ref = %v", err)
	}
}

func TestExecStreamRoundTrip(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	rig.Fake.ExitCode = 3
	sb, _ := create(t, rig, nil)
	es, err := rig.Sub.ExecStream(ctxT(t), sb.Ref, runner.ExecSpec{Argv: []string{"cat"}})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); io.Copy(&stderr, es.Stderr) }() // drained concurrently, as ExecSession requires
	if _, err := es.Stdin.Write([]byte("hello stream")); err != nil {
		t.Fatal(err)
	}
	if err := es.Stdin.Close(); err != nil { // half-close: stdout must stay readable
		t.Fatal(err)
	}
	out, err := io.ReadAll(es.Stdout)
	if err != nil || string(out) != "hello stream" {
		t.Fatalf("stdout = %q, %v", out, err)
	}
	if code, err := es.Wait(); err != nil || code != 3 {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	wg.Wait()
	if stderr.String() != "argv:cat\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if err := es.Resize(10, 20); err != nil {
		t.Fatal(err)
	}
	_ = es.Close()
}

func TestExecStreamTTYHasNoStderrStream(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	es, err := rig.Sub.ExecStream(ctxT(t), sb.Ref, runner.ExecSpec{Argv: []string{"sh"}, TTY: true})
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	if es.Stderr != nil {
		t.Fatal("a TTY exec has one merged stream")
	}
}

type lockedBuf struct {
	mu   sync.Mutex
	b    bytes.Buffer
	done chan struct{}
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) BeginDrain()    {}
func (l *lockedBuf) EndDrain(error) { close(l.done) }

func TestExecOutputStreamsIntoTheCallersWriter(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	w := &lockedBuf{done: make(chan struct{})}
	sb, _ := create(t, rig, func(s *runner.SandboxSpec) { s.ExecOutput = w })
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the output drain never ended")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.b.String() != "agent output of fake-"+strings.TrimPrefix(sb.Ref, "runner:"+rid+"/fake-")+"\n" {
		t.Fatalf("output = %q", w.b.String())
	}
}

func TestOptionalVerbsRoundTrip(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	ctx := ctxT(t)
	local := strings.TrimPrefix(sb.Ref, "runner:"+rid+"/")
	for name, err := range map[string]error{
		"end":        rig.Sub.EndSandbox(ctx, sb.Ref),
		"stop_proxy": rig.Sub.StopProxy(ctx, sb.Ref),
		"can":        rig.Sub.CanReplaceProxy(ctx, sb.Ref),
		"replace":    rig.Sub.ReplaceProxy(ctx, sb.Ref, []byte(`{"stripped":true}`)),
		"ensure":     rig.Sub.EnsureProxyImage(ctx),
		"start":      rig.Sub.StartSandbox(ctx, sb.Ref),
	} {
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, verb := range []string{"end", "stop_proxy", "can_replace", `replace_proxy {"stripped":true}`, "start"} {
		if !rig.Fake.Called(verb, local) {
			t.Errorf("the runner never saw %q", verb)
		}
	}
	if !rig.Fake.Called("ensure_proxy_image", "") {
		t.Error("the runner never saw ensure_proxy_image")
	}
	if got := rig.Sub.ProbeSubstrate(ctx); got != runner.SubstrateOK {
		t.Errorf("ProbeSubstrate = %q", got)
	}
	p, err := rig.Sub.ProbeDrive(ctx, types.DriveMount{})
	if err != nil || p.Result != runner.DriveProbeReadable {
		t.Errorf("ProbeDrive = %+v, %v", p, err)
	}
	var out bytes.Buffer
	if err := rig.Sub.RecoverOutput(ctx, sb.Ref, &out); err != nil || out.String() != "recovered "+local {
		t.Errorf("RecoverOutput = %q, %v", out.String(), err)
	}
	if err := rig.Sub.StopSandbox(ctx, sb.Ref); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestSweepIsTwoCallsBecauseAFuncCannotCrossTheWire(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	keep, drop := uuid.New(), uuid.New()
	rig.Fake.Orphans = []uuid.UUID{keep, drop}
	n, err := rig.Sub.SweepOrphanedSandboxes(ctxT(t), time.Minute, func(id uuid.UUID) bool { return id == drop })
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if len(rig.Fake.Orphans) != 1 || rig.Fake.Orphans[0] != keep {
		t.Fatalf("the runner removed the wrong runs: %v", rig.Fake.Orphans)
	}
}

func TestTeardownWhileOfflineIsQueuedNotFailed(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	rig.Org.Drop()
	waitOffline(t, rig)
	ctx := ctxT(t)

	if err := rig.Sub.KillSandbox(ctx, sb.Ref); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("offline Kill = %v, want ErrPendingOnRunner", err)
	}
	if err := rig.Sub.StopSandbox(ctx, sb.Ref); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("offline Stop = %v, want ErrPendingOnRunner", err)
	}
	if err := rig.Sub.StopProxy(ctx, sb.Ref); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("offline StopProxy = %v, want ErrPendingOnRunner", err)
	}
	want := []string{"kill " + sb.Ref, "end " + sb.Ref, "stop_proxy " + sb.Ref}
	if got := rig.Queued(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("queued = %v, want %v", got, want)
	}
	// Reads never guess: offline is offline.
	if _, err := rig.Sub.Status(ctx, sb.Ref); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("offline Status = %v", err)
	}
	if _, err := rig.Sub.Classes(ctx); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("offline Classes = %v", err)
	}
	if _, err := rig.Sub.CreateSandbox(ctx, runner.SandboxSpec{RunID: uuid.New()}); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("offline create = %v", err)
	}
	if got := rig.Sub.ProbeSubstrate(ctx); got != runner.SubstrateUnreachable {
		t.Fatalf("offline ProbeSubstrate = %q", got)
	}
	// Reconnect: calls work again on a fresh session.
	rig.Connect(t)
	if _, err := rig.Sub.Status(ctx, sb.Ref); err != nil {
		t.Fatalf("Status after reconnect: %v", err)
	}
}

func TestWaitAcrossALinkDropIsRunnerOffline(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	sb, _ := create(t, rig, nil)
	if _, err := rig.Sub.Exec(ctxT(t), sb.Ref, []string{"a"}); err != nil {
		t.Fatal(err)
	}
	rig.Org.SetDelay(200 * time.Millisecond)
	errc := make(chan error, 1)
	go func() { _, err := rig.Sub.Wait(ctxT(t), sb.Ref); errc <- err }()
	time.Sleep(50 * time.Millisecond)
	rig.Org.Drop()
	if err := <-errc; !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("Wait across a drop = %v, want ErrRunnerOffline", err)
	}
}

func waitOffline(t *testing.T, rig *runnertest.Rig) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for rig.Link.Online() {
		if time.Now().After(deadline) {
			t.Fatal("the link never went offline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// DeliverResident is the only road for a resident value; a CreateSandbox that
// would carry one is refused before anything is sent.
func TestCreateSandboxNeverCarriesAResidentValue(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	run, grant := uuid.New(), uuid.New()
	const secret = "s3cr3t-resident-value"
	if err := rig.Sub.DeliverResident(ctxT(t), runnerwire.DeliverResidentArgs{RunID: run, GrantID: grant, Kind: "env_secret", Value: secret, NotAfter: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got := rig.Server.Resident[run]; len(got) != 1 || got[0].Value != secret || got[0].GrantID != grant {
		t.Fatalf("the runner's keystore did not receive the value: %+v", got)
	}

	// The spec names the grant by placeholder: sent.
	ok := runner.SandboxSpec{RunID: run, Image: "img", ConfinementClass: types.CC1, SecretEnv: map[string]string{"TOKEN": placement.ResidentPlaceholder(grant)}}
	sb, err := rig.Sub.CreateSandbox(ctxT(t), ok)
	if err != nil {
		t.Fatalf("a placeholder spec was refused: %v", err)
	}
	if blob := marshalSent(t, rig, run); strings.Contains(blob, secret) {
		t.Fatalf("the CreateSandbox payload carries the resident value: %s", blob)
	}
	_ = sb

	// A spec that carries the value itself is refused; no create reaches the runner.
	before := len(rig.Fake.Created)
	leak := ok
	leak.RunID = run
	leak.SecretEnv = map[string]string{"TOKEN": secret}
	if _, err := rig.Sub.CreateSandbox(ctxT(t), leak); !errors.Is(err, remote.ErrResidentLeak) {
		t.Fatalf("a spec carrying the resident value = %v, want ErrResidentLeak", err)
	}
	if len(rig.Fake.Created) != before {
		t.Fatal("the leaking create reached the runner")
	}

	if err := rig.Sub.EraseResident(ctxT(t), run); err != nil {
		t.Fatal(err)
	}
	if len(rig.Server.Erased) != 1 || len(rig.Server.Resident[run]) != 0 {
		t.Fatal("the runner did not erase the run's resident values")
	}
}

// marshalSent is the CreateSandbox spec the runner received, re-marshalled: what crossed the wire.
func marshalSent(t *testing.T, rig *runnertest.Rig, run uuid.UUID) string {
	t.Helper()
	for _, s := range rig.Fake.Created {
		if s.RunID == run {
			b, err := runnerwire.Marshal(runnerwire.CreateSandboxArgs{Spec: s})
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	t.Fatal("no create seen")
	return ""
}

// The optional-interface list is closed (design §3.4): anything not implemented
// is refused by the capability intersection, never half-served.
func TestOptionalInterfaceListIsPinned(t *testing.T) {
	var s any = remote.New(rid, &remote.Link{}, remote.Options{})
	implemented := map[string]bool{
		"SandboxEnder":    asserts[runner.SandboxEnder](s),
		"ProxyStopper":    asserts[runner.ProxyStopper](s),
		"ProxyReviver":    asserts[runner.ProxyReviver](s),
		"SandboxStarter":  asserts[runner.SandboxStarter](s),
		"SubstrateProber": asserts[runner.SubstrateProber](s),
		"DriveProber":     asserts[runner.DriveProber](s),
		"OutputRecoverer": asserts[runner.OutputRecoverer](s),
		"Sweeper": asserts[interface {
			SweepOrphanedSandboxes(context.Context, time.Duration, func(uuid.UUID) bool) (int, error)
		}](s),
	}
	unsupported := map[string]bool{
		"Freezer":         asserts[runner.Freezer](s),
		"ActivitySampler": asserts[runner.ActivitySampler](s),
		"ImageChecker":    asserts[runner.ImageChecker](s),
		"ImageRemover":    asserts[runner.ImageRemover](s),
		"DriveReclaimer":  asserts[runner.DriveReclaimer](s),
		"FitChecker":      asserts[runner.FitChecker](s),
	}
	for name, has := range implemented {
		if !has {
			t.Errorf("%s is on the 0.9 list and not implemented", name)
		}
	}
	for name, has := range unsupported {
		if has {
			t.Errorf("%s must stay unsupported in 0.9 (design §3.4)", name)
		}
	}
	if len(implemented) != 8 || len(unsupported) != 6 {
		t.Fatal("the design lists eight implemented and six unsupported")
	}
}

func asserts[T any](v any) bool { _, ok := v.(T); return ok }

func deliver(t *testing.T, rig *runnertest.Rig, run uuid.UUID, value string) {
	t.Helper()
	if err := rig.Sub.DeliverResident(ctxT(t), runnerwire.DeliverResidentArgs{RunID: run, GrantID: uuid.New(), Kind: "file_secret", Value: value, NotAfter: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
}

// A file_secret resident value rides ManagedFile.Content, a []byte that marshals as base64:
// the guard compares leaves, so the encoding hides nothing.
func TestResidentGuardSeesManagedFileContent(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	run := uuid.New()
	const secret = "file-secret-value"
	deliver(t, rig, run, secret)
	spec := runner.SandboxSpec{RunID: run, Image: "img", ConfinementClass: types.CC1,
		ManagedFiles: []runner.ManagedFile{{Path: "/etc/x", Content: []byte(secret)}}}
	before := len(rig.Fake.Created)
	if _, err := rig.Sub.CreateSandbox(ctxT(t), spec); !errors.Is(err, remote.ErrResidentLeak) {
		t.Fatalf("a spec carrying the value as file content = %v, want ErrResidentLeak", err)
	}
	if len(rig.Fake.Created) != before {
		t.Fatal("the leaking create reached the runner")
	}
	// A value inside a map and one with a trailing newline are caught too.
	for name, mut := range map[string]func(*runner.SandboxSpec){
		"map value": func(s *runner.SandboxSpec) { s.Env = map[string]string{"K": secret} },
		"trailing nl": func(s *runner.SandboxSpec) {
			s.ManagedFiles = []runner.ManagedFile{{Path: "/x", Content: []byte(secret + "\n")}}
		},
		"nested label":   func(s *runner.SandboxSpec) { s.Labels = map[string]string{"a": secret} },
		"proxy injected": func(s *runner.SandboxSpec) { s.ProxyConfig.RunToken = secret },
	} {
		sp := runner.SandboxSpec{RunID: run, Image: "img", ConfinementClass: types.CC1}
		mut(&sp)
		if _, err := rig.Sub.CreateSandbox(ctxT(t), sp); !errors.Is(err, remote.ErrResidentLeak) {
			t.Errorf("%s: %v, want ErrResidentLeak", name, err)
		}
	}
}

// A short value must not refuse an innocent spec: leaves are compared whole, never as substrings.
func TestResidentGuardHasNoSubstringFalsePositive(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	run := uuid.New()
	deliver(t, rig, run, "img")
	spec := runner.SandboxSpec{RunID: run, Image: "registry.example/image:1", ConfinementClass: types.CC1,
		Labels: map[string]string{"wardyn.img": "imgs"}}
	if _, err := rig.Sub.CreateSandbox(ctxT(t), spec); err != nil {
		t.Fatalf("a spec merely containing the short value was refused: %v", err)
	}
}

func TestEraseResidentForgetsTheDigests(t *testing.T) {
	rig := runnertest.NewRig(t, rid)
	run := uuid.New()
	deliver(t, rig, run, "gone-after-erase")
	if err := rig.Sub.EraseResident(ctxT(t), run); err != nil {
		t.Fatal(err)
	}
	spec := runner.SandboxSpec{RunID: run, Image: "gone-after-erase", ConfinementClass: types.CC1}
	if _, err := rig.Sub.CreateSandbox(ctxT(t), spec); err != nil {
		t.Fatalf("after erase the guard still holds the value: %v", err)
	}
}
