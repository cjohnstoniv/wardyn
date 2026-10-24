// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Harness ids, as the harness catalog and placement know them. "none" ("Your
// own tools") is served by every ready base.
const (
	HarnessNone       = "none"
	HarnessClaudeCode = "claude-code"
	HarnessCodexCLI   = "codex-cli"
)

// PhaseTools is stage 2 of prepare, "Installing developer tools".
const PhaseTools = "tools"

// ToolsRecordFile sits in a base dir and says which tooling the base holds.
const ToolsRecordFile = "tools.json"

// ErrPinMismatch means a download did not match its pinned SHA-256. It is
// never fixed by trying a newer file: the pin ships with the release.
var ErrPinMismatch = errors.New("macvm: downloaded tool does not match its pinned SHA-256")

// maxToolBytes bounds a pinned download and an extracted member.
const maxToolBytes = 1 << 30

// Tooling is deploy/macos-guest/tooling.json: the harness CLIs a base carries,
// each pinned to an exact version and SHA-256.
type Tooling struct {
	Version   int       `json:"version"`
	Harnesses []ToolPin `json:"harnesses"`
}

// ToolPin is one harness CLI. The runner host downloads URL, checks it against
// SHA256 and pushes it to Dest in the guest; Extract names the one member to
// take from a .tar.gz. VersionArgv runs in the guest and must print Version.
type ToolPin struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	URL         string   `json:"url"`
	SHA256      string   `json:"sha256"`
	Size        int64    `json:"size"`
	Extract     string   `json:"extract,omitempty"`
	Dest        string   `json:"dest"`
	VersionArgv []string `json:"version_argv"`
}

var (
	exactVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	sha256Hex    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ParseTooling reads and checks a tooling file. Every pin is an exact
// version with a SHA-256; nothing is ever "latest".
func ParseTooling(b []byte) (Tooling, error) {
	var t Tooling
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return Tooling{}, fmt.Errorf("macvm: tooling: %w", err)
	}
	if t.Version != 1 {
		return Tooling{}, fmt.Errorf("macvm: tooling: unknown format %d", t.Version)
	}
	seen := map[string]bool{}
	for _, p := range t.Harnesses {
		switch {
		case p.ID != HarnessClaudeCode && p.ID != HarnessCodexCLI:
			return Tooling{}, fmt.Errorf("macvm: tooling: %q is not a harness a macOS base serves", p.ID)
		case seen[p.ID]:
			return Tooling{}, fmt.Errorf("macvm: tooling: %s is pinned twice", p.ID)
		case !exactVersion.MatchString(p.Version):
			return Tooling{}, fmt.Errorf("macvm: tooling: %s version %q is not an exact x.y.z", p.ID, p.Version)
		case !sha256Hex.MatchString(p.SHA256):
			return Tooling{}, fmt.Errorf("macvm: tooling: %s has no valid SHA-256 pin", p.ID)
		case !strings.HasPrefix(p.URL, "https://"):
			return Tooling{}, fmt.Errorf("macvm: tooling: %s URL is not https", p.ID)
		case p.Size <= 0 || p.Size > maxToolBytes:
			return Tooling{}, fmt.Errorf("macvm: tooling: %s size %d is out of bounds", p.ID, p.Size)
		case !strings.HasPrefix(p.Dest, "/") || strings.Contains(p.Dest, ".."):
			return Tooling{}, fmt.Errorf("macvm: tooling: %s destination %q is not an absolute guest path", p.ID, p.Dest)
		case len(p.VersionArgv) == 0:
			return Tooling{}, fmt.Errorf("macvm: tooling: %s has no version check", p.ID)
		case p.VersionArgv[0] != p.Dest:
			return Tooling{}, fmt.Errorf("macvm: tooling: %s version check runs %q, not the file it pushes (%s)", p.ID, p.VersionArgv[0], p.Dest)
		}
		seen[p.ID] = true
	}
	return t, nil
}

// ToolsGuest is a booted base in stage 2, reached through the guest agent.
type ToolsGuest interface {
	// Push writes r (size bytes) to dest as root, creating parent dirs.
	Push(ctx context.Context, dest string, mode fs.FileMode, r io.Reader, size int64) error
	// Run runs argv as root and returns its combined output and exit code.
	// err is a transport failure, not a non-zero exit.
	Run(ctx context.Context, argv []string) (out string, exit int, err error)
}

// ToolsBoot boots the base with stage 2's network (the run-style NIC behind
// the prep-only proxy) and returns a guest to work on and a stop. It is
// supplied by the helper's wiring; nothing in this package dials out for it.
type ToolsBoot func(ctx context.Context, spec MachineSpec) (g ToolsGuest, stop func(ctx context.Context) error, err error)

// Stage2Input is what stage 2 works on: the base's machine, and its dir.
type Stage2Input struct {
	Spec MachineSpec
	Dir  string
}

// ToolsResult is what stage 2 left in the guest.
type ToolsResult struct {
	// Harnesses is what the base can serve now: "none" first, then each pinned
	// CLI that is installed and passed its version check.
	Harnesses []string
	// Withheld says why each other pinned harness is not offered.
	Withheld     map[string]error
	CLT          bool
	AgentVersion string // set when the guest agent was replaced
	// Pins are the pins that installed and passed their version check; Tried
	// are all the pins this run attempted.
	Pins  map[string]PinRecord
	Tried map[string]PinRecord
}

// PinRecord is a pin as stage 2 applied it.
type PinRecord struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}

// Stage2 runs stage 2 of prepare, and the tools update, on a base.
type Stage2 interface {
	Run(ctx context.Context, in Stage2Input) (ToolsResult, error)
}

// AgentPin is a guest agent build that stage 2 pushes over the installed one.
type AgentPin struct {
	Version string
	Path    string // the darwin/arm64 binary on the host
	Dest    string // where stage 1 installed it in the guest
}

// ToolsOptions configures ToolsStage.
type ToolsOptions struct {
	Tooling Tooling
	// Cache holds verified downloads between runs.
	Cache string
	HTTP  *http.Client
	Agent *AgentPin
	Log   *slog.Logger
}

// ToolsStage is the Stage2 that boots the base through Boot and applies Tooling.
type ToolsStage struct {
	Boot ToolsBoot
	ToolsOptions
}

var _ Stage2 = (*ToolsStage)(nil)

// Run boots the base, installs the Command Line Tools and the pinned CLIs, and
// stops the VM. A failure of one tool withholds that harness; only a guest that
// cannot be reached, or a cancelled ctx, is an error.
func (s *ToolsStage) Run(ctx context.Context, in Stage2Input) (ToolsResult, error) {
	g, stop, err := s.Boot(ctx, in.Spec)
	if err != nil {
		return ToolsResult{}, fmt.Errorf("macvm: stage 2 boot: %w", err)
	}
	res, err := installTools(ctx, g, s.ToolsOptions)
	sctx, cancel := context.WithTimeout(context.Background(), DefaultStopTimeout)
	defer cancel()
	if serr := stop(sctx); err == nil && serr != nil {
		err = fmt.Errorf("macvm: the stage 2 VM did not shut down cleanly: %w", serr)
	}
	return res, err
}

// cltTrigger is the file softwareupdate looks for to offer the Command Line
// Tools to a non-interactive caller. Apple does not document it (P2b).
const cltTrigger = "/tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress"

const cltGit = "/Library/Developer/CommandLineTools/usr/bin/git"

func installTools(ctx context.Context, g ToolsGuest, o ToolsOptions) (ToolsResult, error) {
	if o.HTTP == nil {
		o.HTTP = http.DefaultClient
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	res := ToolsResult{Harnesses: []string{HarnessNone}, Withheld: map[string]error{}, Pins: map[string]PinRecord{}, Tried: map[string]PinRecord{}}
	for _, p := range o.Tooling.Harnesses {
		res.Tried[p.ID] = PinRecord{Version: p.Version, SHA256: p.SHA256}
	}
	if o.Agent != nil {
		f, err := os.Open(o.Agent.Path)
		if err != nil {
			return res, fmt.Errorf("macvm: guest agent: %w", err)
		}
		fi, err := f.Stat()
		if err == nil {
			err = g.Push(ctx, o.Agent.Dest, 0o755, f, fi.Size())
		}
		_ = f.Close()
		if err != nil {
			return res, fmt.Errorf("macvm: replacing the guest agent: %w", err)
		}
		res.AgentVersion = o.Agent.Version
	}
	var cltErr error
	for attempt := 0; attempt < 2; attempt++ {
		if cltErr = installCLT(ctx, g); cltErr == nil || ctx.Err() != nil {
			break
		}
		o.Log.Warn("macvm: Command Line Tools install failed", "attempt", attempt+1, "err", cltErr.Error())
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	res.CLT = cltErr == nil
	for _, p := range o.Tooling.Harnesses {
		if cltErr != nil {
			res.Withheld[p.ID] = fmt.Errorf("the Command Line Tools did not install: %w", cltErr)
			continue
		}
		err := installPin(ctx, g, o, p)
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if err != nil {
			o.Log.Warn("macvm: harness withheld", "harness", p.ID, "err", err.Error())
			res.Withheld[p.ID] = err
			continue
		}
		res.Harnesses = append(res.Harnesses, p.ID)
		res.Pins[p.ID] = res.Tried[p.ID]
	}
	return res, nil
}

// installCLT installs the Command Line Tools through softwareupdate, the
// non-interactive route Homebrew's installer takes, and checks git runs.
func installCLT(ctx context.Context, g ToolsGuest) error {
	run := func(argv ...string) (string, error) {
		out, exit, err := g.Run(ctx, argv)
		if err == nil && exit != 0 {
			err = fmt.Errorf("%s exited %d", argv[0], exit)
		}
		return out, err
	}
	if _, err := run("/usr/bin/touch", cltTrigger); err != nil {
		return err
	}
	defer func() { _, _, _ = g.Run(context.WithoutCancel(ctx), []string{"/bin/rm", "-f", cltTrigger}) }()
	list, err := run("/usr/sbin/softwareupdate", "-l")
	if err != nil {
		return err
	}
	label := pickCLTLabel(list)
	if label == "" {
		return errors.New("softwareupdate offered no Command Line Tools")
	}
	if _, err := run("/usr/sbin/softwareupdate", "-i", label); err != nil {
		return err
	}
	_, err = run(cltGit, "--version")
	return err
}

// pickCLTLabel is the newest Command Line Tools label in `softwareupdate -l`.
func pickCLTLabel(list string) string {
	var labels []string
	for _, line := range strings.Split(list, "\n") {
		l, ok := strings.CutPrefix(strings.TrimSpace(line), "* ")
		if !ok {
			continue
		}
		l = strings.TrimSpace(strings.TrimPrefix(l, "Label:"))
		if strings.Contains(l, "Command Line Tools") {
			labels = append(labels, l)
		}
	}
	if len(labels) == 0 {
		return ""
	}
	slices.SortStableFunc(labels, cmpVersionLabel)
	return labels[len(labels)-1]
}

var digits = regexp.MustCompile(`[0-9]+`)

// cmpVersionLabel orders labels by the numbers in them, so 16.10 > 16.4.
func cmpVersionLabel(a, b string) int {
	x, y := digits.FindAllString(a, -1), digits.FindAllString(b, -1)
	for i := 0; i < len(x) && i < len(y); i++ {
		m, _ := strconv.Atoi(x[i])
		n, _ := strconv.Atoi(y[i])
		if m != n {
			return m - n
		}
	}
	return len(x) - len(y)
}

// installPin downloads and checks one CLI, pushes it and runs its version check.
func installPin(ctx context.Context, g ToolsGuest, o ToolsOptions, p ToolPin) error {
	f, err := fetchPinned(ctx, o.HTTP, p, o.Cache)
	if err != nil {
		return err
	}
	r, size, err := pinnedBytes(f, p) // closes f on error
	if err == nil {
		err = g.Push(ctx, p.Dest, 0o755, r, size)
		_ = r.Close()
	}
	if err != nil {
		return fmt.Errorf("pushing %s: %w", p.ID, err)
	}
	out, exit, err := g.Run(ctx, p.VersionArgv)
	switch {
	case err != nil:
		return err
	case exit != 0:
		return fmt.Errorf("%s version check exited %d", p.ID, exit)
	case !strings.Contains(out, p.Version):
		return fmt.Errorf("%s version check printed %q, not %s", p.ID, strings.TrimSpace(out), p.Version)
	}
	return nil
}

// httpsOnly is a client that refuses a redirect to anything but https.
func httpsOnly(c *http.Client) *http.Client {
	cc := *c
	prev := c.CheckRedirect
	cc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing a redirect to %s", req.URL.Redacted())
		}
		if prev != nil {
			return prev(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &cc
}

// fetchPinned returns p's download in dir as an open file at offset 0, checked
// against its SHA-256 through that same handle, so what is pushed is what was
// checked. A file already in the cache is reused only if it still matches. A
// download that does not match is deleted and refused. The caller closes it.
func fetchPinned(ctx context.Context, c *http.Client, p ToolPin, dir string) (*os.File, error) {
	if err := mkdirPrivate(dir); err != nil {
		return nil, err
	}
	final := filepath.Join(dir, p.ID+"-"+p.Version+".download")
	if f, err := os.Open(final); err == nil {
		if ok, herr := hasSHA256(f, p.SHA256); herr == nil && ok {
			return f, nil
		}
		_ = f.Close()
		_ = os.Remove(final)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpsOnly(c).Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", p.ID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", p.ID, resp.Status)
	}
	part := final + ".part"
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer os.Remove(part)
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, p.Size+1))
	if err == nil && (n > p.Size || hex.EncodeToString(h.Sum(nil)) != p.SHA256) {
		err = fmt.Errorf("%w: %s %s", ErrPinMismatch, p.ID, p.Version)
	} else if err != nil {
		err = fmt.Errorf("downloading %s: %w", p.ID, err)
	}
	if err == nil {
		err = os.Rename(part, final) // f still names the checked inode
	}
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// hasSHA256 hashes f from the start and leaves it at offset 0.
func hasSHA256(f *os.File, want string) (bool, error) {
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	_, err := f.Seek(0, io.SeekStart)
	return hex.EncodeToString(h.Sum(nil)) == want, err
}

type pinnedReader struct {
	io.Reader
	closers []io.Closer
}

func (r pinnedReader) Close() error {
	var first error
	for _, c := range r.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// pinnedBytes is the bytes to push from the checked file f: the file itself,
// or the one tar.gz member p names. Closing the result closes f.
func pinnedBytes(f *os.File, p ToolPin) (io.ReadCloser, int64, error) {
	if p.Extract == "" {
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, 0, err
		}
		return f, fi.Size(), nil
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("%s archive: %w", p.ID, err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err != nil {
			_ = f.Close()
			if errors.Is(err, io.EOF) {
				return nil, 0, fmt.Errorf("%s archive has no %s", p.ID, p.Extract)
			}
			return nil, 0, fmt.Errorf("%s archive: %w", p.ID, err)
		}
		if h.Typeflag == tar.TypeReg && h.Name == p.Extract {
			if h.Size <= 0 || h.Size > maxToolBytes {
				_ = f.Close()
				return nil, 0, fmt.Errorf("%s member %s has size %d", p.ID, p.Extract, h.Size)
			}
			return pinnedReader{tr, []io.Closer{zr, f}}, h.Size, nil
		}
	}
}

// ToolsRetryAfter is how long an automatic tools update waits before trying a
// harness again that stage 2 withheld. Any update that does run tries every
// pin again.
const ToolsRetryAfter = time.Hour

// WithheldRecord says which pin a harness was withheld at, and why.
type WithheldRecord struct {
	PinRecord
	Reason string `json:"reason"`
}

// ToolsRecord is tools.json: what a base's stage 2 applied. Pins are only
// those that installed and passed their version check; Withheld holds the rest
// with the reason, for the status the drawer reads.
type ToolsRecord struct {
	AgentVersion string                    `json:"agent_version,omitempty"`
	Pins         map[string]PinRecord      `json:"pins"`
	Withheld     map[string]WithheldRecord `json:"withheld,omitempty"`
	Harnesses    []string                  `json:"harnesses"`
	TriedAt      time.Time                 `json:"tried_at"`
}

// ReadToolsRecord reads a base's tools.json.
func ReadToolsRecord(baseDir string) (ToolsRecord, error) {
	var rec ToolsRecord
	b, err := os.ReadFile(filepath.Join(baseDir, ToolsRecordFile))
	if err != nil {
		return rec, err
	}
	return rec, json.Unmarshal(b, &rec)
}

func writeToolsRecord(dir string, r ToolsResult, agentVersion string, at time.Time) error {
	rec := ToolsRecord{AgentVersion: agentVersion, Pins: r.Pins, Harnesses: r.Harnesses, TriedAt: at.UTC()}
	for id, err := range r.Withheld {
		if rec.Withheld == nil {
			rec.Withheld = map[string]WithheldRecord{}
		}
		rec.Withheld[id] = WithheldRecord{PinRecord: r.Tried[id], Reason: err.Error()}
	}
	return writeJSONAtomic(filepath.Join(dir, ToolsRecordFile), rec, 0o600)
}

// NeedsToolsUpdate says whether the base in baseDir lacks the pins in t or
// the guest agent build agentVersion ("" skips the agent check). A harness
// withheld at the current pin does not count for ToolsRetryAfter after the
// attempt; a new pin counts at once.
func NeedsToolsUpdate(baseDir string, t Tooling, agentVersion string, now time.Time) bool {
	rec, err := ReadToolsRecord(baseDir)
	if err != nil {
		return true
	}
	if agentVersion != "" && rec.AgentVersion != agentVersion {
		return true
	}
	for _, p := range t.Harnesses {
		want := PinRecord{Version: p.Version, SHA256: p.SHA256}
		if rec.Pins[p.ID] == want {
			continue
		}
		if w, ok := rec.Withheld[p.ID]; ok && w.PinRecord == want && now.Sub(rec.TriedAt) < ToolsRetryAfter {
			continue
		}
		return true
	}
	return false
}

// baseLockPath is the lock on one base: shared while a run is cloned from it,
// exclusive only around an update's swap. updateLockPath serialises updates
// for their whole run. Both sit beside the base dir, which an update swaps out
// from under any lock inside it.
func baseLockPath(baseDir string) string   { return baseDir + ".lock" }
func updateLockPath(baseDir string) string { return baseDir + ".update.lock" }

// UpdateOptions configures UpdateTools.
type UpdateOptions struct {
	Engine Engine
	Layout Layout
	Slot   int
	Power  PowerSource
	// Admit takes the host ledger entry and returns its release; the update
	// is a VM like any other (nil: the caller already holds one).
	Admit func(ctx context.Context) (release func(), err error)
	Stage Stage2
	// Tooling and AgentVersion are what the base should end up with.
	Tooling      Tooling
	AgentVersion string
	// Force runs the update even when the base looks current or a withheld
	// harness is inside its retry delay: an update someone asked for.
	Force bool
	// CPUs and MemoryMiB are the runner's per-run limits, never raised.
	CPUs          int
	MemoryMiB     uint64
	HelperVersion string
	PowerPoll     time.Duration
	Progress      func(PrepareEvent)
	// Now is the clock (nil: time.Now).
	Now func() time.Time
}

// UpdateTools brings a ready base up to the current pins and guest agent: it
// clones the base, runs stage 2 on the clone, and swaps the clone in as the
// base with one atomic exchange. The whole run holds the update lock, so two
// updates serialise; runs keep cloning the old base meanwhile. Only the
// exchange holds the base's lock exclusively, so a clone (which holds it
// shared) sees the old base or the new one, never a mix. A failed or
// interrupted update leaves the old base as it was. It reports whether
// anything changed.
func UpdateTools(ctx context.Context, o UpdateOptions) (Manifest, bool, error) {
	if o.Engine == nil || o.Power == nil || o.Stage == nil || o.Layout.Root == "" {
		return Manifest{}, false, errors.New("macvm: a tools update needs an engine, a power source, a stage 2 and a root")
	}
	if o.Progress == nil {
		o.Progress = func(PrepareEvent) {}
	}
	if o.PowerPoll == 0 {
		o.PowerPoll = 2 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	base := o.Layout.BaseDir(o.Slot)
	nb, done, err := beginUpdate(ctx, base)
	if err != nil {
		return Manifest{}, false, err
	}
	defer done()
	m, err := ReadyBase(base)
	if err != nil {
		return m, false, err
	}
	if !o.Force && !NeedsToolsUpdate(base, o.Tooling, o.AgentVersion, o.Now()) {
		return m, false, nil
	}
	drifted := o.AgentVersion != "" && m.AgentVersion != o.AgentVersion
	// An agent build stage 2 cannot install would leave the base "drifted"
	// for ever, and every start would reboot it: refuse before booting.
	if ts, ok := o.Stage.(*ToolsStage); ok && drifted && (ts.Agent == nil || ts.Agent.Version != o.AgentVersion) {
		return m, false, fmt.Errorf("macvm: the guest agent build is %s but this update has no such build to install", o.AgentVersion)
	}
	if err := thermalGate(o.Engine); err != nil {
		return m, false, err
	}
	if o.Admit != nil {
		release, err := o.Admit(ctx)
		if err != nil {
			return m, false, err
		}
		defer release()
	}
	// Like prepare's later steps: starts only on AC, never paused.
	d := &downloader{power: o.Power, poll: o.PowerPoll, paused: func(on bool) {
		ev := PrepareEvent{Phase: PhaseTools, Paused: on}
		if on {
			ev.Detail = "waiting for AC power"
		}
		o.Progress(ev)
	}}
	if err := d.waitAC(ctx); err != nil {
		return m, false, err
	}
	o.Progress(PrepareEvent{Phase: PhaseTools})
	id, err := cloneRun(o.Engine, base, nb, IdentityTravel)
	if err != nil {
		return m, false, err
	}
	spec := MachineSpec{
		CPUs: o.CPUs, MemoryBytes: o.MemoryMiB * mib, MinCPUs: m.MinCPUs, MinMemoryBytes: m.MinMemoryMiB * mib,
		HardwareModel: m.HardwareModel, MachineIdentifier: id.MachineIdentifier,
		DiskPath: filepath.Join(nb, DiskFile), AuxPath: filepath.Join(nb, AuxFile),
	}
	res, err := o.Stage.Run(ctx, Stage2Input{Spec: spec, Dir: nb})
	if err != nil {
		return m, false, err
	}
	if drifted && res.AgentVersion != o.AgentVersion {
		return m, false, fmt.Errorf("macvm: stage 2 did not install guest agent build %s", o.AgentVersion)
	}
	agent := m.AgentVersion
	if res.AgentVersion != "" {
		agent = res.AgentVersion
	}
	m.AgentVersion, m.Harnesses, m.HelperVersion = agent, res.Harnesses, o.HelperVersion
	if err := writeToolsRecord(nb, res, agent, o.Now()); err != nil {
		return m, false, err
	}
	if err := writeJSONAtomic(filepath.Join(nb, ManifestFile), m, 0o600); err != nil {
		return m, false, err
	}
	if err := swapBase(ctx, nb, base); err != nil {
		return m, false, err
	}
	o.Progress(PrepareEvent{Phase: PhaseReady, Fraction: 1})
	return m, true, nil
}

// beginUpdate takes base's update lock for a tools update or a re-prepare and
// sweeps what a crashed one left in the staging dir <base>.update (a half-built
// base, or an old generation after a swap), whether or not the caller goes on
// to build. It returns the staging dir; done deletes it (after a swap, the old
// generation, so that goes once the base lock is released) and then drops the
// update lock.
func beginUpdate(ctx context.Context, base string) (staging string, done func(), err error) {
	staging = base + ".update"
	unlock, err := lockFile(ctx, updateLockPath(base), true)
	if err != nil {
		return "", nil, err
	}
	if err := os.RemoveAll(staging); err != nil {
		unlock()
		return "", nil, err
	}
	return staging, func() { _ = os.RemoveAll(staging); unlock() }, nil
}

// swapBase exchanges the updated base nb with base while holding base's lock
// exclusively; nb then holds the old generation.
func swapBase(ctx context.Context, nb, base string) error {
	unlock, err := lockFile(ctx, baseLockPath(base), true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := swapFn(nb, base); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS) {
			return fmt.Errorf("macvm: the macOS VM directory must be on an APFS volume, which can swap two directories at once (%v); the base was left as it was", err)
		}
		return fmt.Errorf("macvm: swapping in the updated base: %w", err)
	}
	return nil
}

// swapFn is swapDirs; tests replace it to force a failing exchange. There is
// no fallback to two renames: a base that is half swapped is not a base.
var swapFn = swapDirs
