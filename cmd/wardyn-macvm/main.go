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
	"github.com/cjohnstoniv/wardyn/internal/version"
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
	// firstBoot is the provisioning first boot (macvm.NewProvisioner); nil
	// until that is linked.
	firstBoot macvm.FirstBooter
	// fstat replaces fstat(2) in the ledger's ownership checks (tests).
	fstat func(fd int, name string) (unix.Stat_t, error)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], deps{
		engine: platformEngine, host: admission.SystemHost(), stdout: os.Stdout, stderr: os.Stderr,
		prepare: prepareDeps,
	}))
}

func run(ctx context.Context, args []string, d deps) int {
	if len(args) == 0 {
		fmt.Fprintln(d.stderr, "usage: wardyn-macvm run|status|prepare|probe|version [flags]")
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
	case "prepare":
		return cmdPrepare(ctx, args[1:], d)
	case "probe":
		return cmdProbe(d)
	}
	fmt.Fprintf(d.stderr, "wardyn-macvm: unknown command %q\n", args[0])
	return 2
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
	err = macvm.Run(ctx, macvm.RunOptions{
		Engine: eng, RunDir: *dir, Log: log,
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
	cc, err := macvm.DialControl(ctx, filepath.Join(*dir, macvm.ControlSock))
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

func cmdPrepare(ctx context.Context, args []string, d deps) int {
	fs := flag.NewFlagSet("prepare", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	root := fs.String("root", "", "the macos-vm.noindex directory (default: the user's)")
	slot := fs.Int("slot", 0, "the base slot to prepare")
	cpus := fs.Int("cpus", 0, "the runner's per-run CPU limit (never raised)")
	memMiB := fs.Uint64("memory-mib", 0, "the runner's per-run memory limit in MiB (never raised)")
	keep := fs.Bool("keep-ipsw", false, "keep the downloaded restore image")
	reprepare := fs.Bool("reprepare", false, "build a new base even when one is ready (a newer restore image), and swap it in")
	runner := fs.String("runner-id", "", "the runner whose admission entry this prepare holds")
	entry := fs.String("ledger-entry", "", "the admission entry runnerd took for this prepare")
	ledgerDir := fs.String("ledger-dir", "", "the admission ledger directory (default: the package's)")
	if fs.Parse(args) != nil || *cpus <= 0 || *memMiB == 0 {
		fmt.Fprintln(d.stderr, "wardyn-macvm prepare: --cpus and --memory-mib are required")
		return 2
	}
	if *root == "" {
		r, err := macvm.DefaultRoot()
		if err != nil {
			fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
			return 1
		}
		*root = r
	}
	eng, err := d.engine()
	if err != nil {
		fmt.Fprintf(d.stderr, "wardyn-macvm: %v\n", err)
		return 1
	}
	enc := json.NewEncoder(d.stdout)
	o := macvm.PrepareOptions{
		Engine: eng, Layout: macvm.Layout{Root: *root}, Slot: *slot, Power: macvm.PmsetPower{},
		CPUs: *cpus, MemoryMiB: *memMiB, KeepIPSW: *keep, Reprepare: *reprepare, HelperVersion: version.String(),
		Progress: func(ev macvm.PrepareEvent) { _ = enc.Encode(ev) },
		// runnerd took the entry; the helper claims it before any VM exists,
		// and runnerd releases it after verified destruction.
		Admit: func(context.Context) (func(), error) {
			if *runner == "" || *entry == "" {
				return nil, errors.New("prepare needs --runner-id and --ledger-entry: runnerd takes the admission entry before it starts prepare")
			}
			err := claim(macvm.RunConfig{RunnerID: *runner, LedgerEntry: *entry, LedgerDir: *ledgerDir}, d)
			return func() {}, err
		},
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

// prepareDeps wires prepare's first-boot provisioner (macvm.NewProvisioner),
// prep network (internal/vmnet) and stage 1. They arrive from their own
// lanes; until they are merged this build refuses prepare by name.
func prepareDeps(o *macvm.PrepareOptions) error {
	if o.FirstBoot == nil || o.PrepNet == nil || o.Bootstrap == nil {
		return errors.New("this build has no guest provisioning, prep network or stage 1 yet; prepare is not available")
	}
	return nil
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
}

// cmdProbe prints whether this Mac and this build can run macOS VMs, and
// exits 0 only when they can.
func cmdProbe(d deps) int {
	r := probeReport{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Cgo: cgoEnabled, VZ: vzLinked, HelperVersion: version.String()}
	hostErr := errors.New("no engine")
	if eng, err := d.engine(); err != nil {
		hostErr = err
	} else {
		hostErr = eng.HostCheck()
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
