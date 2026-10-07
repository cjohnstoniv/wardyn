// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testfloor"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type signInFixtureCase struct {
	Name string            `json:"name"`
	Pane string            `json:"pane"`
	Go   runSignInResponse `json:"go"`
	TS   *string           `json:"ts"`
}

// signInFixtures reads the cases login-pty-extract.test.ts reads too.
func signInFixtures(t *testing.T) []signInFixtureCase {
	t.Helper()
	b, err := os.ReadFile("testdata/sign_in_pane_fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []signInFixtureCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) < 4 {
		t.Fatalf("%d fixture cases, want the waiting, completed, failed and two-attempts cases at least", len(doc.Cases))
	}
	return doc.Cases
}

// captureProbeStore answers the capture-row probe the way the PG store does.
type captureProbeStore struct {
	store.Store
	captured bool
	err      error

	mu     sync.Mutex
	probes []store.AuditFilter
}

func (s *captureProbeStore) HasRunAuditEvent(_ context.Context, _ uuid.UUID, f store.AuditFilter) (bool, error) {
	s.mu.Lock()
	s.probes = append(s.probes, f)
	s.mu.Unlock()
	return s.captured, s.err
}

// awsSignInFixture is a pane fixture whose run is the admin token's own AWS
// sign-in run, on a store that can probe the capture row.
func awsSignInFixture(t *testing.T, pane func(runner.ExecSpec) (*runner.ExecSession, error), probe *captureProbeStore) *paneFixture {
	t.Helper()
	f := newPaneFixture(t, pane, func(c *Config) {
		if probe != nil {
			probe.Store = c.Store
			c.Store = probe
		}
	})
	f.st.mu.Lock()
	f.st.run.Task, f.st.run.Agent = harnessLoginTask, awsSSOAgent
	f.st.run.CreatedBy, f.st.run.OperatorOwned = "admin-token", true
	f.st.mu.Unlock()
	return f
}

func (f *paneFixture) signIn(t *testing.T) (int, string) {
	t.Helper()
	w := do(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/sign-in", adminToken, "")
	return w.Code, w.Body.String()
}

// Each shared fixture case, read through the route on a fake ExecStream,
// answers the case's Go answer, and neither a log line nor an audit row
// carries the pane or the code.
func TestRunSignIn_SharedFixture(t *testing.T) {
	logs := captureSlog(t)
	for _, tc := range signInFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			if link, code, ok := parseSignInPane([]byte(tc.Pane)); ok != (tc.Go.State == signInStateWaiting) ||
				link != tc.Go.VerificationURL || code != tc.Go.UserCode {
				t.Fatalf("parseSignInPane = %q %q %v, want %+v", link, code, ok, tc.Go)
			}
			f := awsSignInFixture(t, textPane(tc.Pane, 0), &captureProbeStore{})
			code, body := f.signIn(t)
			if code != http.StatusOK {
				t.Fatalf("GET sign-in: %d %s", code, body)
			}
			var got runSignInResponse
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatal(err)
			}
			if got != tc.Go {
				t.Fatalf("answer = %+v, want %+v", got, tc.Go)
			}
			if execs := f.pr.execs(); len(execs) != 1 || strings.Join(execs[0].Argv, " ") != strings.Join(paneSnapshotArgv, " ") {
				t.Fatalf("execs = %+v, want one tmux capture-pane", execs)
			}
			f.audit.mu.Lock()
			rows := slices.Clone(f.audit.events)
			f.audit.mu.Unlock()
			for _, ev := range rows {
				if strings.Contains(string(ev.Data), "user_code") || strings.Contains(ev.Target, "user_code") {
					t.Errorf("an audit row carries the pane: %+v", ev)
				}
			}
		})
	}
	if strings.Contains(logs.String(), "user_code") || strings.Contains(logs.String(), "amazonaws") {
		t.Errorf("a log line carries the pane: %s", logs.String())
	}
}

// Every completion or failure line the parser stops at is printed, verbatim,
// by the source it names.
func TestSignInPane_EndLinesMatchTheirSources(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	srcs := read("../../deploy/images/aws-sso/login-hint.sh") + read("../../cmd/wardyn-aws-sso/main.go")
	for _, line := range signInEndLines[1:] { // [0] is the AWS CLI's own line
		if !strings.Contains(srcs, line) {
			t.Errorf("no source prints %q", line)
		}
	}
}

// A run whose sign-in this run already captured answers not_waiting whatever
// the pane says, from the capture's audit row, and nothing is executed.
func TestRunSignIn_CapturedRunIsNotWaiting(t *testing.T) {
	waiting := signInFixtures(t)[0].Pane
	probe := &captureProbeStore{captured: true}
	f := awsSignInFixture(t, textPane(waiting, 0), probe)
	if code, body := f.signIn(t); code != http.StatusOK || !strings.Contains(body, `"state":"not_waiting"`) {
		t.Fatalf("captured run: %d %s, want 200 not_waiting", code, body)
	}
	if n := len(f.pr.execs()); n != 0 {
		t.Errorf("%d execs after the capture, want none", n)
	}
	want := store.AuditFilter{Action: "harness.credential.capture", Outcome: "success"}
	if len(probe.probes) != 1 || probe.probes[0] != want {
		t.Errorf("probes = %+v, want one for %+v", probe.probes, want)
	}

	// A store that cannot answer is unreadable, never a guess either way.
	f = awsSignInFixture(t, textPane(waiting, 0), nil)
	if code, body := f.signIn(t); code != http.StatusServiceUnavailable || !strings.Contains(body, `"reason":"run_sign_in_unreadable"`) {
		t.Fatalf("no probe: %d %s, want 503 run_sign_in_unreadable", code, body)
	}
}

// Not RUNNING, not dispatched, or not an AWS sign-in: no exec.
func TestRunSignIn_OnlyARunningAWSSignInIsRead(t *testing.T) {
	waiting := signInFixtures(t)[0].Pane
	f := awsSignInFixture(t, textPane(waiting, 0), &captureProbeStore{})
	f.st.mu.Lock()
	f.st.state = types.RunKilled
	f.st.mu.Unlock()
	if code, body := f.signIn(t); code != http.StatusOK || !strings.Contains(body, `"state":"not_waiting"`) {
		t.Fatalf("killed run: %d %s, want 200 not_waiting", code, body)
	}
	f = awsSignInFixture(t, textPane(waiting, 0), &captureProbeStore{})
	f.st.mu.Lock()
	f.st.run.Agent = "claude-code"
	f.st.mu.Unlock()
	if code, body := f.signIn(t); code != http.StatusConflict || !strings.Contains(body, `"reason":"run_sign_in_not_aws"`) {
		t.Fatalf("a Claude sign-in: %d %s, want 409 run_sign_in_not_aws", code, body)
	}
	if n := len(f.pr.execs()); n != 0 {
		t.Errorf("%d execs, want none", n)
	}
}

// The admin on a person's run is refused with run_owner_only, the security
// tier gets the 404, and the owner reaches the handler.
func TestRunSignIn_OwnerOnly(t *testing.T) {
	srv, run, _, ses := entryServer(t, false)
	path := "/api/v1/runs/" + run.ID.String() + "/sign-in"
	if w := doSSO(t, srv, http.MethodGet, path, ses["admin"], ""); !refusedOwnerOnly(w.Code, w.Body.String()) {
		t.Fatalf("a super admin on a person's run: %d %s, want the 403 run_owner_only body", w.Code, w.Body.String())
	}
	if w := do(t, srv, http.MethodGet, path, adminToken, ""); w.Code != http.StatusForbidden {
		t.Fatalf("the admin token on a person's run: %d %s, want 403", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodGet, path, ses["security"], ""); w.Code != http.StatusNotFound {
		t.Fatalf("the security tier: %d %s, want 404", w.Code, w.Body.String())
	}
	if w := doSSO(t, srv, http.MethodGet, path, ses["owner"], ""); w.Code != http.StatusConflict ||
		!strings.Contains(w.Body.String(), "run_sign_in_not_aws") {
		t.Fatalf("the owner: %d %s, want the handler's own 409 for a run that is not a sign-in", w.Code, w.Body.String())
	}
}

// endless is a reader that never ends until closed.
type endless struct {
	b      byte
	closed chan struct{}
}

func (e *endless) Read(p []byte) (int, error) {
	select {
	case <-e.closed:
		return 0, io.EOF
	default:
	}
	for i := range p {
		p[i] = e.b
	}
	return len(p), nil
}

// blocked is a reader or a wait that returns only once closed.
func blocked(closed chan struct{}) io.Reader {
	pr, pw := io.Pipe()
	go func() { <-closed; _ = pw.Close() }()
	return pr
}

// boundSession builds a session whose Close is recorded.
type boundSession struct {
	closed chan struct{}
	once   sync.Once
}

func (b *boundSession) close() error { b.once.Do(func() { close(b.closed) }); return nil }

func (b *boundSession) wasClosed() bool {
	select {
	case <-b.closed:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// The read is bounded on every axis: endless stdout overflows the cap,
// stderr is drained and discarded, a wait that never returns hits the
// deadline, and a cancelled request ends the read. Each closes the exec.
func TestRunSignIn_Bounds(t *testing.T) {
	waiting := signInFixtures(t)[0].Pane
	for _, tc := range []struct {
		name     string
		sess     func(b *boundSession) *runner.ExecSession
		cancel   bool
		wantCode int
		wantBody string
	}{
		{"endless stdout", func(b *boundSession) *runner.ExecSession {
			return &runner.ExecSession{Stdout: &endless{b: 'x', closed: b.closed}, Stderr: strings.NewReader(""),
				Wait: func() (int, error) { <-b.closed; return 0, nil }, Close: b.close}
		}, false, http.StatusServiceUnavailable, `"reason":"run_sign_in_unreadable"`},
		{"stderr only", func(b *boundSession) *runner.ExecSession {
			return &runner.ExecSession{Stdout: strings.NewReader(""), Stderr: io.MultiReader(strings.NewReader(waiting), &endless{b: 'e', closed: b.closed}),
				Wait: func() (int, error) { return 0, nil }, Close: b.close}
		}, false, http.StatusOK, `"state":"not_waiting"`},
		{"a blocked wait", func(b *boundSession) *runner.ExecSession {
			return &runner.ExecSession{Stdout: strings.NewReader(waiting), Stderr: strings.NewReader(""),
				Wait: func() (int, error) { <-b.closed; return 0, nil }, Close: b.close}
		}, false, http.StatusServiceUnavailable, `"reason":"run_sign_in_unreadable"`},
		{"a cancelled request", func(b *boundSession) *runner.ExecSession {
			return &runner.ExecSession{Stdout: blocked(b.closed), Stderr: blocked(b.closed),
				Wait: func() (int, error) { <-b.closed; return 0, nil }, Close: b.close}
		}, true, http.StatusServiceUnavailable, `"reason":"run_sign_in_unreadable"`},
		{"a non-zero exit", func(b *boundSession) *runner.ExecSession {
			return &runner.ExecSession{Stdout: strings.NewReader(waiting), Stderr: strings.NewReader(""),
				Wait: func() (int, error) { return 1, nil }, Close: b.close}
		}, false, http.StatusServiceUnavailable, `"reason":"run_sign_in_unreadable"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			b := &boundSession{closed: make(chan struct{})}
			f := awsSignInFixture(t, func(runner.ExecSpec) (*runner.ExecSession, error) { return tc.sess(b), nil }, &captureProbeStore{})
			f.srv.paneSnapshotTimeoutOverride = 300 * time.Millisecond
			if tc.cancel {
				f.srv.paneSnapshotTimeoutOverride = time.Minute
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/sign-in", nil)
			r.Header.Set("Authorization", "Bearer "+adminToken)
			r.Host, r.RemoteAddr = "127.0.0.1", "127.0.0.1:54321"
			if tc.cancel {
				time.AfterFunc(100*time.Millisecond, cancel)
			}
			w := httptest.NewRecorder()
			start := time.Now()
			f.srv.Handler().ServeHTTP(w, r)
			if took := time.Since(start); took > 5*time.Second {
				t.Fatalf("the read took %v", took)
			}
			if w.Code != tc.wantCode || !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Fatalf("%d %s, want %d %s", w.Code, w.Body.String(), tc.wantCode, tc.wantBody)
			}
			if !b.wasClosed() {
				t.Error("the exec session was not closed")
			}
			for _, leak := range []string{"xxxx", "eeee", "user_code", "ABCD-EFGH"} {
				if strings.Contains(w.Body.String(), leak) || strings.Contains(logs.String(), leak) {
					t.Errorf("pane text %q reached the answer or a log line", leak)
				}
			}
		})
	}
}

// The capture-row evidence against Postgres: only this run's successful
// capture row counts.
func TestRunSignIn_CaptureRowEvidencePG(t *testing.T) {
	testfloor.Mark(t, "pg")
	pool := throwawayPGPool(t)
	pg := store.PG{Pool: pool}
	ctx := t.Context()
	newRun := func() uuid.UUID {
		r := newFinalizeRun()
		r.Task, r.Agent = harnessLoginTask, awsSSOAgent
		created, err := pg.CreateRun(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	run, other := newRun(), newRun()
	srv := &Server{cfg: Config{Store: pg}}
	captured := func() bool {
		got, err := srv.signInCaptured(ctx, run)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	record := func(runID uuid.UUID, action, outcome string) {
		ev := newTestEvent(action)
		ev.RunID, ev.Outcome, ev.Time = &runID, outcome, time.Now().UTC()
		if err := (store.Recorder{Pool: pool}).Record(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	if captured() {
		t.Fatal("captured before any row")
	}
	record(other, "harness.credential.capture", "success")
	record(run, "harness.credential.capture", "failure")
	record(run, "secret.read", "success")
	if captured() {
		t.Fatal("captured from another run's row, a failed capture or another action")
	}
	record(run, "harness.credential.capture", "success")
	if !captured() {
		t.Fatal("not captured after this run's capture row")
	}
}
