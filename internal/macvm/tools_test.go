// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

func shaHex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// guest is a fake booted base: Push stores files, Run answers a macOS guest
// that has everything installed unless a hook says otherwise.
type guest struct {
	mu    sync.Mutex
	files map[string][]byte
	runs  [][]string
	// sw overrides `softwareupdate` (nil: offers 16.4 and 16.10, installs fine).
	sw func(argv []string) (string, int)
	// version overrides a version check's output.
	version map[string]string
	// passwords are the desktop passwords set, in order; passwordSet, if set,
	// answers whether the nth (1-based) was set.
	passwords   []string
	passwordSet func(n int) bool
	// pointerX/Y is where the guest's pointer is.
	pointerX, pointerY int
}

func newGuest() *guest { return &guest{files: map[string][]byte{}, version: map[string]string{}} }

func (g *guest) Push(_ context.Context, dest string, mode fs.FileMode, r io.Reader, size int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != size || mode != 0o755 {
		return fmt.Errorf("push %s: %d bytes of %d, mode %v", dest, len(b), size, mode)
	}
	g.mu.Lock()
	g.files[dest] = b
	g.mu.Unlock()
	return nil
}

func (g *guest) Run(_ context.Context, argv []string) (string, int, error) {
	g.mu.Lock()
	g.runs = append(g.runs, argv)
	g.mu.Unlock()
	switch {
	case strings.HasSuffix(argv[0], "softwareupdate"):
		if g.sw != nil {
			out, exit := g.sw(argv)
			return out, exit, nil
		}
		if argv[1] == "-l" {
			return "Software Update found the following new or updated software:\n" +
				"* Label: Command Line Tools for Xcode-16.4\n\tTitle: x\n* Label: Command Line Tools for Xcode-16.10\n\tTitle: y\n* Label: macOS 27.1\n", 0, nil
		}
		return "", 0, nil
	case strings.HasSuffix(argv[len(argv)-1], "--version"):
		if v, ok := g.version[argv[0]]; ok {
			return v, 0, nil
		}
		return filepath.Base(argv[0]) + " " + map[string]string{"/usr/local/bin/claude": "2.1.310", "/usr/local/bin/codex": "0.164.0"}[argv[0]], 0, nil
	}
	return "", 0, nil
}

func (g *guest) DesktopPassword(_ context.Context, pw []byte) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.passwords = append(g.passwords, string(pw))
	if g.passwordSet != nil {
		return g.passwordSet(len(g.passwords)), nil
	}
	return true, nil
}

func (g *guest) Pointer(context.Context) (int, int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pointerX, g.pointerY, nil
}

func (g *guest) ToolsDial(context.Context) (string, error) { return macvm.ToolsDialConnected, nil }

func (g *guest) ran(prefix ...string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.ContainsFunc(g.runs, func(a []string) bool {
		return len(a) >= len(prefix) && slices.Equal(a[:len(prefix)], prefix)
	})
}

func (g *guest) ranCount(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, a := range g.runs {
		if strings.HasSuffix(a[0], name) {
			n++
		}
	}
	return n
}

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, m := range []struct {
		n string
		b []byte
	}{{"README", []byte("not it")}, {name, body}} {
		if err := tw.WriteHeader(&tar.Header{Name: m.n, Mode: 0o755, Size: int64(len(m.b)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(m.b)
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

// toolsFixture serves two pinned artifacts from an httptest server.
type toolsFixture struct {
	srv     *httptest.Server
	claude  []byte
	codex   []byte // the member
	archive []byte
	hits    atomic.Int64
	tooling macvm.Tooling
	opts    macvm.ToolsOptions
}

func newTools(t *testing.T) *toolsFixture {
	t.Helper()
	f := &toolsFixture{claude: []byte("claude-binary-2.1.310"), codex: []byte("codex-binary-0.164.0")}
	f.archive = tarGz(t, "codex-aarch64-apple-darwin", f.codex)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		switch r.URL.Path {
		case "/claude":
			_, _ = w.Write(f.claude)
		case "/codex.tar.gz":
			_, _ = w.Write(f.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	f.tooling = macvm.Tooling{Version: 1, Harnesses: []macvm.ToolPin{
		{ID: macvm.HarnessClaudeCode, Version: "2.1.310", URL: f.srv.URL + "/claude", SHA256: shaHex(f.claude), Size: int64(len(f.claude)),
			Dest: "/usr/local/bin/claude", VersionArgv: []string{"/usr/local/bin/claude", "--version"}},
		{ID: macvm.HarnessCodexCLI, Version: "0.164.0", URL: f.srv.URL + "/codex.tar.gz", SHA256: shaHex(f.archive), Size: int64(len(f.archive)),
			Extract: "codex-aarch64-apple-darwin", Dest: "/usr/local/bin/codex", VersionArgv: []string{"/usr/local/bin/codex", "--version"}},
	}}
	f.opts = macvm.ToolsOptions{Tooling: f.tooling, Cache: t.TempDir(), HTTP: f.srv.Client()}
	return f
}

// stage is a ToolsStage whose boot hands out g and counts boots and stops.
func (f *toolsFixture) stage(g *guest, boots, stops *atomic.Int64) *macvm.ToolsStage {
	return &macvm.ToolsStage{
		ToolsOptions: f.opts,
		Boot: func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
			boots.Add(1)
			return g, func(context.Context) error { stops.Add(1); return nil }, nil
		},
	}
}

func runStage(t *testing.T, s macvm.Stage2) macvm.ToolsResult {
	t.Helper()
	res, err := s.Run(context.Background(), macvm.Stage2Input{})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestStage2InstallsPinnedHarnesses(t *testing.T) {
	f, g := newTools(t), newGuest()
	var boots, stops atomic.Int64
	res := runStage(t, f.stage(g, &boots, &stops))
	if want := []string{"none", "claude-code", "codex-cli"}; !slices.Equal(res.Harnesses, want) || len(res.Withheld) != 0 || !res.CLT {
		t.Fatalf("result = %+v", res)
	}
	if !bytes.Equal(g.files["/usr/local/bin/claude"], f.claude) || !bytes.Equal(g.files["/usr/local/bin/codex"], f.codex) {
		t.Fatalf("pushed files = %v", len(g.files))
	}
	if !g.ran("/usr/sbin/softwareupdate", "-i", "Command Line Tools for Xcode-16.10") {
		t.Fatalf("did not install the newest CLT: %v", g.runs)
	}
	if !g.ran("/bin/rm", "-f") || boots.Load() != 1 || stops.Load() != 1 {
		t.Fatalf("trigger left or VM not stopped: boots %d stops %d", boots.Load(), stops.Load())
	}
}

// A download that is not the pinned bytes is refused: nothing is pushed, the
// harness is withheld, the bad file is not kept, and the other harness is
// unaffected.
func TestStage2PinMismatchRefuses(t *testing.T) {
	f, g := newTools(t), newGuest()
	f.claude = []byte("claude-binary-2.1.399") // same length, other bytes: only the hash can tell
	var boots, stops atomic.Int64
	res := runStage(t, f.stage(g, &boots, &stops))
	if want := []string{"none", "codex-cli"}; !slices.Equal(res.Harnesses, want) {
		t.Fatalf("harnesses = %v", res.Harnesses)
	}
	if err := res.Withheld[macvm.HarnessClaudeCode]; !errors.Is(err, macvm.ErrPinMismatch) {
		t.Fatalf("withheld = %v", res.Withheld)
	}
	if _, ok := g.files["/usr/local/bin/claude"]; ok {
		t.Fatal("a mismatched download was pushed")
	}
	if des, _ := os.ReadDir(f.opts.Cache); slices.ContainsFunc(des, func(d os.DirEntry) bool { return strings.HasPrefix(d.Name(), "claude-code") }) {
		t.Fatalf("mismatched download kept: %v", des)
	}
}

// An oversized body is a mismatch too, and a cached file that no longer
// matches is thrown away and fetched again.
func TestStage2CachedFileIsRechecked(t *testing.T) {
	f, g := newTools(t), newGuest()
	var boots, stops atomic.Int64
	runStage(t, f.stage(g, &boots, &stops))
	hits := f.hits.Load()
	runStage(t, f.stage(newGuest(), &boots, &stops))
	if f.hits.Load() != hits {
		t.Fatal("a verified cached download was fetched again")
	}
	if err := os.WriteFile(filepath.Join(f.opts.Cache, "claude-code-2.1.310.download"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	g2 := newGuest()
	runStage(t, f.stage(g2, &boots, &stops))
	if f.hits.Load() != hits+1 || !bytes.Equal(g2.files["/usr/local/bin/claude"], f.claude) {
		t.Fatalf("tampered cache reused: %d hits", f.hits.Load()-hits)
	}
}

func TestStage2VersionCheckWithholds(t *testing.T) {
	f, g := newTools(t), newGuest()
	g.version["/usr/local/bin/codex"] = "codex-cli 0.163.0"
	var boots, stops atomic.Int64
	res := runStage(t, f.stage(g, &boots, &stops))
	if want := []string{"none", "claude-code"}; !slices.Equal(res.Harnesses, want) || res.Withheld[macvm.HarnessCodexCLI] == nil {
		t.Fatalf("result = %+v", res)
	}
}

// No Command Line Tools, no harness: the base still serves "none", and
// nothing is downloaded. The install is tried twice.
func TestStage2CLTFailureLeavesNoneOnly(t *testing.T) {
	f, g := newTools(t), newGuest()
	g.sw = func(argv []string) (string, int) {
		if argv[1] == "-i" {
			return "", 1
		}
		return "* Label: Command Line Tools for Xcode-16.4\n", 0
	}
	var boots, stops atomic.Int64
	res := runStage(t, f.stage(g, &boots, &stops))
	if !slices.Equal(res.Harnesses, []string{"none"}) || res.CLT || len(res.Withheld) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if f.hits.Load() != 0 || len(g.files) != 0 || g.ranCount("softwareupdate") != 4 {
		t.Fatalf("hits %d files %d softwareupdate runs %d", f.hits.Load(), len(g.files), g.ranCount("softwareupdate"))
	}
}

func TestStage2CLTRetriesOnce(t *testing.T) {
	f, g := newTools(t), newGuest()
	installs := 0
	g.sw = func(argv []string) (string, int) {
		if argv[1] == "-i" {
			installs++
			if installs == 1 {
				return "", 1
			}
			return "", 0
		}
		return "* Label: Command Line Tools for Xcode-16.4\n", 0
	}
	var boots, stops atomic.Int64
	if res := runStage(t, f.stage(g, &boots, &stops)); !res.CLT || len(res.Harnesses) != 3 {
		t.Fatalf("result = %+v", res)
	}
}

// With no pins at all, and a guest agent to refresh, stage 2 pushes the agent.
func TestStage2ReplacesGuestAgent(t *testing.T) {
	f, g := newTools(t), newGuest()
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("new agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.opts.Agent = &macvm.AgentPin{Version: "0.10.1", Path: bin, Dest: "/usr/local/libexec/wardyn-guest-agent"}
	var boots, stops atomic.Int64
	res := runStage(t, f.stage(g, &boots, &stops))
	if res.AgentVersion != "0.10.1" || string(g.files["/usr/local/libexec/wardyn-guest-agent"]) != "new agent" {
		t.Fatalf("agent not replaced: %+v", res)
	}
}

func TestParseToolingRefusesLoosePins(t *testing.T) {
	good := func(mut func(p *macvm.ToolPin)) error {
		p := macvm.ToolPin{ID: "claude-code", Version: "2.1.310", URL: "https://example.test/c", SHA256: strings.Repeat("a", 64), Size: 10,
			Dest: "/usr/local/bin/claude", VersionArgv: []string{"/usr/local/bin/claude", "--version"}}
		mut(&p)
		_, err := macvm.ParseTooling([]byte(fmt.Sprintf(`{"version":1,"harnesses":[%s]}`, mustJSON(t, p))))
		return err
	}
	if err := good(func(*macvm.ToolPin) {}); err != nil {
		t.Fatalf("a good pin: %v", err)
	}
	for name, mut := range map[string]func(*macvm.ToolPin){
		"latest":        func(p *macvm.ToolPin) { p.Version = "latest" },
		"range":         func(p *macvm.ToolPin) { p.Version = "2.1" },
		"no sha":        func(p *macvm.ToolPin) { p.SHA256 = "" },
		"upper sha":     func(p *macvm.ToolPin) { p.SHA256 = strings.Repeat("A", 64) },
		"http":          func(p *macvm.ToolPin) { p.URL = "http://example.test/c" },
		"unknown id":    func(p *macvm.ToolPin) { p.ID = "latest" },
		"no size":       func(p *macvm.ToolPin) { p.Size = 0 },
		"relative":      func(p *macvm.ToolPin) { p.Dest = "bin/claude" },
		"no check":      func(p *macvm.ToolPin) { p.VersionArgv = nil },
		"traversal":     func(p *macvm.ToolPin) { p.Dest = "/usr/local/../etc/x" },
		"none is not":   func(p *macvm.ToolPin) { p.ID = "none" },
		"argv not dest": func(p *macvm.ToolPin) { p.VersionArgv = []string{"/usr/bin/true", "--version"} },
	} {
		if err := good(mut); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := macvm.ParseTooling([]byte(`{"version":1,"harnesses":[],"extra":1}`)); err == nil {
		t.Error("unknown field accepted")
	}
}

func mustJSON(t *testing.T, p macvm.ToolPin) string {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(fmt.Sprintf(`{"id":%q,"version":%q,"url":%q,"sha256":%q,"size":%d,"dest":%q,"version_argv":[`, p.ID, p.Version, p.URL, p.SHA256, p.Size, p.Dest))
	for i, a := range p.VersionArgv {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(fmt.Sprintf("%q", a))
	}
	b.WriteString("]}")
	return b.String()
}

// The shipped pins parse, and their ids are the harness catalog's.
func TestShippedToolingMatchesCatalog(t *testing.T) {
	b, err := os.ReadFile("../../deploy/macos-guest/tooling.json")
	if err != nil {
		t.Fatal(err)
	}
	tl, err := macvm.ParseTooling(b)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range tl.Harnesses {
		ids = append(ids, p.ID)
	}
	if !slices.Equal(ids, []string{macvm.HarnessClaudeCode, macvm.HarnessCodexCLI}) {
		t.Fatalf("ids = %v", ids)
	}
	cat, err := os.ReadFile("../api/harness.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if !strings.Contains(string(cat), fmt.Sprintf("ID: %q,", id)) {
			t.Errorf("%s is not a harness in internal/api/harness.go", id)
		}
	}
}

// stage2Prep wires f's Prepare to run the given stage 2.
func stage2Prep(t *testing.T) (*prepFixture, *toolsFixture, *guest) {
	t.Helper()
	f, tf, g := newPrep(t), newTools(t), newGuest()
	var boots, stops atomic.Int64
	f.opts.Stage2 = tf.stage(g, &boots, &stops)
	return f, tf, g
}

func TestPrepareRunsStage2(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"none", "claude-code", "codex-cli"}; !slices.Equal(m.Harnesses, want) {
		t.Fatalf("manifest harnesses = %v", m.Harnesses)
	}
	base := f.opts.Layout.BaseDir(0)
	if macvm.NeedsToolsUpdate(base, tf.tooling, "0.10.0", time.Now()) {
		t.Fatal("a base stage 2 just finished wants an update")
	}
	tf.tooling.Harnesses[0].Version = "2.1.311"
	if !macvm.NeedsToolsUpdate(base, tf.tooling, "0.10.0", time.Now()) || !macvm.NeedsToolsUpdate(base, tf.tooling, "0.10.1", time.Now()) {
		t.Fatal("a changed pin or agent build was not noticed")
	}
	if !f.ev.has(macvm.PhaseTools) {
		t.Fatal("no tools phase reported")
	}
}

// failingStage2 stops the first time as a cut-short stage 2 does (the Mac lost
// power), then runs the real one.
type failingStage2 struct {
	next  macvm.Stage2
	calls atomic.Int64
}

func (s *failingStage2) Run(ctx context.Context, in macvm.Stage2Input) (macvm.ToolsResult, error) {
	if s.calls.Add(1) == 1 {
		return macvm.ToolsResult{}, errors.New("power lost")
	}
	return s.next.Run(ctx, in)
}

// An interrupted stage 2 leaves no ready base: the next Prepare discards the
// partial base and restarts from create with the cached restore image.
func TestPrepareInterruptedStage2RestartsFromCreate(t *testing.T) {
	f, _, _ := stage2Prep(t)
	st := &failingStage2{next: f.opts.Stage2}
	f.opts.Stage2 = st
	if _, err := macvm.Prepare(context.Background(), f.opts); err == nil {
		t.Fatal("the cut-short prepare succeeded")
	}
	if _, err := macvm.ReadyBase(f.opts.Layout.BaseDir(0)); err == nil {
		t.Fatal("a base with an unfinished stage 2 is ready")
	}
	reqs := f.srv.reqs.Load()
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if f.eng.Installs() != 2 || f.fb.Boots() != 2 || st.calls.Load() != 2 {
		t.Fatalf("installs %d first boots %d stage 2 runs %d: want a full restart from create", f.eng.Installs(), f.fb.Boots(), st.calls.Load())
	}
	if f.srv.reqs.Load() != reqs {
		t.Fatal("the restore image was downloaded again")
	}
	if len(m.Harnesses) != 3 {
		t.Fatalf("harnesses = %v", m.Harnesses)
	}
}

// Stage 2 starts only on AC.
func TestPrepareStage2WaitsForAC(t *testing.T) {
	f, _, _ := stage2Prep(t)
	f.ev.on = func(ev macvm.PrepareEvent) {
		if ev.Phase == macvm.PhaseBootstrap && !ev.Paused {
			f.power.set(false) // on battery as stage 1 starts: stage 1 is not paused
		}
		if ev.Phase == macvm.PhaseTools && ev.Paused {
			go f.power.set(true)
		}
	}
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if !f.ev.paused(macvm.PhaseTools) || f.ev.paused(macvm.PhaseBootstrap) {
		t.Fatalf("tools paused %v, bootstrap paused %v", f.ev.paused(macvm.PhaseTools), f.ev.paused(macvm.PhaseBootstrap))
	}
}

func updateOpts(f *prepFixture, tf *toolsFixture, g *guest, agent string) macvm.UpdateOptions {
	var boots, stops atomic.Int64
	return macvm.UpdateOptions{
		Engine: f.eng, Layout: f.opts.Layout, Power: f.power, PowerPoll: f.opts.PowerPoll,
		Stage: tf.stage(g, &boots, &stops), Tooling: tf.tooling, AgentVersion: agent,
		CPUs: 4, MemoryMiB: 8192, HelperVersion: "test2",
	}
}

func TestUpdateToolsClonesAndSwaps(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	before, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	// Up to date: nothing runs.
	g := newGuest()
	if _, changed, err := macvm.UpdateTools(context.Background(), updateOpts(f, tf, g, "0.10.0")); err != nil || changed || len(g.runs) != 0 {
		t.Fatalf("no-op update: changed %v err %v runs %d", changed, err, len(g.runs))
	}
	// New pin and a new guest agent.
	tf.claude = []byte("claude-binary-2.1.311")
	tf.tooling.Harnesses[0].Version, tf.tooling.Harnesses[0].SHA256, tf.tooling.Harnesses[0].Size = "2.1.311", shaHex(tf.claude), int64(len(tf.claude))
	tf.opts.Tooling = tf.tooling
	bin := filepath.Join(t.TempDir(), "agent")
	_ = os.WriteFile(bin, []byte("agent 0.10.1"), 0o600)
	tf.opts.Agent = &macvm.AgentPin{Version: "0.10.1", Path: bin, Dest: "/usr/local/libexec/wardyn-guest-agent"}
	g = newGuest()
	g.version["/usr/local/bin/claude"] = "claude 2.1.311"
	m, changed, err := macvm.UpdateTools(context.Background(), updateOpts(f, tf, g, "0.10.1"))
	if err != nil || !changed {
		t.Fatalf("update: changed %v err %v", changed, err)
	}
	if m.AgentVersion != "0.10.1" || m.HelperVersion != "test2" || m.IPSWSHA256 != before.IPSWSHA256 || !slices.Equal(m.Harnesses, before.Harnesses) {
		t.Fatalf("manifest = %+v", m)
	}
	if got, err := macvm.ReadyBase(base); err != nil || got.AgentVersion != "0.10.1" {
		t.Fatalf("base after swap: %+v %v", got, err)
	}
	if macvm.NeedsToolsUpdate(base, tf.tooling, "0.10.1", time.Now()) {
		t.Fatal("an updated base still wants an update")
	}
	for _, leftover := range []string{base + ".update"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Fatalf("%s left behind", leftover)
		}
	}
	a, _ := os.ReadFile(filepath.Join(base, macvm.MachineIDFile))
	if len(a) == 0 {
		t.Fatal("the swapped base lost its machine identifier")
	}
}

// A failed update leaves the old base as it was.
func TestUpdateToolsFailureKeepsOldBase(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	o := updateOpts(f, tf, newGuest(), "0.10.1")
	o.Stage = &failingStage2{next: o.Stage}
	if _, changed, err := macvm.UpdateTools(context.Background(), o); err == nil || changed {
		t.Fatalf("update: changed %v err %v", changed, err)
	}
	if m, err := macvm.ReadyBase(base); err != nil || m.AgentVersion != "0.10.0" {
		t.Fatalf("old base damaged: %+v %v", m, err)
	}
	if _, err := os.Stat(base + ".update"); !os.IsNotExist(err) {
		t.Fatal("the failed clone was left behind")
	}
}

func (e *events) has(phase string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.ContainsFunc(e.evs, func(ev macvm.PrepareEvent) bool { return ev.Phase == phase })
}

// hookStage is a Stage2 that runs fn.
type hookStage func(ctx context.Context, in macvm.Stage2Input) (macvm.ToolsResult, error)

func (h hookStage) Run(ctx context.Context, in macvm.Stage2Input) (macvm.ToolsResult, error) {
	return h(ctx, in)
}

var noneOnly = macvm.ToolsResult{Harnesses: []string{macvm.HarnessNone}}

// A harness stage 2 withheld is recorded with its reason and not as installed,
// so it is tried again: by an automatic update after the delay, at once when
// its pin changes, and by any update that is asked for.
func TestWithheldHarnessIsRetried(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	good := tf.claude
	tf.claude = []byte("claude-binary-2.1.399") // not what the pin says
	m, err := macvm.Prepare(context.Background(), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"none", "codex-cli"}; !slices.Equal(m.Harnesses, want) {
		t.Fatalf("harnesses = %v", m.Harnesses)
	}
	base := f.opts.Layout.BaseDir(0)
	rec, err := macvm.ReadToolsRecord(base)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.Pins["claude-code"]; ok || rec.Withheld["claude-code"].Reason == "" || rec.Withheld["claude-code"].Version != "2.1.310" || rec.Pins["codex-cli"].Version == "" {
		t.Fatalf("record = %+v", rec)
	}
	tried := rec.TriedAt
	if macvm.NeedsToolsUpdate(base, tf.tooling, "0.10.0", tried.Add(10*time.Minute)) {
		t.Fatal("a withheld harness was retried inside the delay")
	}
	if !macvm.NeedsToolsUpdate(base, tf.tooling, "0.10.0", tried.Add(macvm.ToolsRetryAfter+time.Minute)) {
		t.Fatal("a withheld harness is never retried")
	}
	bumped := tf.tooling
	bumped.Harnesses = slices.Clone(tf.tooling.Harnesses)
	bumped.Harnesses[0].Version = "2.1.311"
	if !macvm.NeedsToolsUpdate(base, bumped, "0.10.0", tried.Add(time.Minute)) {
		t.Fatal("a new pin for a withheld harness waited out the delay")
	}
	// The artifact is fixed. Inside the delay an automatic update does nothing...
	tf.claude = good
	o := updateOpts(f, tf, newGuest(), "0.10.0")
	o.Now = func() time.Time { return tried.Add(10 * time.Minute) }
	if _, changed, err := macvm.UpdateTools(context.Background(), o); err != nil || changed {
		t.Fatalf("update inside the delay: changed %v err %v", changed, err)
	}
	// ...and one that is asked for retries it.
	o.Force = true
	m, changed, err := macvm.UpdateTools(context.Background(), o)
	if err != nil || !changed || !slices.Equal(m.Harnesses, []string{"none", "claude-code", "codex-cli"}) {
		t.Fatalf("forced update: %+v changed %v err %v", m, changed, err)
	}
	if rec, _ := macvm.ReadToolsRecord(base); len(rec.Withheld) != 0 || rec.Pins["claude-code"].Version != "2.1.310" {
		t.Fatalf("record after retry = %+v", rec)
	}
}

// A clone that starts while an update is in stage 2 does not wait for it: it
// takes the old base at once.
func TestCloneDuringUpdateTakesOldBase(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	for n, c := range map[string]string{macvm.DiskFile: "gen1-disk", macvm.AuxFile: "gen1-aux"} {
		if err := os.WriteFile(filepath.Join(base, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runDir := filepath.Join(t.TempDir(), "run")
	o := updateOpts(f, tf, newGuest(), "")
	o.Force = true
	o.Stage = hookStage(func(_ context.Context, in macvm.Stage2Input) (macvm.ToolsResult, error) {
		_ = os.WriteFile(in.Spec.DiskPath, []byte("gen2-disk"), 0o600)
		_ = os.WriteFile(in.Spec.AuxPath, []byte("gen2-aux"), 0o600)
		cloned := make(chan error, 1)
		go func() {
			_, err := macvm.CloneRun(f.eng, base, runDir, macvm.IdentityFresh)
			cloned <- err
		}()
		select {
		case err := <-cloned:
			if err != nil {
				t.Errorf("clone during stage 2: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("a clone waited for the update's stage 2")
		}
		return noneOnly, nil
	})
	if _, changed, err := macvm.UpdateTools(context.Background(), o); err != nil || !changed {
		t.Fatalf("update: changed %v err %v", changed, err)
	}
	d, _ := os.ReadFile(filepath.Join(runDir, macvm.DiskFile))
	a, _ := os.ReadFile(filepath.Join(runDir, macvm.AuxFile))
	if string(d) != "gen1-disk" || string(a) != "gen1-aux" {
		t.Fatalf("clone saw disk %q aux %q, want the old base", d, a)
	}
	if d, _ := os.ReadFile(filepath.Join(base, macvm.DiskFile)); string(d) != "gen2-disk" {
		t.Fatalf("base after the update holds %q", d)
	}
	// A clone after the swap gets the new generation.
	after := filepath.Join(t.TempDir(), "run2")
	if _, err := macvm.CloneRun(f.eng, base, after, macvm.IdentityFresh); err != nil {
		t.Fatal(err)
	}
	if d, _ := os.ReadFile(filepath.Join(after, macvm.DiskFile)); string(d) != "gen2-disk" {
		t.Fatalf("clone after the swap saw %q", d)
	}
	if _, err := os.Stat(base + ".update"); !os.IsNotExist(err) {
		t.Fatal("the old generation was left behind")
	}
}

// Two updates of one base run one after the other.
func TestConcurrentUpdatesSerialise(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	var cur, peak, runs atomic.Int64
	stage := hookStage(func(context.Context, macvm.Stage2Input) (macvm.ToolsResult, error) {
		n := cur.Add(1)
		for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
		}
		runs.Add(1)
		time.Sleep(100 * time.Millisecond)
		cur.Add(-1)
		return noneOnly, nil
	})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := updateOpts(f, tf, newGuest(), "")
			o.Force, o.Stage = true, stage
			_, _, err := macvm.UpdateTools(context.Background(), o)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if peak.Load() != 1 || runs.Load() != 2 {
		t.Fatalf("peak concurrency %d over %d runs", peak.Load(), runs.Load())
	}
}

func TestUpdateToolsRefusesWhatItCannotDo(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	before, _ := os.ReadFile(filepath.Join(base, macvm.ToolsRecordFile))
	// No power source: refused, not a panic.
	o := updateOpts(f, tf, newGuest(), "")
	o.Power, o.Force = nil, true
	if _, _, err := macvm.UpdateTools(context.Background(), o); err == nil {
		t.Fatal("a nil power source was accepted")
	}
	// A guest agent build the stage has no binary for.
	var boots, stops atomic.Int64
	o = updateOpts(f, tf, newGuest(), "0.10.1")
	o.Stage = tf.stage(newGuest(), &boots, &stops)
	if _, _, err := macvm.UpdateTools(context.Background(), o); err == nil || boots.Load() != 0 {
		t.Fatalf("an agent build with nothing to install: err %v", err)
	}
	// A stage that does not report the build it was asked for.
	o.Stage = hookStage(func(context.Context, macvm.Stage2Input) (macvm.ToolsResult, error) { return noneOnly, nil })
	if _, changed, err := macvm.UpdateTools(context.Background(), o); err == nil || changed {
		t.Fatalf("a stage that skipped the agent: changed %v err %v", changed, err)
	}
	if after, _ := os.ReadFile(filepath.Join(base, macvm.ToolsRecordFile)); !bytes.Equal(before, after) {
		t.Fatal("a refused update changed the base")
	}
}

func TestUpdateToolsReportsWaitingForAC(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	f.power.set(false)
	ev := &events{}
	ev.on = func(e macvm.PrepareEvent) {
		if e.Phase == macvm.PhaseTools && e.Paused {
			go f.power.set(true)
		}
	}
	o := updateOpts(f, tf, newGuest(), "")
	o.Force, o.Progress = true, ev.add
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, changed, err := macvm.UpdateTools(ctx, o)
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if !slices.ContainsFunc(ev.evs, func(e macvm.PrepareEvent) bool {
		return e.Phase == macvm.PhaseTools && e.Paused && e.Detail == "waiting for AC power"
	}) {
		t.Fatalf("changed %v err %v events = %+v", changed, err, ev.evs)
	}
}

// A download never follows a redirect off https.
func TestDownloadRefusesRedirectToHTTP(t *testing.T) {
	var plainHits atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainHits.Add(1)
		_, _ = w.Write([]byte("claude-binary-2.1.310"))
	}))
	t.Cleanup(plain.Close)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/claude", http.StatusFound)
	}))
	t.Cleanup(tlsSrv.Close)
	body := []byte("claude-binary-2.1.310")
	pin := macvm.ToolPin{ID: macvm.HarnessClaudeCode, Version: "2.1.310", URL: tlsSrv.URL + "/claude", SHA256: shaHex(body), Size: int64(len(body)),
		Dest: "/usr/local/bin/claude", VersionArgv: []string{"/usr/local/bin/claude", "--version"}}
	st := &macvm.ToolsStage{ToolsOptions: macvm.ToolsOptions{Tooling: macvm.Tooling{Version: 1, Harnesses: []macvm.ToolPin{pin}}, Cache: t.TempDir(), HTTP: tlsSrv.Client()},
		Boot: func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
			return newGuest(), func(context.Context) error { return nil }, nil
		}}
	res := runStage(t, st)
	if err := res.Withheld[macvm.HarnessClaudeCode]; err == nil || !strings.Contains(err.Error(), "redirect") || plainHits.Load() != 0 {
		t.Fatalf("withheld = %v, plain hits %d", res.Withheld, plainHits.Load())
	}
}

// What a crashed update left behind is swept by the next call under the update
// lock, even when that call finds nothing to update.
func TestUpdateToolsSweepsLeftoversEvenWhenCurrent(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	base := f.opts.Layout.BaseDir(0)
	if err := os.MkdirAll(base+".update", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base+".update", macvm.DiskFile), []byte("old generation"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := newGuest()
	if _, changed, err := macvm.UpdateTools(context.Background(), updateOpts(f, tf, g, "0.10.0")); err != nil || changed || len(g.runs) != 0 {
		t.Fatalf("update of a current base: changed %v err %v runs %d", changed, err, len(g.runs))
	}
	if _, err := os.Stat(base + ".update"); !os.IsNotExist(err) {
		t.Fatalf("leftover generation kept: %v", err)
	}
}
