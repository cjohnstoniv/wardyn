// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build macvm_live

// Package macvmlive is the macOS VM live conformance suite: the proofs P4-P22 of the rented-Mac
// window, run on a Mac with the org stack up on loopback, the runner package installed and a ready
// base. It builds nothing and installs nothing. Every P-test skips unless it runs on darwin/arm64
// with WARDYN_MACVM_LIVE=1, so hosted CI compiles it and runs it to a clean skip; the summary
// self-tests (summary_test.go) run everywhere.
//
// The suite drives runs through the wardyn CLI, whose session the operator makes for the window
// (`wardyn login` as the window's test admin); it never reads a credential file, a password or a
// one-time code, and never asks for one. A step only a person can do is a prompt on /dev/tty that
// waits with a timeout and records the answer; with WARDYN_MACVM_PROMPTS unset, tests that need
// one skip and say when the window runs them.
//
// Outcomes are explicit. A proof that cannot be witnessed calls notRun, which logs the NOT_RUN
// marker and skips; run.sh's summary counts every skip inside a P-row as NOT_RUN, never as PASS.
// A test whose failure blocks the release calls block and is listed in blockItems.
//
// Names of lanes that had not merged when this was written sit in the assumed* variables below, so
// a different name is a one-line fix. Evidence beyond the test log goes under
// WARDYN_MACVM_EVIDENCE/<test>/.
package macvmlive

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
)

var (
	// assumedRunnerdStatus prints the runner's macOS VM state as JSON (the RUNNER lane's
	// `wardyn-runnerd macos-vm status`). Only statusKeys are ever read from it.
	assumedRunnerdStatus = []string{"macos-vm", "status", "--json"}
	// assumedRunnerdLabel is runnerd's LaunchAgent label (deploy/runner/macos).
	assumedRunnerdLabel = env("WARDYN_MACVM_RUNNERD_LABEL", "com.wardyn.runnerd.managed")
	// assumedGuestAgent is the guest agent's process name in the guest (cmd/wardyn-guest-agent).
	assumedGuestAgent = "wardyn-guest-agent"
)

// statusKeys are the only keys of runnerd's status the suite reads, logs or keeps.
var statusKeys = []string{"state", "phase", "base", "desktop", "harnesses", "thermal", "max", "identity", "held_disk_bytes", "leaked", "error"}

// Sentences the suite matches. Placement's come from internal/api/run_placement_capabilities.go,
// the ledger's from internal/macvm/admission; console copy may still change with the approved mock.
var msg = map[string]*regexp.Regexp{
	"licence": regexp.MustCompile(`This Mac already runs 2 macOS VMs, the most Apple's licence allows`),
	"noRoom":  regexp.MustCompile(`This Mac has no room for another macOS VM right now`),
	// ledgerDisk is the runner ledger's own disk refusal.
	"ledgerDisk": regexp.MustCompile(`this Mac needs \d+ GiB free for another macOS VM`),
	"preparing":  regexp.MustCompile(`This Mac is preparing macOS, which uses one of its 2 VM slots`),
	"posture":    regexp.MustCompile("This Mac has no MDM posture file saying it is managed"),
	"managedOp":  regexp.MustCompile("This Mac is not marked as company-managed. A security operator marks it with `wardyn runner trust"),
	"managedMem": regexp.MustCompile(`This Mac is not marked as company-managed. Ask a security operator to mark it`),
	// signInsOff is the 403 at desktop open and at the sign-in pause (internal/api), never placement's.
	"signInsOff": regexp.MustCompile(`Your admin doesn't allow sign-ins in this desktop while this agent runs`),
	// placementAny is any placement refusal: a create that failed without one is a CLI failure.
	"placementAny": regexp.MustCompile(`placement_|runner_posture_unmet|This Mac |macOS virtual machines|macOS VM`),
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// cfg is read once from the environment. WARDYN_MACVM_LIVE, WARDYN_MACVM_RUNNER and
// WARDYN_MACVM_EVIDENCE are required on the rental; the URLs are what each network proof needs.
var cfg = struct {
	live, prompts, twoLogins, localWindow bool
	evidence, runner, runDoc              string
	identityCmd                           string
	wardyn, runnerd                       string
	allowedURL, deniedHost, gitURL        string
	bigURL, cookieURL                     string
	upgradePkg                            string
	clockFor, clockEvery                  time.Duration
	promptWait                            time.Duration
	baseMinMemGiB, dockerMemGiB           int
	runDiskGiB, slotBases                 int
	stopLoops, warmTravel, warmFresh      int
}{
	live:          os.Getenv("WARDYN_MACVM_LIVE") == "1",
	prompts:       os.Getenv("WARDYN_MACVM_PROMPTS") == "1",
	twoLogins:     os.Getenv("WARDYN_MACVM_TWO_LOGINS") == "1",
	localWindow:   os.Getenv("WARDYN_MACVM_LOCAL_WINDOW") == "1",
	evidence:      os.Getenv("WARDYN_MACVM_EVIDENCE"),
	runner:        os.Getenv("WARDYN_MACVM_RUNNER"),
	runDoc:        os.Getenv("WARDYN_MACVM_RUN_DOC"),
	identityCmd:   os.Getenv("WARDYN_MACVM_IDENTITY_CMD"),
	wardyn:        env("WARDYN_MACVM_WARDYN", "wardyn"),
	runnerd:       env("WARDYN_MACVM_RUNNERD", "wardyn-runnerd"),
	allowedURL:    os.Getenv("WARDYN_MACVM_ALLOWED_URL"),
	deniedHost:    os.Getenv("WARDYN_MACVM_DENIED_HOST"),
	gitURL:        os.Getenv("WARDYN_MACVM_GIT_URL"),
	bigURL:        os.Getenv("WARDYN_MACVM_1G_URL"),
	cookieURL:     os.Getenv("WARDYN_MACVM_COOKIE_URL"),
	upgradePkg:    os.Getenv("WARDYN_MACVM_UPGRADE_PKG"),
	clockFor:      envDur("WARDYN_MACVM_CLOCK_FOR", time.Hour),
	clockEvery:    envDur("WARDYN_MACVM_CLOCK_EVERY", 5*time.Minute),
	promptWait:    envDur("WARDYN_MACVM_PROMPT_WAIT", 30*time.Minute),
	baseMinMemGiB: envInt("WARDYN_MACVM_BASE_MIN_MEMORY_GB", 4),
	dockerMemGiB:  envInt("WARDYN_MACVM_DOCKER_MEMORY_GB", 12),
	runDiskGiB:    envInt("WARDYN_MACVM_RUN_DISK_GB", 50),
	slotBases:     envInt("WARDYN_MACVM_SLOT_BASES", 1),
	stopLoops:     envInt("WARDYN_MACVM_STOP_LOOPS", 10),
	warmTravel:    envInt("WARDYN_MACVM_WARM_TRAVEL", 10),
	warmFresh:     envInt("WARDYN_MACVM_WARM_FRESH", 5),
}

func envDur(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(k)); err == nil && d > 0 {
		return d
	}
	return def
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return n
	}
	return def
}

// live skips a test anywhere but the rental: on another OS or architecture, or without the gate.
func live(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skipf("not darwin/arm64 (%s/%s): macOS VM live proofs run only on the rented Mac", runtime.GOOS, runtime.GOARCH)
	}
	if !cfg.live {
		t.Skip("WARDYN_MACVM_LIVE is not 1: no rental window")
	}
	if cfg.runner == "" || cfg.evidence == "" {
		t.Fatal("WARDYN_MACVM_RUNNER and WARDYN_MACVM_EVIDENCE must name the window's runner and evidence dir")
	}
}

// notRun records that a proof could not be witnessed, with why, and skips. The summary maps the
// marker to NOT_RUN; it is never a pass.
func notRun(t *testing.T, format string, args ...any) {
	t.Helper()
	t.Skip(notRunMarker + fmt.Sprintf(format, args...))
}

// untested lists a sub-property the window cannot observe at all, with why, and skips. The summary
// prints it under "Listed untested"; it is never a pass and never decides its row's outcome.
func untested(t *testing.T, reason string) {
	t.Helper()
	t.Skip(untestedMarker + reason)
}

// block marks t as a BLOCK item: its failure holds macOS VMs out of the release. t must be listed
// in blockItems (summary_test.go), which the summary uses even when t skipped before calling this.
// Mark the narrowest test: a parent's own failure is attributed to its failed children.
func block(t *testing.T) {
	t.Helper()
	if !isBlock(t.Name()) {
		t.Fatalf("block(%s): not in blockItems", t.Name())
	}
	t.Log(blockMarker)
}

// needPerson skips a test whose steps need a person unless prompts are on.
func needPerson(t *testing.T, slot string) {
	t.Helper()
	if !cfg.prompts {
		t.Skipf("needs a person at the Mac: run at %s with WARDYN_MACVM_PROMPTS=1", slot)
	}
}

func need(t *testing.T, v, name string) {
	t.Helper()
	if v == "" {
		notRun(t, "%s is empty: this proof needs it", name)
	}
}

// ---- prompts ----

// The terminal is opened once and read by one goroutine for the process's life, so no reader is
// left behind by a prompt that timed out.
var tty struct {
	sync.Mutex
	once sync.Once
	p    *prompts
	err  error
}

// prompts is one prompt stream. Once a prompt times out, no answer can be bound to the prompt
// after it (the person may still be typing the late one), so every later prompt is NOT_RUN.
type prompts struct {
	w        io.Writer
	lines    chan string
	timedOut string // the prompt that timed out, if one did
}

// lineReader sends each line read from r, trimmed, and closes the channel at EOF.
func lineReader(r io.Reader) chan string {
	lines := make(chan string, 16)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			lines <- strings.TrimSpace(sc.Text())
		}
	}()
	return lines
}

// ask puts a question to the person on /dev/tty and returns the line they type. It never asks for
// a secret: questions name a step to do or an observation to report. No answer within the wait is
// NOT_RUN, never a guess.
func ask(t *testing.T, q string) string {
	t.Helper()
	tty.Lock()
	defer tty.Unlock()
	tty.once.Do(func() {
		f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			tty.err = err
			return
		}
		tty.p = &prompts{w: f, lines: lineReader(f)}
	})
	if tty.err != nil {
		notRun(t, "no terminal for the prompt %q: %v", q, tty.err)
	}
	return tty.p.ask(t, q, cfg.promptWait)
}

// ask is one prompt on the stream. A line typed before the prompt is discarded, never taken as its
// answer.
func (p *prompts) ask(t *testing.T, q string, wait time.Duration) string {
	t.Helper()
	if p.timedOut != "" {
		notRun(t, "no prompt is put after %q timed out: its late answer could be taken for %q", p.timedOut, q)
	}
	for drained := false; !drained; {
		select {
		case l, ok := <-p.lines:
			if !ok {
				notRun(t, "the terminal closed before the prompt %q", q)
			}
			t.Logf("PROMPT discarded a line typed before %q: %q", q, l)
		default:
			drained = true
		}
	}
	fmt.Fprintf(p.w, "\n[%s] %s\n(answer within %s) > ", t.Name(), q, wait)
	select {
	case a, ok := <-p.lines:
		if !ok {
			notRun(t, "the terminal closed before an answer to %q", q)
		}
		t.Logf("PROMPT %q -> %q", q, a)
		return a
	case <-time.After(wait):
		p.timedOut = q
		notRun(t, "no answer within %s to %q; no further prompt is put this run", wait, q)
		return ""
	}
}

// confirm asks a yes/no question until the answer is one of them.
func confirm(t *testing.T, q string) bool {
	t.Helper()
	for {
		switch strings.ToLower(ask(t, q+" [yes/no]")) {
		case "yes", "y":
			return true
		case "no", "n":
			return false
		}
	}
}

// doStep asks the person to do something and waits for "done"; "skip" is NOT_RUN.
func doStep(t *testing.T, step string) {
	t.Helper()
	for {
		switch strings.ToLower(ask(t, step+" Type done, or skip if it cannot be done.")) {
		case "done":
			return
		case "skip":
			notRun(t, "the person could not do %q", step)
		}
	}
}

// ---- evidence ----

func evidenceDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(cfg.evidence, strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatalf("evidence dir: %v", err)
	}
	return d
}

// keep writes one evidence file and logs where.
func keep(t *testing.T, name string, b []byte) {
	t.Helper()
	p := filepath.Join(evidenceDir(t), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Errorf("evidence %s: %v", p, err)
		return
	}
	t.Logf("evidence: %s (%d bytes)", p, len(b))
}

func keepJSON(t *testing.T, name string, v any) {
	t.Helper()
	b, _ := json.MarshalIndent(v, "", "  ")
	keep(t, name, b)
}

// record is one measured or recorded value, as a greppable log line for the window report.
func record(t *testing.T, key string, v any) {
	t.Helper()
	t.Logf("RECORD %s=%v", key, v)
}

// ---- commands ----

type result struct {
	out, err string
	code     int
}

// sh runs one command with a timeout and logs it with its exit status.
func sh(t *testing.T, timeout time.Duration, name string, args ...string) result {
	t.Helper()
	return shIn(t, timeout, nil, name, args...)
}

func shIn(t *testing.T, timeout time.Duration, stdin []byte, name string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	r := result{out: o.String(), err: e.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	default:
		r.code, r.err = -1, r.err+err.Error()
	}
	if ctx.Err() != nil {
		r.code, r.err = -1, r.err+" (timed out after "+timeout.String()+")"
	}
	t.Logf("$ %s %s -> exit %d", name, strings.Join(args, " "), r.code)
	return r
}

func mustOK(t *testing.T, r result, what string) string {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("%s: exit %d: %s", what, r.code, strings.TrimSpace(r.err+" "+r.out))
	}
	return r.out
}

func wardyn(t *testing.T, args ...string) result { return sh(t, 2*time.Minute, cfg.wardyn, args...) }

// runnerdStatus is the allowlisted part of runnerd's macOS VM status. Nothing else from it is read
// or logged, so a one-time code or any later field never reaches the evidence.
func runnerdStatus(t *testing.T) map[string]any {
	t.Helper()
	r := sh(t, time.Minute, cfg.runnerd, assumedRunnerdStatus...)
	if r.code != 0 {
		t.Fatalf("runnerd status: exit %d (its output is not logged)", r.code)
	}
	m, err := allowStatus([]byte(r.out))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// allowStatus keeps only statusKeys from a status body. Its errors name the field it looked for,
// never any of the body.
func allowStatus(b []byte) (map[string]any, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return nil, fmt.Errorf("runnerd status: not one JSON object, so none of %v could be read", statusKeys)
	}
	out := map[string]any{}
	for _, k := range statusKeys {
		if v, ok := raw[k]; ok {
			var x any
			if json.Unmarshal(v, &x) != nil {
				return nil, fmt.Errorf("runnerd status: field %q is not JSON", k)
			}
			out[k] = x
		}
	}
	return out, nil
}

// setIdentity switches the identity strategy for new runs through the command the RUNNER lane
// exposes (WARDYN_MACVM_IDENTITY_CMD, with {strategy} in it). Without one, switching is NOT_RUN.
func setIdentity(t *testing.T, s macvm.IdentityStrategy) {
	t.Helper()
	if cfg.identityCmd == "" {
		notRun(t, "no identity-strategy switch is exposed (WARDYN_MACVM_IDENTITY_CMD is empty), so %s cannot be selected", s)
	}
	mustOK(t, sh(t, 2*time.Minute, "sh", "-c", strings.ReplaceAll(cfg.identityCmd, "{strategy}", string(s))), "switch the identity strategy to "+string(s))
}

func checkIdentity(t *testing.T, st macvm.Status, s macvm.IdentityStrategy) {
	t.Helper()
	if st.Identity != s {
		t.Fatalf("run %s was configured with identity %q, want %q: the switch did not take", st.RunID, st.Identity, s)
	}
}

// ---- the window runner ----

type amounts struct {
	CPUMillis int64 `json:"cpu_millis"`
	MemoryMiB int64 `json:"memory_mib"`
	DiskMiB   int64 `json:"disk_mib"`
}

type tally struct {
	Runs      int   `json:"runs"`
	CPUMillis int64 `json:"cpu_millis"`
	MemoryMiB int64 `json:"memory_mib"`
	DiskMiB   int64 `json:"disk_mib"`
}

// runnerView is the part of `wardyn runner list --json` the suite reads.
type runnerView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	TrustClass string `json:"trust_class"`
	Limits     *struct {
		RunsAtOnce int     `json:"runs_at_once"`
		Total      amounts `json:"total"`
		PerRun     amounts `json:"per_run"`
	} `json:"limits"`
	Usage struct {
		InUse     tally `json:"in_use"`
		Available tally `json:"available"`
	} `json:"usage"`
}

func runnerRow(t *testing.T) runnerView {
	t.Helper()
	var rows []runnerView
	if err := json.Unmarshal([]byte(mustOK(t, wardyn(t, "runner", "list", "--json"), "runner list")), &rows); err != nil {
		t.Fatalf("runner list JSON: %v", err)
	}
	for _, r := range rows {
		if r.ID == cfg.runner || r.Name == cfg.runner {
			return r
		}
	}
	t.Fatalf("runner %q is not in runner list", cfg.runner)
	return runnerView{}
}

var runnerIDCache string

// runnerID is the window runner's id, resolved from WARDYN_MACVM_RUNNER (an id or a name).
func runnerID(t *testing.T) string {
	t.Helper()
	if runnerIDCache == "" {
		runnerIDCache = runnerRow(t).ID
	}
	if runnerIDCache == "" {
		t.Fatal("the window runner has no id")
	}
	return runnerIDCache
}

// ---- runs ----

type run struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	FailureHint string `json:"failure_hint"`
	RunnerID    string `json:"runner_id"`
	MachineKind string `json:"machine_kind"`
}

func (r run) terminal() bool {
	return slices.Contains([]string{"FAILED", "KILLED", "COMPLETED", "STOPPED", "ARCHIVED"}, r.State)
}

// runDocument is the kind: Run document a run starts from: path when set, else an interactive run
// with no harness. intent gets machine_kind, the pin to the window runner and the caller's fields.
func runDocument(t *testing.T, path string, intent map[string]any) []byte {
	t.Helper()
	doc := map[string]any{"api_version": "wardyn/v1", "kind": "Run", "intent": map[string]any{"agent": "none", "interactive": true}}
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("run document %s: %v", path, err)
		}
		doc = nil
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("run document %s must be JSON: %v", path, err)
		}
	}
	in, _ := doc["intent"].(map[string]any)
	if in == nil {
		in = map[string]any{}
		doc["intent"] = in
	}
	in["machine_kind"] = "macos_vm"
	in["placement"] = "local"
	in["runner_id"] = runnerID(t)
	for k, v := range intent {
		in[k] = v
	}
	b, _ := json.Marshal(doc)
	return b
}

// spec is one ask for a run: its intent fields, the document it starts from (default
// WARDYN_MACVM_RUN_DOC), and the test that owns it (the run is ended when that test ends; default
// the asking test). A run that must outlive a subtest is owned by the parent.
type spec struct {
	intent map[string]any
	doc    string
	owner  *testing.T
}

// errPlacement is a create the server refused at placement, with its sentence.
type errPlacement struct{ text string }

func (e errPlacement) Error() string { return "refused at placement: " + e.text }

// create asks for one run. A placement refusal is returned as errPlacement; any other failure of
// the CLI fails the test with its output.
func create(t *testing.T, s spec) (string, error) {
	t.Helper()
	if s.doc == "" {
		s.doc = cfg.runDoc
	}
	owner := s.owner
	if owner == nil {
		owner = t
	}
	r := shIn(t, 2*time.Minute, runDocument(t, s.doc, s.intent), cfg.wardyn, "run", "--file", "-", "--json", "--title", "macvm-live "+t.Name())
	if r.code != 0 {
		text := strings.TrimSpace(r.err + " " + r.out)
		if msg["placementAny"].MatchString(text) {
			return "", errPlacement{text}
		}
		t.Fatalf("create failed without a placement refusal: exit %d: %s", r.code, text)
	}
	var v run
	if err := json.Unmarshal([]byte(r.out), &v); err != nil || v.ID == "" {
		t.Fatalf("create: no run id in %q", r.out)
	}
	owner.Cleanup(func() { end(owner, v.ID) })
	return v.ID, nil
}

func get(t *testing.T, id string) run {
	t.Helper()
	var v run
	if err := json.Unmarshal([]byte(mustOK(t, wardyn(t, "run", "get", id, "--json"), "run get")), &v); err != nil {
		t.Fatalf("run get JSON: %v", err)
	}
	return v
}

// wait polls the run until it is RUNNING or terminal, or the timeout passes.
func wait(t *testing.T, id string, timeout time.Duration) run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		v := get(t, id)
		if v.State == "RUNNING" || v.terminal() || time.Now().After(deadline) {
			return v
		}
		time.Sleep(2 * time.Second)
	}
}

// pinned fails the test if a run landed anywhere but the window runner.
func pinned(t *testing.T, v run) {
	t.Helper()
	if v.RunnerID != "" && v.RunnerID != runnerID(t) {
		t.Fatalf("run %s landed on runner %s, not the window runner %s", v.ID, v.RunnerID, runnerID(t))
	}
}

// start creates a run and waits for RUNNING on the window runner, failing the test otherwise.
func start(t *testing.T, s spec) string {
	t.Helper()
	id, err := create(t, s)
	if err != nil {
		t.Fatal(err)
	}
	v := wait(t, id, 5*time.Minute)
	if v.State != "RUNNING" {
		t.Fatalf("run %s is %s, not RUNNING: %s", id, v.State, v.FailureHint)
	}
	if v.RunnerID == "" {
		t.Fatalf("run %s is RUNNING with no runner_id", id)
	}
	pinned(t, v)
	return id
}

// refusal is how an ask that should be refused ended: at placement (create), by the runner (the
// run failed with a reason, the ledger's among them), or not at all ("none").
type refusal struct {
	By, Reason, RunID, State string
}

func askRefused(t *testing.T, s spec) refusal {
	t.Helper()
	id, err := create(t, s)
	var ep errPlacement
	if errors.As(err, &ep) {
		return refusal{By: "placement", Reason: ep.text}
	}
	v := wait(t, id, 3*time.Minute)
	pinned(t, v)
	if v.terminal() {
		return refusal{By: "runner", Reason: v.FailureHint, RunID: id, State: v.State}
	}
	return refusal{By: "none", Reason: v.FailureHint, RunID: id, State: v.State}
}

// mustRefuse records a refusal and asserts it came from `by` with a sentence matching re.
func mustRefuse(t *testing.T, r refusal, by string, re *regexp.Regexp) {
	t.Helper()
	t.Logf("RECORD refused_by=%s reason=%q state=%s", r.By, r.Reason, r.State)
	switch {
	case r.By == "none":
		t.Fatalf("not refused: run %s is %s", r.RunID, r.State)
	case r.By != by:
		t.Fatalf("refused by %s, want %s: %q", r.By, by, r.Reason)
	case !re.MatchString(r.Reason):
		t.Fatalf("refused by %s without the expected sentence (%s): %q", r.By, re, r.Reason)
	}
}

// end kills a run and waits for it to be terminal. Safe to call twice.
func end(t *testing.T, id string) {
	t.Helper()
	if get(t, id).terminal() {
		return
	}
	wardyn(t, "run", "kill", id)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if get(t, id).terminal() {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("run %s did not end within 3 minutes of its kill", id)
}

// guest runs a shell script in the run's guest and returns its output and exit status.
func guest(t *testing.T, id, script string, timeout time.Duration) result {
	t.Helper()
	return sh(t, timeout, cfg.wardyn, "run", "exec", id, "--", "sh", "-c", script)
}

func mustGuest(t *testing.T, id, script string) string {
	t.Helper()
	return mustOK(t, guest(t, id, script, 2*time.Minute), "guest: "+script)
}

// ---- host side ----

func layout(t *testing.T) macvm.Layout {
	t.Helper()
	root, err := macvm.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	return macvm.Layout{Root: root}
}

func runDir(t *testing.T, id string) string { return layout(t).RunDir(id) }

func dialHelper(ctx context.Context, t *testing.T, id string) (*macvm.ControlClient, error) {
	t.Helper()
	return macvm.DialControl(ctx, filepath.Join(runDir(t, id), macvm.ControlSock))
}

// helperStatus asks the run's helper over its ctl.sock. The helper serves each connection on its
// own, so this does not disturb runnerd's.
func helperStatus(t *testing.T, id string) macvm.Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cc, err := dialHelper(ctx, t, id)
	if err != nil {
		t.Fatalf("ctl.sock of run %s: %v", id, err)
	}
	defer cc.Close()
	st, err := cc.Status(ctx)
	if err != nil {
		t.Fatalf("status of run %s: %v", id, err)
	}
	return st
}

// helperStop asks the run's helper to stop its VM (gracefully first: the guest is asked to shut
// down), the stop path that bypasses the org.
func helperStop(t *testing.T, id string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cc, err := dialHelper(ctx, t, id)
	if err != nil {
		return err
	}
	defer cc.Close()
	return cc.Stop(ctx, true, 30*time.Second)
}

// helloAfter waits for the helper's guest_agent event and returns when it came.
func helloAfter(t *testing.T, id string, within time.Duration) (time.Time, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	for {
		cc, err := dialHelper(ctx, t, id)
		if err == nil {
			defer cc.Close()
			ev, err := cc.Events(ctx)
			if err != nil {
				return time.Time{}, false
			}
			for e := range ev {
				if e.Type == macvm.EventGuestAgent {
					return e.Time, true
				}
			}
			return time.Time{}, false
		}
		select {
		case <-ctx.Done():
			return time.Time{}, false
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ledgerFile is one file in the admission ledger dir, read without the ledger's lock: the suite
// only observes.
type ledgerFile struct {
	Name string
	UID  int
	Mode os.FileMode
	Tomb bool
	// Honoured is set on a tombstone the ledger would honour now (tombstonable); any other
	// tombstone leaves its entry counted.
	Honoured bool
	Entry    admission.Entry
}

// tombstonable is the ledger's tombstone condition (internal/macvm/admission/tombstone.go): an
// entry of this boot, not leaked, whose helper (or, with none yet, its owner) is gone while uid has
// no Virtualization process and no helper process.
func tombstonable(e admission.Entry, uid int, boot int64, ps []admission.Proc) bool {
	alive := func(pid int, start int64) bool {
		return slices.ContainsFunc(ps, func(p admission.Proc) bool { return p.PID == pid && p.Start == start })
	}
	count := func(name string) int {
		n := 0
		for _, p := range ps {
			if p.UID == uid && p.Name != "" && strings.HasPrefix(name, p.Name) {
				n++
			}
		}
		return n
	}
	gone := !alive(e.OwnerPID, e.OwnerStart)
	if e.HelperPID != 0 {
		gone = !alive(e.HelperPID, e.HelperStart)
	}
	return e.BootTime == boot && e.Kind != admission.KindLeaked && gone &&
		count(admission.VirtualizationProcess) == 0 && count(admission.HelperProcess) == 0
}

func ledger(t *testing.T) []ledgerFile {
	t.Helper()
	des, err := os.ReadDir(admission.DefaultDir)
	if err != nil {
		t.Fatalf("ledger dir: %v", err)
	}
	var out []ledgerFile
	for _, de := range des {
		n := de.Name()
		if !strings.HasPrefix(n, "e-") || !strings.HasSuffix(n, ".json") && !strings.HasSuffix(n, ".json.tombstone") {
			continue
		}
		fi, err := os.Lstat(filepath.Join(admission.DefaultDir, n))
		if err != nil {
			continue
		}
		lf := ledgerFile{Name: n, Mode: fi.Mode(), Tomb: strings.HasSuffix(n, ".tombstone")}
		if st, ok := fi.Sys().(*unix.Stat_t); ok {
			lf.UID = int(st.Uid)
		}
		if b, err := os.ReadFile(filepath.Join(admission.DefaultDir, n)); err == nil {
			_ = json.Unmarshal(b, &lf.Entry)
		}
		out = append(out, lf)
	}
	host := admission.SystemHost()
	boot, berr := host.BootTime()
	ps, perr := host.Processes()
	if berr != nil || perr != nil {
		t.Logf("ledger: boot time or processes unreadable (%v, %v): no tombstone is honoured", berr, perr)
	}
	byName := map[string]ledgerFile{}
	for _, lf := range out {
		if !lf.Tomb {
			byName[lf.Name] = lf
		}
	}
	for i, lf := range out {
		if e, ok := byName[strings.TrimSuffix(lf.Name, ".tombstone")]; lf.Tomb && ok && berr == nil && perr == nil {
			out[i].Honoured = tombstonable(e.Entry, e.UID, boot, ps)
		}
	}
	return out
}

// countedFiles is the ledger's live entries: every entry whose tombstone, if any, the ledger
// would not honour now.
func countedFiles(fs []ledgerFile) []ledgerFile {
	tomb := map[string]bool{}
	for _, f := range fs {
		if f.Tomb && f.Honoured {
			tomb[strings.TrimSuffix(f.Name, ".tombstone")] = true
		}
	}
	var out []ledgerFile
	for _, f := range fs {
		if !f.Tomb && !tomb[f.Name] {
			out = append(out, f)
		}
	}
	return out
}

func counted(fs []ledgerFile) int { return len(countedFiles(fs)) }

// heldDisk is the disk every entry in the ledger reserves, tombstoned ones included: a tombstone
// frees a slot and memory but its disk stays held until its owner returns.
func heldDisk(fs []ledgerFile) int64 {
	var n int64
	for _, f := range fs {
		if !f.Tomb {
			n += f.Entry.DiskReserve
		}
	}
	return n
}

// procs counts processes whose executable's base name starts with prefix, for uid (or any, -1).
func procs(t *testing.T, prefix string, uid int) int {
	t.Helper()
	out := mustOK(t, sh(t, 30*time.Second, "ps", "-axo", "uid=,comm="), "ps")
	n := 0
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		u, _ := strconv.Atoi(f[0])
		if (uid < 0 || u == uid) && strings.HasPrefix(filepath.Base(strings.Join(f[1:], " ")), prefix) {
			n++
		}
	}
	return n
}

// freeBytes is the space available to this user on path's volume.
func freeBytes(t *testing.T, path string) int64 {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		t.Fatalf("statfs %s: %v", path, err)
	}
	return int64(st.Bavail) * int64(st.Bsize)
}

const gib = 1 << 30

// snapshotDate is the date tmutil names a local snapshot by; nothing else is passed to delete.
var snapshotDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-\d{6}$`)

// takeSnapshot makes one host local snapshot and returns its date.
func takeSnapshot(t *testing.T) string {
	t.Helper()
	out := mustOK(t, sh(t, 5*time.Minute, "tmutil", "localsnapshot"), "tmutil localsnapshot")
	f := strings.Fields(out)
	if len(f) == 0 || !snapshotDate.MatchString(f[len(f)-1]) {
		t.Fatalf("tmutil localsnapshot printed no snapshot date: %q", out)
	}
	return f[len(f)-1]
}

// deleteSnapshot deletes the window's own snapshot by its date, or asks the person to.
func deleteSnapshot(t *testing.T, date string) {
	t.Helper()
	if !snapshotDate.MatchString(date) {
		t.Fatalf("refusing to delete %q: not a snapshot date", date)
	}
	if sh(t, 10*time.Minute, "tmutil", "deletelocalsnapshots", date).code == 0 {
		return
	}
	if cfg.prompts {
		doStep(t, "Run: sudo tmutil deletelocalsnapshots "+date)
		return
	}
	t.Errorf("the window's snapshot %s is still there: delete it by hand (sudo tmutil deletelocalsnapshots %s)", date, date)
}

// ---- statistics ----

func percentile(xs []time.Duration, p float64) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	return s[max(i, 0)]
}

// ---- identity ----

// guestIdentity reads the guest's serial number and platform UUID from IOPlatformExpertDevice.
type guestIdentity struct{ Serial, UUID string }

var ioregKey = regexp.MustCompile(`"(IOPlatformSerialNumber|IOPlatformUUID)" = "([^"]*)"`)

func readGuestIdentity(t *testing.T, id string) guestIdentity {
	t.Helper()
	var g guestIdentity
	for _, m := range ioregKey.FindAllStringSubmatch(mustGuest(t, id, "ioreg -rd1 -c IOPlatformExpertDevice"), -1) {
		if m[1] == "IOPlatformSerialNumber" {
			g.Serial = m[2]
		} else {
			g.UUID = m[2]
		}
	}
	if g.Serial == "" || g.UUID == "" {
		t.Fatalf("run %s: no serial number or platform UUID in ioreg", id)
	}
	return g
}

// trackingCheck is P4's check: the guest's identity follows the configured identifier if it is the
// same across the travel series and different across the fresh series. It cannot hold when either
// series did not boot. It is recorded as evidence only: it never qualifies a strategy for two
// concurrent VMs.
func trackingCheck(travel, fresh []guestIdentity) (holds bool, why string) {
	if len(travel) < 2 || len(fresh) < 2 {
		return false, "a series did not boot at least twice"
	}
	for _, g := range travel[1:] {
		if g != travel[0] {
			return false, "the travel series reported different identities"
		}
	}
	seen := map[guestIdentity]bool{}
	for _, g := range fresh {
		if seen[g] {
			return false, "the fresh series repeated an identity"
		}
		seen[g] = true
	}
	return true, "travel identical, fresh all distinct"
}

// qualifies is P22's rule for two concurrent VMs: only distinct configured identifiers qualify, so
// fresh qualifies when its two VMs' configured hashes differ and their guests report a different
// serial number and UUID. travel shares one identifier per base, so it is capped at one VM per slot
// base and qualifies for two only across two slot bases, by the same test.
func qualifies(hashes [2]string, ids [2]guestIdentity) (bool, string) {
	switch {
	case hashes[0] == "" || hashes[0] == hashes[1]:
		return false, "the two VMs were configured with the same identifier"
	case ids[0].Serial == ids[1].Serial || ids[0].UUID == ids[1].UUID:
		return false, "the two guests report the same serial number or UUID"
	}
	return true, "distinct configured identifiers and distinct guest identities"
}
