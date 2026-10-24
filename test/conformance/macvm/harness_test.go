// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build macvm_live

// Package macvmlive is the macOS VM live conformance suite: the proofs P4-P22 of the rented-Mac
// window, run on a Mac with the org stack up on loopback, the runner package installed and a ready
// base. It builds nothing and installs nothing. Every test skips unless it runs on darwin/arm64 with
// WARDYN_MACVM_LIVE=1, so hosted CI compiles it and runs it to a clean skip.
//
// The suite drives runs through the wardyn CLI, whose session the operator makes for the window
// (`wardyn login` as the window's test admin); it never reads a credential file, a password or a
// one-time code, and never asks for one. A step only a person can do is a prompt on /dev/tty that
// waits with a timeout and records the answer; with WARDYN_MACVM_PROMPTS unset, tests that need
// one skip and say which slot of the window runs them.
//
// The runner's and the desktop's command names are assumed where their lanes had not landed when
// this was written; each sits in one variable below (assumed*), so a different name is a one-line
// fix. Evidence beyond the test log goes under WARDYN_MACVM_OUT/<test>/.
package macvmlive

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Assumed command lines of lanes that had not merged (RUNNER, DESKTOP, PKG-CI). The helper's
// own subcommands come from cmd/wardyn-macvm on this base, except probe and describe.
var (
	// assumedRunnerdStatus prints the runner's macOS VM state as JSON.
	assumedRunnerdStatus = []string{"macos-vm", "status", "--json"}
	// assumedIdentity switches the identity strategy runnerd gives new runs (both are kept until
	// P4/P22 pick one). The suite checks the switch took through each helper's status.
	assumedIdentity = func(s macvm.IdentityStrategy) []string { return []string{"macos-vm", "identity", string(s)} }
	// assumedDescribe lists a running VM's devices as JSON: {"nics":[{"type":"filehandle"}], ...}.
	assumedDescribe = func(runDir string) []string { return []string{"describe", "--dir", runDir} }
	// assumedRunnerdLabel is runnerd's LaunchAgent label (deploy/runner/macos).
	assumedRunnerdLabel = env("WARDYN_MACVM_RUNNERD_LABEL", "com.wardyn.runnerd.managed")
	// assumedGuestStop stops the guest from inside (P12's guest stop path). The exec user is
	// assumed to have password-less sudo for shutdown in the base.
	assumedGuestStop = env("WARDYN_MACVM_GUEST_STOP", "sudo -n /sbin/shutdown -h now")
	// assumedGuestAgent is the guest agent's process name in the guest (cmd/wardyn-guest-agent).
	assumedGuestAgent = "wardyn-guest-agent"
)

// Messages the suite matches, loosely: the final words come from the approved mock catalogue
// (the approved mock catalogue), and the ledger's own reasons are in internal/macvm/admission.
var msg = map[string]*regexp.Regexp{
	"licence":   regexp.MustCompile(`(?i)already runs \d+ macOS VMs|licence|most it may|at its macOS VM limit`),
	"preparing": regexp.MustCompile(`(?i)preparing macOS.*slot|preparing uses a slot`),
	"disk":      regexp.MustCompile(`(?i)free disk|GiB free for another macOS VM|GB free`),
	"posture":   regexp.MustCompile(`(?i)posture`),
	"managed":   regexp.MustCompile(`(?i)company-managed|managed_device`),
	"memory":    regexp.MustCompile(`(?i)memory`),
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// cfg is read once from the environment. Only WARDYN_MACVM_LIVE and WARDYN_MACVM_RUNNER are
// required; the URLs are what each network proof needs and its test skips without them.
var cfg = struct {
	live, prompts, twoLogins, localWindow bool
	out, runner, runDoc                   string
	wardyn, runnerd, helper               string
	allowedURL, deniedHost, gitURL        string
	bigURL, cookieURL                     string
	upgradePkg                            string
	clockFor, clockEvery                  time.Duration
	promptWait                            time.Duration
	baseMinMemGiB, dockerMemGiB           int
	runDiskGiB                            int
	slotBases                             int
	stopLoops, warmTravel, warmFresh      int
}{
	live:          os.Getenv("WARDYN_MACVM_LIVE") == "1",
	prompts:       os.Getenv("WARDYN_MACVM_PROMPTS") == "1",
	twoLogins:     os.Getenv("WARDYN_MACVM_TWO_LOGINS") == "1",
	localWindow:   os.Getenv("WARDYN_MACVM_LOCAL_WINDOW") == "1",
	out:           env("WARDYN_MACVM_OUT", "macvm-evidence"),
	runner:        os.Getenv("WARDYN_MACVM_RUNNER"),
	runDoc:        os.Getenv("WARDYN_MACVM_RUN_DOC"),
	wardyn:        env("WARDYN_MACVM_WARDYN", "wardyn"),
	runnerd:       env("WARDYN_MACVM_RUNNERD", "wardyn-runnerd"),
	helper:        env("WARDYN_MACVM_HELPER", "wardyn-macvm"),
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
		t.Skip("WARDYN_MACVM_LIVE is not 1: no rental window (the window's run sets it)")
	}
	if cfg.runner == "" {
		t.Fatal("WARDYN_MACVM_RUNNER is empty: name the window's runner (id or name)")
	}
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
		t.Skipf("%s is empty: this proof needs it", name)
	}
}

// ---- prompts ----

var ttyMu sync.Mutex

// ask puts a question to the person on /dev/tty and returns the line they type. It never asks for
// a secret: questions name a step to do or an observation to report. No answer within the wait is
// NOT RUN, never a guess.
func ask(t *testing.T, q string) string {
	t.Helper()
	ttyMu.Lock()
	defer ttyMu.Unlock()
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("NOT RUN: no terminal for the prompt %q: %v", q, err)
	}
	defer tty.Close()
	fmt.Fprintf(tty, "\n[%s] %s\n(answer within %s) > ", t.Name(), q, cfg.promptWait)
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(tty).ReadString('\n')
		line <- strings.TrimSpace(s)
	}()
	select {
	case a := <-line:
		t.Logf("PROMPT %q -> %q", q, a)
		return a
	case <-time.After(cfg.promptWait):
		t.Fatalf("NOT RUN: no answer within %s to %q", cfg.promptWait, q)
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

// doStep asks the person to do something and waits for "done"; "skip" records the step NOT RUN.
func doStep(t *testing.T, step string) {
	t.Helper()
	for {
		switch strings.ToLower(ask(t, step+" Type done, or skip if it cannot be done.")) {
		case "done":
			return
		case "skip":
			t.Skipf("NOT RUN: the person could not do %q", step)
		}
	}
}

// ---- evidence ----

func evidenceDir(t *testing.T) string {
	t.Helper()
	d := filepath.Join(cfg.out, strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
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
		t.Fatalf("%s: exit %d: %s", what, r.code, strings.TrimSpace(r.err))
	}
	return r.out
}

func wardyn(t *testing.T, args ...string) result { return sh(t, 2*time.Minute, cfg.wardyn, args...) }

// runnerdStatus is the runner's macOS VM state as JSON. Only named keys are read from it.
func runnerdStatus(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(mustOK(t, sh(t, time.Minute, cfg.runnerd, assumedRunnerdStatus...), "runnerd status")), &m); err != nil {
		t.Fatalf("runnerd status JSON: %v", err)
	}
	// The status may carry a one-time setup code while Setup Assistant waits; it is dropped
	// before anything is logged or kept.
	delete(m, "setup_code")
	delete(m, "one_time_code")
	return m
}

// setIdentity switches the strategy for new runs; checkIdentity proves a run got it.
func setIdentity(t *testing.T, s macvm.IdentityStrategy) {
	t.Helper()
	mustOK(t, sh(t, 2*time.Minute, cfg.runnerd, assumedIdentity(s)...), "switch the identity strategy to "+string(s))
}

func checkIdentity(t *testing.T, st macvm.Status, s macvm.IdentityStrategy) {
	t.Helper()
	if st.Identity != s {
		t.Fatalf("run %s was configured with identity %q, want %q: the runner did not switch", st.RunID, st.Identity, s)
	}
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

// runDocument is the kind: Run document every run starts from: WARDYN_MACVM_RUN_DOC when set,
// else an interactive run with no harness. intent gets machine_kind and the caller's fields.
func runDocument(t *testing.T, intent map[string]any) []byte {
	t.Helper()
	doc := map[string]any{"api_version": "wardyn/v1", "kind": "Run", "intent": map[string]any{"agent": "none", "interactive": true}}
	if cfg.runDoc != "" {
		b, err := os.ReadFile(cfg.runDoc)
		if err != nil {
			t.Fatalf("WARDYN_MACVM_RUN_DOC: %v", err)
		}
		doc = nil
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("WARDYN_MACVM_RUN_DOC must be JSON: %v", err)
		}
	}
	in, _ := doc["intent"].(map[string]any)
	if in == nil {
		in = map[string]any{}
		doc["intent"] = in
	}
	in["machine_kind"] = "macos_vm"
	for k, v := range intent {
		in[k] = v
	}
	b, _ := json.Marshal(doc)
	return b
}

// create asks for one macOS VM run. A refusal at create (placement) is returned as an error with
// the server's sentence; the caller decides whether that is the expected outcome.
func create(t *testing.T, intent map[string]any) (string, error) {
	t.Helper()
	r := shIn(t, 2*time.Minute, runDocument(t, intent), cfg.wardyn, "run", "--file", "-", "--json", "--title", "macvm-live "+t.Name())
	if r.code != 0 {
		return "", fmt.Errorf("refused at create: %s", strings.TrimSpace(r.err+" "+r.out))
	}
	var v run
	if err := json.Unmarshal([]byte(r.out), &v); err != nil || v.ID == "" {
		t.Fatalf("create: no run id in %q", r.out)
	}
	t.Cleanup(func() { end(t, v.ID) })
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

// start creates a run and waits for RUNNING, failing the test otherwise.
func start(t *testing.T, intent map[string]any) string {
	t.Helper()
	id, err := create(t, intent)
	if err != nil {
		t.Fatal(err)
	}
	if v := wait(t, id, 5*time.Minute); v.State != "RUNNING" {
		t.Fatalf("run %s is %s, not RUNNING: %s", id, v.State, v.FailureHint)
	}
	return id
}

// refusal is how an ask that should be refused ended: at placement (create), by the runner (the
// run failed with a reason, the ledger's among them), or not refused at all.
type refusal struct {
	By, Reason, RunID string
}

func askRefused(t *testing.T, intent map[string]any) refusal {
	t.Helper()
	id, err := create(t, intent)
	if err != nil {
		return refusal{By: "placement", Reason: err.Error()}
	}
	v := wait(t, id, 3*time.Minute)
	switch {
	case v.State == "RUNNING":
		return refusal{By: "none", RunID: id}
	case v.terminal():
		return refusal{By: "runner", Reason: v.FailureHint, RunID: id}
	default:
		return refusal{By: "none (" + v.State + ")", Reason: v.FailureHint, RunID: id}
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

// helperStatus asks the run's helper over its ctl.sock. The helper serves each connection on its
// own, so this does not disturb runnerd's.
func helperStatus(t *testing.T, id string) macvm.Status {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cc, err := macvm.DialControl(ctx, filepath.Join(runDir(t, id), macvm.ControlSock))
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

// helloAfter waits for the helper's guest_agent event and returns when it came.
func helloAfter(t *testing.T, id string, within time.Duration) (time.Time, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	sock := filepath.Join(runDir(t, id), macvm.ControlSock)
	for {
		cc, err := macvm.DialControl(ctx, sock)
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
	Name  string
	UID   int
	Mode  os.FileMode
	Tomb  bool
	Entry admission.Entry
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
	return out
}

// counted is the ledger's live entries: every non-tombstone entry not tombstoned.
func counted(fs []ledgerFile) int {
	tomb := map[string]bool{}
	for _, f := range fs {
		if f.Tomb {
			tomb[strings.TrimSuffix(f.Name, ".tombstone")] = true
		}
	}
	n := 0
	for _, f := range fs {
		if !f.Tomb && !tomb[f.Name] {
			n++
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
// series did not boot. It is recorded as evidence only: it never qualifies a
// strategy for two concurrent VMs.
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

// qualifies is P22's rule for two concurrent VMs: only distinct configured
// identifiers qualify, so fresh qualifies when its two VMs' configured hashes differ and their
// guests report a different serial number and UUID. travel shares one identifier per base, so it
// is capped at one VM per slot base and qualifies for two only across two slot bases, by the same
// test.
func qualifies(hashes [2]string, ids [2]guestIdentity) (bool, string) {
	switch {
	case hashes[0] == "" || hashes[0] == hashes[1]:
		return false, "the two VMs were configured with the same identifier"
	case ids[0].Serial == ids[1].Serial || ids[0].UUID == ids[1].UUID:
		return false, "the two guests report the same serial number or UUID"
	}
	return true, "distinct configured identifiers and distinct guest identities"
}
