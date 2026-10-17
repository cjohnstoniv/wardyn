// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// testClock is a settable Config.Now.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// runOutputServer is the matrix server with recordings OFF (no RecordingStore)
// and a settable clock, plus a seeder for runs owned by sub-member.
func runOutputServer(t *testing.T, shape ...func(*Config)) (*Server, *testClock, func(types.AgentRun) uuid.UUID) {
	t.Helper()
	clock := &testClock{now: time.Now().UTC()}
	shape = append([]func(*Config){func(c *Config) {
		c.RecordingStore = nil
		c.Now = clock.Now
	}}, shape...)
	srv, ast, _, _ := newAuthzMatrixServer(t, shape...)
	seed := func(run types.AgentRun) uuid.UUID {
		run.ID = uuid.New()
		if run.CreatedBy == "" {
			run.CreatedBy = "sub-member"
		}
		if run.State == "" {
			run.State = types.RunCompleted
		}
		ast.mu.Lock()
		ast.runs[run.ID] = run
		ast.mu.Unlock()
		return run.ID
	}
	return srv, clock, seed
}

func outputOwnerCookie(t *testing.T) *http.Cookie {
	return ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
}

func getRunOutput(t *testing.T, srv *Server, id uuid.UUID, query string, cookie *http.Cookie) (int, runOutputResponse, errorBody) {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+id.String()+"/output"+query, cookie, "")
	var ok runOutputResponse
	var refusal errorBody
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &ok); err != nil {
			t.Fatalf("decode 200 body %q: %v", w.Body, err)
		}
	} else if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode %d body %q: %v", w.Code, w.Body, err)
	}
	return w.Code, ok, refusal
}

func writeExecOutput(t *testing.T, w io.Writer, chunks ...string) {
	t.Helper()
	if w == nil {
		t.Fatal("openExecOutput returned no writer for an exec run")
	}
	for _, c := range chunks {
		if n, err := w.Write([]byte(c)); err != nil || n != len(c) {
			t.Fatalf("Write(%q) = %d, %v; a tail write must never fail", c, n, err)
		}
	}
}

// TestRunOutput_OwnerReadsCompletedExecRunWithRecordingsOff is #1232's first
// done-when: no recording store at all, and the owner still reads the end of
// their finished exec run.
func TestRunOutput_OwnerReadsCompletedExecRunWithRecordingsOff(t *testing.T) {
	srv, _, seed := runOutputServer(t)
	if srv.cfg.RecordingStore != nil {
		t.Fatal("fixture has a recording store; this test is about recordings off")
	}
	id := seed(types.AgentRun{})
	writeExecOutput(t, srv.openExecOutput(types.AgentRun{ID: id}, false), "go test ./...\n", "ok  pkg 0.1s\n")
	srv.finishRunOutput(t.Context(), id) // complete means a final capture

	code, got, _ := getRunOutput(t, srv, id, "", outputOwnerCookie(t))
	if code != http.StatusOK {
		t.Fatalf("owner: status %d, want 200", code)
	}
	want := runOutputResponse{Output: "go test ./...\nok  pkg 0.1s\n", Truncated: false, Complete: true, Source: "stdout"}
	if got != want {
		t.Fatalf("owner: got %+v, want %+v", got, want)
	}
}

// TestRunOutput_LiveRunIsNotComplete: a RUNNING exec run answers what it has so far.
func TestRunOutput_LiveRunIsNotComplete(t *testing.T) {
	srv, _, seed := runOutputServer(t)
	id := seed(types.AgentRun{State: types.RunRunning})
	writeExecOutput(t, srv.openExecOutput(types.AgentRun{ID: id}, false), "step 1\n")
	code, got, _ := getRunOutput(t, srv, id, "", outputOwnerCookie(t))
	if code != http.StatusOK || got.Output != "step 1\n" || got.Complete {
		t.Fatalf("live run: status %d body %+v, want 200 with step 1 and complete=false", code, got)
	}
}

// TestRunOutput_ForeignMemberGets404AdminReads is D-6: a member who cannot read
// the run gets GET /runs/{id}'s byte-identical 404 — for a foreign run and for
// an unknown one — and an admin reads the tail.
func TestRunOutput_ForeignMemberGets404AdminReads(t *testing.T) {
	srv, _, seed := runOutputServer(t)
	id := seed(types.AgentRun{CreatedBy: "sub-other-member"})
	writeExecOutput(t, srv.openExecOutput(types.AgentRun{ID: id}, false), "secret-free output\n")
	member := outputOwnerCookie(t)

	for name, rid := range map[string]uuid.UUID{"foreign": id, "unknown": uuid.New()} {
		out := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+rid.String()+"/output", member, "")
		get := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+rid.String(), member, "")
		if out.Code != http.StatusNotFound || get.Code != http.StatusNotFound {
			t.Fatalf("%s run: output %d, GET run %d; want 404 for both", name, out.Code, get.Code)
		}
		if out.Body.String() != get.Body.String() {
			t.Fatalf("%s run: output body %q differs from GET /runs/{id}'s %q — an existence oracle", name, out.Body, get.Body)
		}
	}

	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	code, got, _ := getRunOutput(t, srv, id, "", admin)
	if code != http.StatusOK || got.Output != "secret-free output\n" {
		t.Fatalf("admin: status %d body %+v, want 200 with the tail", code, got)
	}
}

// TestRunOutput_InteractiveRunRefused: an interactive run is refused with its
// own reason, even when (by some other path) a tail exists for it — the route
// never serves a terminal.
func TestRunOutput_InteractiveRunRefused(t *testing.T) {
	srv, _, seed := runOutputServer(t)
	id := seed(types.AgentRun{Interactive: true, State: types.RunRunning})
	if w := srv.openExecOutput(types.AgentRun{ID: id}, true); w != nil {
		t.Fatal("openExecOutput kept a tail for an interactive run")
	}
	code, _, refusal := getRunOutput(t, srv, id, "", outputOwnerCookie(t))
	if code != http.StatusConflict || refusal.Reason != reasonRunOutputInteractive {
		t.Fatalf("interactive: status %d reason %q, want 409 %s", code, refusal.Reason, reasonRunOutputInteractive)
	}
}

// TestRunOutput_CapAndTail pins the default bound: the ring keeps the LAST bytes,
// says it dropped some, and ?tail= is capped at the bound.
func TestRunOutput_CapAndTail(t *testing.T) {
	srv, _, seed := runOutputServer(t)
	id := seed(types.AgentRun{})
	w := srv.openExecOutput(types.AgentRun{ID: id}, false)
	// One oversized write and many small ones both have to land on the bound.
	writeExecOutput(t, w, strings.Repeat("a", 3*defaultRunOutputTailBytes))
	for i := 0; i < defaultRunOutputTailBytes/4; i++ {
		writeExecOutput(t, w, "bcd\n")
	}
	writeExecOutput(t, w, "END")
	all := strings.Repeat("a", 3*defaultRunOutputTailBytes) + strings.Repeat("bcd\n", defaultRunOutputTailBytes/4) + "END"
	wantAll := all[len(all)-defaultRunOutputTailBytes:]
	srv.finishRunOutput(t.Context(), id)

	cases := []struct {
		query string
		want  string
	}{
		{"", wantAll},
		{"?tail=100000", wantAll},
		{"?tail=3", "END"},
	}
	for _, tc := range cases {
		code, got, _ := getRunOutput(t, srv, id, tc.query, outputOwnerCookie(t))
		if code != http.StatusOK || got.Output != tc.want || !got.Truncated || !got.Complete {
			t.Fatalf("%q: status %d, len %d truncated %v complete %v; want 200, len %d, truncated, complete",
				tc.query, code, len(got.Output), got.Truncated, got.Complete, len(tc.want))
		}
	}

	small := seed(types.AgentRun{})
	writeExecOutput(t, srv.openExecOutput(types.AgentRun{ID: small}, false), "hello")
	if _, got, _ := getRunOutput(t, srv, small, "?tail=5", outputOwnerCookie(t)); got.Output != "hello" || got.Truncated {
		t.Fatalf("tail equal to the whole output: %+v, want hello, not truncated", got)
	}
	if _, got, _ := getRunOutput(t, srv, small, "?tail=2", outputOwnerCookie(t)); got.Output != "lo" || !got.Truncated {
		t.Fatalf("tail shorter than the output: %+v, want lo, truncated", got)
	}

	for _, bad := range []string{"?tail=0", "?tail=-1", "?tail=abc"} {
		if code, _, refusal := getRunOutput(t, srv, id, bad, outputOwnerCookie(t)); code != http.StatusBadRequest || refusal.Reason != reasonRunOutputTailInvalid {
			t.Fatalf("%s: status %d reason %q, want 400 %s", bad, code, refusal.Reason, reasonRunOutputTailInvalid)
		}
	}
}

// TestRunOutput_TTLExpiryDropsTheBuffer: TTL after the last output the tail is
// released and a read says it expired; after a second TTL even that marker is
// gone. Output written late restarts the clock.
func TestRunOutput_TTLExpiryDropsTheBuffer(t *testing.T) {
	const ttl = time.Hour
	srv, clock, seed := runOutputServer(t, func(c *Config) { c.ExecOutputTailTTL = ttl })
	id := seed(types.AgentRun{})
	w := srv.openExecOutput(types.AgentRun{ID: id}, false)
	writeExecOutput(t, w, "first\n")

	clock.advance(ttl - time.Minute)
	writeExecOutput(t, w, "second\n")
	clock.advance(ttl - time.Minute)
	if code, got, _ := getRunOutput(t, srv, id, "", outputOwnerCookie(t)); code != http.StatusOK || got.Output != "first\nsecond\n" {
		t.Fatalf("inside the TTL of the last output: status %d body %+v, want 200 with both lines", code, got)
	}

	clock.advance(time.Minute)
	code, _, refusal := getRunOutput(t, srv, id, "", outputOwnerCookie(t))
	if code != http.StatusGone || refusal.Reason != reasonRunOutputExpired {
		t.Fatalf("at the TTL: status %d reason %q, want 410 %s", code, refusal.Reason, reasonRunOutputExpired)
	}
	e := srv.execOutputs.m[id]
	e.mw.mu.Lock()
	released := e.ring.buf == nil
	e.mw.mu.Unlock()
	if !released {
		t.Fatal("an expired tail still holds its bytes")
	}
	writeExecOutput(t, w, "after expiry\n") // a silent run that speaks again stays expired
	if code, _, _ := getRunOutput(t, srv, id, "", outputOwnerCookie(t)); code != http.StatusGone {
		t.Fatalf("a write after expiry revived the tail: status %d", code)
	}

	clock.advance(ttl)
	code, _, refusal = getRunOutput(t, srv, id, "", outputOwnerCookie(t))
	srv.execOutputs.mu.Lock()
	_, held := srv.execOutputs.m[id]
	srv.execOutputs.mu.Unlock()
	if held || code != http.StatusConflict || refusal.Reason != reasonRunOutputNotKept {
		t.Fatalf("two TTLs on: held %v, status %d reason %q; want dropped, 409 %s", held, code, refusal.Reason, reasonRunOutputNotKept)
	}
}

// TestRunOutput_OffAndNotKept: the off switch keeps nothing and says so; a run
// with no tail (a sign-in run, or one from before a restart) says that.
func TestRunOutput_OffAndNotKept(t *testing.T) {
	srv, _, seed := runOutputServer(t)
	harness := seed(types.AgentRun{})
	if w := srv.openExecOutput(types.AgentRun{ID: harness, Task: harnessLoginTask}, false); w != nil {
		t.Fatal("openExecOutput kept a tail for a sign-in run that is not interactive")
	}
	if code, _, refusal := getRunOutput(t, srv, harness, "", outputOwnerCookie(t)); code != http.StatusConflict || refusal.Reason != reasonRunOutputNotKept {
		t.Fatalf("harness run: status %d reason %q, want 409 %s", code, refusal.Reason, reasonRunOutputNotKept)
	}

	off, _, seedOff := runOutputServer(t, func(c *Config) { c.ExecOutputTailOff = true })
	id := seedOff(types.AgentRun{})
	if w := off.openExecOutput(types.AgentRun{ID: id}, false); w != nil {
		t.Fatal("WARDYN_EXEC_OUTPUT_TAIL=off still kept a tail")
	}
	if code, _, refusal := getRunOutput(t, off, id, "", outputOwnerCookie(t)); code != http.StatusConflict || refusal.Reason != reasonRunOutputOff {
		t.Fatalf("off: status %d reason %q, want 409 %s", code, refusal.Reason, reasonRunOutputOff)
	}
}

// TestRunOutput_MasksRegisteredSecrets: a value registered after the run
// started is masked as it is written, including across two writes. On a
// finished run the bytes the masker held back are hidden when they are a
// secret cut short (MinLen or more) and served when they are only a short
// shared prefix; a live run serves neither yet.
func TestRunOutput_MasksRegisteredSecrets(t *testing.T) {
	reg := secretmask.NewRegistry()
	srv, _, seed := runOutputServer(t, func(c *Config) { c.MaskRegistry = reg })
	for _, tc := range []struct {
		name, last, wantSuffix string
		state                  types.RunState
	}{
		{"cut-short secret", "trailing s3cr3t-tok", "trailing <secret-hidden>", types.RunCompleted},
		{"short shared prefix", "exit s3c", "exit s3c", types.RunCompleted},
		{"live run holds back", "trailing s3cr3t-tok", "done\ntrailing ", types.RunRunning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := seed(types.AgentRun{State: tc.state})
			w := srv.openExecOutput(types.AgentRun{ID: id}, false)
			reg.Add(id, []byte("s3cr3t-token-value"))
			writeExecOutput(t, w, "token=s3cr3t-to", "ken-value done\n", tc.last)

			_, got, _ := getRunOutput(t, srv, id, "", outputOwnerCookie(t))
			if strings.Contains(got.Output, "s3cr3t-to") {
				t.Fatalf("output leaks the registered secret: %q", got.Output)
			}
			if !strings.HasPrefix(got.Output, "token=<secret-hidden> done\n") || !strings.HasSuffix(got.Output, tc.wantSuffix) {
				t.Fatalf("output %q, want the split secret masked and a %q suffix", got.Output, tc.wantSuffix)
			}
		})
	}
}

// TestDispatch_HandsNonInteractiveRunsAnOutputWriter is the wiring: dispatch
// gives the runner a writer for every non-interactive run except the sign-in
// run, and what the runner writes there is what the route serves.
func TestDispatch_HandsNonInteractiveRunsAnOutputWriter(t *testing.T) {
	for _, tc := range []struct {
		name        string
		taskMode    string
		task        string
		interactive bool
		off         bool
		want        bool
	}{
		{"exec", "exec", "", false, false, true},
		{"agent mode", "", "", false, false, true},
		{"interactive", "", "", true, false, false},
		// Not interactive on purpose: only runIsUnrecordable keeps it out.
		{"sign-in run", "", harnessLoginTask, false, false, false},
		{"tail off", "", "", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rn := &fakeRunner{}
			srv, _, run := statusDetailDispatchFixture(t, rn)
			srv.cfg.ExecOutputTailOff = tc.off
			run.Task = tc.task
			srv.dispatchRun(t.Context(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
				RunToken: "run-token", Image: "wardyn/claude-code:latest", TaskMode: tc.taskMode, Interactive: tc.interactive,
				Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
			})
			rn.mu.Lock()
			out := rn.lastSpec.ExecOutput
			creates := rn.createCalls
			rn.mu.Unlock()
			if creates != 1 {
				t.Fatalf("CreateSandbox called %d times, want 1", creates)
			}
			if (out != nil) != tc.want {
				t.Fatalf("SandboxSpec.ExecOutput set = %v, want %v", out != nil, tc.want)
			}
			if !tc.want {
				return
			}
			writeExecOutput(t, out, "from the runner\n")
			w := do(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/output", adminToken, "")
			var got runOutputResponse
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
				t.Fatalf("GET /runs/{id}/output = %d %q (%v), want 200 with the runner's bytes", w.Code, w.Body, err)
			}
			if got.Output != "from the runner\n" || got.Truncated {
				t.Fatalf("route served %q truncated %v, want the runner's bytes", got.Output, got.Truncated)
			}
		})
	}
}

// TestRunOutput_ConfiguredSizeAndDefault: a 100 KiB write keeps the last 65536
// bytes by default, and a configured size bounds both the ring and ?tail=.
func TestRunOutput_ConfiguredSizeAndDefault(t *testing.T) {
	srv, _, seed := runOutputServer(t)
	id := seed(types.AgentRun{})
	all := strings.Repeat("0123456789abcdef", 100<<10/16)
	writeExecOutput(t, srv.openExecOutput(types.AgentRun{ID: id}, false), all)
	_, got, _ := getRunOutput(t, srv, id, "", outputOwnerCookie(t))
	if got.Output != all[len(all)-65536:] || !got.Truncated {
		t.Fatalf("default: len %d truncated %v, want the last 65536 bytes, truncated", len(got.Output), got.Truncated)
	}

	small, _, seedSmall := runOutputServer(t, func(c *Config) { c.RunOutputTailBytes = 2048 })
	id = seedSmall(types.AgentRun{})
	writeExecOutput(t, small.openExecOutput(types.AgentRun{ID: id}, false), all)
	_, got, _ = getRunOutput(t, small, id, "?tail=65536", outputOwnerCookie(t))
	if got.Output != all[len(all)-2048:] || !got.Truncated {
		t.Fatalf("configured 2048: len %d truncated %v, want the last 2048 bytes, truncated", len(got.Output), got.Truncated)
	}
}
