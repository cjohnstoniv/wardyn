// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-macvm is the per-VM helper behind macOS VM sandboxes. runnerd
// starts one per macOS VM, in a new session so the VM outlives a runnerd
// restart:
//
//	wardyn-macvm run --dir <run-dir>     run one VM from <run-dir>/run.json
//	wardyn-macvm status --dir <run-dir>  print a running helper's status (JSON)
//	wardyn-macvm prepare --root <dir> --cpus N --memory-mib M
//	                                     build this Mac's macOS base image
//	wardyn-macvm update-tools --root <dir> --cpus N --memory-mib M
//	                                     bring the base up to the shipped tool pins
//	wardyn-macvm probe                   say (JSON) whether this Mac and build can run macOS VMs
//	wardyn-macvm version
//
// It is the only Wardyn binary that uses Virtualization.framework (and the
// only one signed with its entitlement); runnerd itself stays cgo-free.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/macvm/stage1"
	"github.com/cjohnstoniv/wardyn/internal/version"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
	"golang.org/x/sys/unix"
)

// deps are what main wires and tests replace.
type deps struct {
	engine func() (macvm.Engine, error)
	host   admission.Host
	stdout io.Writer
	stderr io.Writer
	// prepare supplies prepare's first-boot provisioner, prep network and
	// stage 1, which other parts of the build provide.
	prepare func(*macvm.PrepareOptions) error
	// firstBoot is the provisioning first boot (macvm.NewProvisioner), which
	// probe asks whether this host can run it.
	firstBoot macvm.FirstBooter
	// fstat replaces fstat(2) in the ledger's ownership checks (tests).
	fstat func(fd int, name string) (unix.Stat_t, error)
	// getuid replaces os.Getuid (tests).
	getuid func() int
	// sockFstat replaces fstat(2) in the socket directory checks (tests).
	sockFstat func(fd int, name string) (unix.Stat_t, error)
	// powerSource replaces pmset for a tools update (tests).
	powerSource macvm.PowerSource
}

func (d deps) power() macvm.PowerSource {
	if d.powerSource != nil {
		return d.powerSource
	}
	return macvm.PmsetPower{}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], mainDeps()))
}

// mainDeps are the helper's real deps.
func mainDeps() deps {
	return deps{
		engine: platformEngine, host: admission.SystemHost(), stdout: os.Stdout, stderr: os.Stderr,
		prepare: prepareDeps, firstBoot: macvm.NewProvisioner(),
	}
}

func run(ctx context.Context, args []string, d deps) int {
	if len(args) == 0 {
		fmt.Fprintln(d.stderr, "usage: wardyn-macvm run|status|prepare|update-tools|probe|ledger-setup|version [flags]")
		return 2
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(d.stdout, version.String())
		return 0
	case "run":
		return cmdRun(ctx, args[1:], d)
	case "status":
		return cmdStatus(ctx, args[1:], d)
	case macvm.PrepareCommand:
		return cmdPrepare(ctx, args[1:], d)
	case macvm.UpdateToolsCommand:
		return cmdUpdateTools(ctx, args[1:], d)
	case "probe":
		return cmdProbe(d)
	case "ledger-setup":
		return cmdLedgerSetup(args[1:], d)
	}
	fmt.Fprintf(d.stderr, "wardyn-macvm: unknown command %q\n", args[0])
	return 2
}

// cmdLedgerSetup is the runner package postinstall's step, as root: make the
// macOS VM admission ledger under the base directory exact (admission.Setup).
func cmdLedgerSetup(args []string, d deps) int {
	if len(args) != 1 {
		fmt.Fprintln(d.stderr, "usage: wardyn-macvm ledger-setup <base directory>")
		return 2
	}
	uid := os.Getuid
	if d.getuid != nil {
		uid = d.getuid
	}
	if uid() != 0 {
		fmt.Fprintln(d.stderr, "wardyn-macvm ledger-setup: must run as root (the runner package's installer runs it)")
		return 1
	}
	if err := admission.Setup(admission.SetupConfig{Base: args[0], Fstat: d.fstat}); err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm ledger-setup: %v\n", err)
		return 1
	}
	return 0
}

func cmdRun(ctx context.Context, args []string, d deps) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	dir := fs.String("dir", "", "the run directory runnerd prepared (holds run.json)")
	if fs.Parse(args) != nil || *dir == "" {
		fmt.Fprintln(d.stderr, "wardyn-macvm run: --dir is required")
		return 2
	}
	logf, err := macvm.OpenRotatingLog(filepath.Join(*dir, macvm.HelperLog), macvm.HelperLogMax)
	if err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	defer logf.Close()
	log := slog.New(slog.NewJSONHandler(logf, nil))
	eng, err := d.engine()
	if err != nil {
		log.Error("wardyn-macvm: engine", "err", err.Error())
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	// The run's VNC password: runnerd reads it from ctl.sock for the guest agent's Init, and the shim
	// logs in to the guest's Screen Sharing with it.
	pw, err := macvm.NewDesktopSecret()
	if err != nil {
		log.Error("wardyn-macvm: desktop password", "err", err.Error())
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	err = macvm.Run(ctx, macvm.RunOptions{
		Engine: eng, RunDir: *dir, Log: log, SockFstat: d.sockFstat,
		Desktop:         &macvm.RFBShim{Credential: pw.Credential, Log: log},
		DesktopPassword: pw,
		Claim: func(c macvm.RunConfig) error {
			if c.LedgerEntry == "" || c.RunnerID == "" {
				return errors.New("run.json names no admission entry")
			}
			return claim(c, d)
		},
		HoldIdleSleep: holdIdleSleep,
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("wardyn-macvm: run ended", "err", err.Error())
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	return 0
}

// claim records this helper in its admission entry before the VM is built.
// Every macOS VM holds one; callers refuse a missing entry in their own words.
func claim(c macvm.RunConfig, d deps) error {
	host := d.host
	dir := c.LedgerDir
	if dir == "" {
		dir = admission.DefaultDir
	}
	l, err := admission.Open(admission.Config{Dir: dir, UID: os.Getuid(), RunnerID: c.RunnerID, Host: host, Fstat: d.fstat})
	if err != nil {
		return err
	}
	start, err := admission.Self(host, os.Getpid())
	if err != nil {
		return err
	}
	_, err = l.ClaimHelper(c.LedgerEntry, os.Getpid(), start)
	return err
}

// holdIdleSleep holds an idle-sleep assertion while the VM runs (macOS only).
func holdIdleSleep(pid int) func() {
	if runtime.GOOS != "darwin" {
		return func() {}
	}
	argv := macvm.IdleSleepCommand(pid)
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return func() {}
	}
	return func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }
}

func cmdStatus(ctx context.Context, args []string, d deps) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	dir := fs.String("dir", "", "the run directory")
	if fs.Parse(args) != nil || *dir == "" {
		fmt.Fprintln(d.stderr, "wardyn-macvm status: --dir is required")
		return 2
	}
	cfg, err := macvm.ReadRunConfig(*dir)
	if err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	sock, err := macvm.RunSockDir(cfg, d.sockFstat)
	if err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	cc, err := macvm.DialControl(ctx, filepath.Join(sock, macvm.ControlSock))
	if err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	defer cc.Close()
	st, err := cc.Status(ctx)
	if err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(d.stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(st)
	return 0
}

// jobFlags are the flags prepare and update-tools share.
type jobFlags struct {
	root, runner, entry, ledgerDir, sockRoot *string
	slot, cpus                               *int
	memMiB                                   *uint64
}

func newJobFlags(fs *flag.FlagSet) jobFlags {
	return jobFlags{
		root:      fs.String(macvm.FlagRoot, "", "the macos-vm.noindex directory (default: the user's)"),
		slot:      fs.Int(macvm.FlagSlot, 0, "the base slot"),
		cpus:      fs.Int(macvm.FlagCPUs, 0, "the runner's per-run CPU limit (never raised)"),
		memMiB:    fs.Uint64(macvm.FlagMemoryMiB, 0, "the runner's per-run memory limit in MiB (never raised)"),
		runner:    fs.String(macvm.FlagRunnerID, "", "the runner whose admission entry this job holds"),
		entry:     fs.String(macvm.FlagLedgerEntry, "", "the admission entry runnerd took for this job"),
		ledgerDir: fs.String(macvm.FlagLedgerDir, "", "the admission ledger directory (default: the package's)"),
		sockRoot:  fs.String(macvm.FlagSockRoot, "", "the socket root (default: the package's)"),
	}
}

// parse parses args and fills the default root; it prints the usage error
// and returns its exit code, or 0.
func (j jobFlags) parse(fs *flag.FlagSet, args []string, d deps) int {
	if fs.Parse(args) != nil || *j.cpus <= 0 || *j.memMiB == 0 {
		fmt.Fprintf(d.stderr, "wardyn-macvm %s: --%s and --%s are required\n", fs.Name(), macvm.FlagCPUs, macvm.FlagMemoryMiB)
		return 2
	}
	if *j.root == "" {
		r, err := macvm.DefaultRoot()
		if err != nil {
			fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
			return 1
		}
		*j.root = r
	}
	return 0
}

// admit is the job's Admit: runnerd took the entry; the helper claims it
// before any VM exists, and runnerd releases it after verified destruction.
func (j jobFlags) admit(name string, d deps) func(context.Context) (func(), error) {
	return func(context.Context) (func(), error) {
		if *j.runner == "" || *j.entry == "" {
			return nil, fmt.Errorf("%s needs --%s and --%s: runnerd takes the admission entry before it starts %s", name, macvm.FlagRunnerID, macvm.FlagLedgerEntry, name)
		}
		err := claim(macvm.RunConfig{RunnerID: *j.runner, LedgerEntry: *j.entry, LedgerDir: *j.ledgerDir}, d)
		return func() {}, err
	}
}

// stage2 is the job's stage 2: the shipped pins, applied over a stage-2 boot
// whose ctl.sock (JobControlSock) takes runnerd's net_fd.
func (j jobFlags) stage2(eng macvm.Engine, d deps) (*macvm.ToolsStage, error) {
	if *j.entry == "" {
		return nil, fmt.Errorf("a stage-2 boot needs --%s: its socket directory is named by the entry", macvm.FlagLedgerEntry)
	}
	t, err := macvm.ShippedTooling()
	if err != nil {
		return nil, err
	}
	agent, err := guestAgentPath()
	if err != nil {
		return nil, err
	}
	boot := macvm.Stage2BootConfig{Engine: eng, SockRoot: *j.sockRoot, SockKey: macvm.JobSockKey(*j.entry), SockFstat: d.sockFstat}
	return &macvm.ToolsStage{Boot: boot.ToolsBoot(), ToolsOptions: macvm.ToolsOptions{
		Tooling: t, Cache: filepath.Join(macvm.Layout{Root: *j.root}.CacheDir(), "tools"),
		// Stage 2 replaces the guest agent where stage 1 installed it, with
		// the build beside this helper (§3.4, guest agent drift).
		Agent: &macvm.AgentPin{Version: version.String(), Path: agent, Dest: guestagent.MacAgentBinaryPath},
	}}, nil
}

func cmdPrepare(ctx context.Context, args []string, d deps) int {
	fs := flag.NewFlagSet(macvm.PrepareCommand, flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	j := newJobFlags(fs)
	keep := fs.Bool(macvm.FlagKeepIPSW, false, "keep the downloaded restore image")
	reprepare := fs.Bool(macvm.FlagReprepare, false, "build a new base even when one is ready (a newer restore image), and swap it in")
	byHand := fs.Duration("setup-by-hand-wait", macvm.DefaultSetupByHandWait, "how long to wait for macOS's setup by hand (0: stop instead)")
	if code := j.parse(fs, args, d); code != 0 {
		return code
	}
	eng, err := d.engine()
	if err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(d.stdout)
	o := macvm.PrepareOptions{
		Engine: eng, Layout: macvm.Layout{Root: *j.root}, Slot: *j.slot, Power: d.power(),
		CPUs: *j.cpus, MemoryMiB: *j.memMiB, KeepIPSW: *keep, Reprepare: *reprepare, HelperVersion: version.String(),
		SetupByHandWait: *byHand,
		Progress:        func(ev macvm.PrepareEvent) { _ = enc.Encode(ev) },
		Admit:           j.admit(macvm.PrepareCommand, d),
	}
	if *j.entry != "" {
		st, err := j.stage2(eng, d)
		if err != nil {
			fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
			return 1
		}
		o.Stage2 = st
	}
	if err := d.prepare(&o); err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	if _, err := macvm.Prepare(ctx, o); err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	return 0
}

// cmdUpdateTools is `wardyn-macvm update-tools`, which runnerd starts under
// a tools_update ledger entry: macvm.UpdateTools with the shipped pins, its
// stage 2 on a stage-2 boot that takes runnerd's net_fd.
func cmdUpdateTools(ctx context.Context, args []string, d deps) int {
	fs := flag.NewFlagSet(macvm.UpdateToolsCommand, flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	j := newJobFlags(fs)
	force := fs.Bool(macvm.FlagForce, false, "update even when the base looks current, and retry every withheld harness")
	if code := j.parse(fs, args, d); code != 0 {
		return code
	}
	eng, err := d.engine()
	if err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	st, err := j.stage2(eng, d)
	if err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(d.stdout)
	o := macvm.UpdateOptions{
		Engine: eng, Layout: macvm.Layout{Root: *j.root}, Slot: *j.slot, Power: d.power(), Admit: j.admit(macvm.UpdateToolsCommand, d),
		Stage: st, Tooling: st.Tooling, AgentVersion: version.String(), Force: *force, CPUs: *j.cpus, MemoryMiB: *j.memMiB, HelperVersion: version.String(),
		Progress: func(ev macvm.PrepareEvent) { _ = enc.Encode(ev) },
		// The staging-cleanup warning goes to the job's log (runnerd's tools-update.log), not nowhere.
		Log: slog.New(slog.NewTextHandler(d.stderr, nil)),
	}
	if _, _, err := macvm.UpdateTools(ctx, o); err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	return 0
}

// prepareDeps wires prepare's first-boot provisioner (macvm.NewProvisioner),
// prep network (internal/vmnet) and stage 1. Stage 1 installs the
// wardyn-guest-agent that sits beside this helper. Where this build or host
// cannot provision a first boot, prepare is refused with the provisioner's
// reason.
func prepareDeps(o *macvm.PrepareOptions) error {
	if o.FirstBoot == nil {
		o.FirstBoot = macvm.NewProvisioner()
	}
	if o.PrepNet == nil {
		o.PrepNet = newPrepNet
	}
	if o.Bootstrap == nil {
		agent, err := guestAgentPath()
		if err != nil {
			return err
		}
		o.Bootstrap = stage1.New(stage1.Config{AgentPath: agent, AgentVersion: version.String()})
	}
	if err := o.FirstBoot.Supported(); err != nil {
		return fmt.Errorf("%w; prepare is not available", err)
	}
	return nil
}

// guestAgentPath is the darwin/arm64 wardyn-guest-agent beside this helper:
// stage 1 installs it, and stage 2 replaces the guest's copy with it.
func guestAgentPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("this helper's own path: %w", err)
	}
	return filepath.Join(filepath.Dir(self), "wardyn-guest-agent"), nil
}

// probeReport is what probe prints.
type probeReport struct {
	Supported     bool   `json:"supported"`
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
	Cgo           bool   `json:"cgo"`
	VZ            bool   `json:"vz"` // the Virtualization-framework engine is linked
	HelperVersion string `json:"helper_version"`
	Host          string `json:"host"`         // "ok" or why this Mac cannot
	Provisioning  string `json:"provisioning"` // "ok" or why first boot cannot
	// Thermal is the Mac's thermal state (nominal, fair, serious, critical;
	// at serious and above no new VM starts), when the engine can read it.
	Thermal string `json:"thermal,omitempty"`
}

// cmdProbe prints whether this Mac and this build can run macOS VMs, and
// exits 0 only when they can.
func cmdProbe(d deps) int {
	r := probeReport{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Cgo: cgoEnabled, VZ: vzLinked, HelperVersion: version.String()}
	var hostErr error
	if eng, err := d.engine(); err != nil {
		hostErr = err
	} else {
		hostErr = eng.HostCheck()
		r.Thermal = eng.ThermalState().String()
	}
	provErr := errors.New("this build has no guest provisioning")
	if d.firstBoot != nil {
		provErr = d.firstBoot.Supported()
	}
	r.Host, r.Provisioning = okOr(hostErr), okOr(provErr)
	r.Supported = hostErr == nil && provErr == nil
	enc := json.NewEncoder(d.stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(r)
	if !r.Supported {
		return 1
	}
	return 0
}

func okOr(err error) string {
	if err != nil {
		return err.Error()
	}
	return "ok"
}
