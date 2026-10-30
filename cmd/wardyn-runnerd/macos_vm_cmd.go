// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// newMacOSVMCommand is `wardyn-runnerd macos-vm`: this Mac's macOS VM state, its base's preparation, and
// the base's removal.
func newMacOSVMCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "macos-vm", Short: "Show, prepare or remove this Mac's macOS VM base", Args: cobra.NoArgs}
	open := func(cmd *cobra.Command) (*macOSVMs, string, error) {
		cfg, err := serveConfig(cmd, "")
		if err != nil {
			return nil, "", err
		}
		stateDir := cfg.get("state_dir")
		if stateDir == "" {
			if stateDir, err = runneridentity.DefaultDir(); err != nil {
				return nil, "", err
			}
		}
		org, err := macVMOrg(cfg, serviceOrgArg)
		if err != nil {
			return nil, "", err
		}
		id, err := runneridentity.Load(stateDir, org)
		if err != nil {
			return nil, "", err
		}
		root, err := macvm.DefaultRoot()
		if err != nil {
			return nil, "", err
		}
		m := openMacOSVMs(macOSVMSetup{ledgerDir: admission.DefaultDir, root: root, runnerID: id.RunnerID.String(), uid: os.Getuid(),
			host: admission.SystemHost(), owner: fileOwner, limitsFile: macOSVMLimitsFile(stateDir)})
		if m.helper, err = macVMHelperPath(); err != nil {
			return nil, "", err
		}
		if !macOSVMOn(cfg.get("macos_vm")) {
			fmt.Fprintln(cmd.ErrOrStderr(), "macOS VM sandboxes are off on this runner (setting macos_vm, from "+cfg.field("macos_vm").Source+").")
		}
		return m, id.RunnerID.String(), nil
	}
	var asJSON bool
	status := &cobra.Command{Use: "status", Short: "Show this Mac's macOS VM base, ledger and offer", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		m, _, err := open(cmd)
		if err != nil {
			return err
		}
		st, err := m.status(cmd.Context())
		if err != nil {
			return err
		}
		if asJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(st)
		}
		st.writeText(cmd.OutOrStdout())
		return nil
	}}
	status.Flags().BoolVar(&asJSON, "json", false, "print one JSON object")
	var cpu float64
	var memGB int64
	var keep, newImage bool
	var ipsw, sha256 string
	prepare := &cobra.Command{Use: "prepare", Short: "Build this Mac's macOS base image from Apple's restore image (hours; it continues in the background)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, runnerID, err := open(cmd)
			if err != nil {
				return err
			}
			cpus, memMiB, err := prepareLimits(m.limitsFile, cpu, memGB)
			if err != nil {
				return err
			}
			err = m.startPrepare(cmd.Context(), prepareArgs{helper: m.helper, runnerID: runnerID, cpus: cpus, memoryMiB: memMiB, keepIPSW: keep, ipsw: ipsw, sha256: sha256, newImage: newImage})
			if errors.Is(err, errMacOSBaseReady) {
				if ipsw != "" {
					fmt.Fprintln(cmd.OutOrStdout(), "This Mac's macOS base is ready; nothing to prepare, and the --ipsw file was not used. `--new-image` builds a new one, from that file.")
					return nil
				}
				fmt.Fprintln(cmd.OutOrStdout(), "This Mac's macOS base is ready; nothing to prepare. `--new-image` builds a new one from Apple's newest restore image.")
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Preparing this Mac's macOS base in the background; progress is in %s and in `wardyn-runnerd macos-vm status`.\n", m.layout.PrepareLog())
			return nil
		}}
	prepare.Flags().Float64Var(&cpu, "per-run-cpu", 0, "the runner's per-run CPUs (default: the limit its organisation last sent)")
	prepare.Flags().Int64Var(&memGB, "per-run-memory", 0, "the runner's per-run memory in GB (default: the limit its organisation last sent)")
	prepare.Flags().BoolVar(&keep, "keep-ipsw", false, "keep Apple's restore image after the base is ready")
	prepare.Flags().StringVar(&ipsw, "ipsw", "", "build from this macOS restore image (.ipsw) you downloaded instead of downloading Apple's; needs --sha256. With a ready base it does nothing (the file is not used) unless --new-image is given, which builds the new base from this file")
	prepare.Flags().StringVar(&sha256, "sha256", "", "the SHA-256 (64 hex digits) the --ipsw file must have, from a source you trust; prepare checks a copy in its cache and refuses a file that differs, before anything uses it")
	prepare.Flags().BoolVar(&newImage, "new-image", false, "build a new base from Apple's newest restore image (or the --ipsw file) even when one is ready, and swap it in")
	remove := &cobra.Command{Use: "remove", Short: "Delete this Mac's macOS base images and download cache", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		m, _, err := open(cmd)
		if err != nil {
			return err
		}
		return m.remove(cmd.OutOrStdout())
	}}
	cmd.AddCommand(status, prepare, remove)
	return cmd
}

// macVMOrg is the organisation address status and prepare load the runner's identity for, ranked as serve
// ranks it: a locked managed OrgURL, then the --org an earlier install's service definition passes serve
// (legacy; serve takes that argument as a flag, and doctor reads it the same way), then the rest of serve's
// own resolution (cfg). With none, the error says where runnerd.json should be and how to write it, instead of
// the address check's bare refusal.
func macVMOrg(cfg runnerConfig, legacy func() string) (string, error) {
	f := cfg.field("org_url")
	if f.Value != "" && strings.HasSuffix(f.Source, "(locked)") {
		return f.Value, nil
	}
	if org := legacy(); org != "" {
		return org, nil
	}
	if f.Value != "" {
		return f.Value, nil
	}
	path, err := defaultRunnerConfigPath()
	if err != nil {
		return "", err
	}
	fix := "run `wardyn-runnerd setup`, write org_url into that file, or set OrgURL in the managed profile"
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no runnerd.json was found at %s, so this runner's organisation URL is unknown: %s", path, fix)
	}
	return "", fmt.Errorf("%s sets no org_url, so this runner's organisation URL is unknown: %s", path, fix)
}

// serviceOrgArg is the --org the installed service definition passes serve ("": none, or no setup host).
func serviceOrgArg() string {
	h, err := osSetupHost()
	if err != nil {
		return ""
	}
	p, err := h.paths()
	if err != nil {
		return ""
	}
	org, _ := p.serviceOrgArg()
	return org
}

// macOSVMStatus is `macos-vm status`: its JSON (--json) and, with the unexported fields, its text. The
// one-time setup code is never in it: only the path of the file that holds it.
type macOSVMStatus struct {
	State         string   `json:"state"` // the base's state
	Phase         string   `json:"phase"` // prepare's last phase, "setup_by_hand" while macOS waits for it
	Base          string   `json:"base"`  // the base's macOS
	Desktop       string   `json:"desktop"`
	Harnesses     []string `json:"harnesses"`
	Thermal       string   `json:"thermal"` // the helper's probe's, "" when it reports none
	Max           int      `json:"max"`
	Identity      string   `json:"identity"`
	HeldDiskBytes int64    `json:"held_disk_bytes"` // disk held for ended logins' VMs
	Leaked        int      `json:"leaked"`          // framework slots held until a restart
	ToolsDial     string   `json:"tools_dial"`      // how the last run boot's tools-port dial ended, "" before one
	Error         string   `json:"error"`
	Failure       string   `json:"failure,omitempty"` // runnerwire.MacVMFailure*, when the base has one

	progress, counted, minCPUs, minMemoryMiB int
	setupCode                                string
	withheld                                 []string // selftestWithheld's lines, for a ready base
	// byHand: the helper's probe says this Mac has no guest provisioning, so its prepare needs setup by hand.
	byHand bool
}

// status reads the ledger, the base, the offer and the helper's probe.
func (m *macOSVMs) status(ctx context.Context) (macOSVMStatus, error) {
	st := macOSVMStatus{State: runnerwire.MacVMBaseNone, Identity: string(macOSVMIdentity), Harnesses: []string{}}
	if m.ledger == nil {
		st.Error = cmp.Or(m.refusal, m.why)
		return st, nil
	}
	pr, perr := probeHelper(ctx, m.helper)
	st.Thermal, st.byHand = pr.Thermal, perr == nil && pr.Reason == runnerwire.MacVMHostNoProvisioning
	m.recompute()
	c := m.current()
	st.State, st.Base, st.Max, st.progress, st.Error = c.Base.State, c.Base.GuestOS, c.Max, c.Base.Progress, c.Base.Error
	st.Failure = c.Base.Failure
	if perr != nil {
		st.Error = cmp.Or(st.Error, perr.Error())
	}
	if c.Base.State == runnerwire.MacVMBaseReady {
		st.Desktop, st.Harnesses, st.minCPUs, st.minMemoryMiB = cmp.Or(c.Desktop, "none"), append(st.Harnesses, c.Harnesses...), c.MinCPUs, c.MinMemoryMiB
		st.withheld = selftestWithheld(m.layout.BaseDir(0))
	}
	if ev, ok, _, _ := lastHelperLines(m.layout.PrepareLog()); ok {
		st.Phase = ev.Phase
	}
	if st.setupCode = setupCodeWaiting(m.layout.Root); st.setupCode != "" {
		st.Phase = "setup_by_hand"
	}
	st.ToolsDial = lastToolsDial(m.layout.Root)
	counted, err := m.ledger.Counted()
	if err != nil {
		return st, err
	}
	debts, err := m.ledger.Debts()
	if err != nil {
		return st, err
	}
	st.counted = len(counted)
	for _, e := range counted {
		if e.Kind == admission.KindLeaked {
			st.Leaked++
		}
	}
	for _, e := range debts {
		st.HeldDiskBytes += e.DiskReserve
	}
	return st, nil
}

// writeText prints the status for a person.
func (st macOSVMStatus) writeText(out io.Writer) {
	fmt.Fprintf(out, "base: %s", st.State)
	if st.Base != "" {
		fmt.Fprintf(out, ", %s", st.Base)
	}
	if st.State == runnerwire.MacVMBasePreparing {
		fmt.Fprintf(out, ", %d%%", st.progress)
	}
	if st.Error != "" {
		fmt.Fprintf(out, ": %s", st.Error)
	}
	fmt.Fprintln(out)
	if st.setupCode != "" {
		fmt.Fprintf(out, "macOS asked for setup by hand. In the window %q on this Mac, create the account wardyn-admin with the one-time code in %s as its password, and turn on Remote Login.\n", macOSVMSetupWindow, st.setupCode)
	} else if st.byHand && st.State != runnerwire.MacVMBaseReady {
		fmt.Fprintf(out, "This Mac's macOS has no guest provisioning (macOS %d has it), so preparing its base needs setup by hand once: after the install, the window %q opens on this Mac.\n", macvm.ProvisionHostMajor, macOSVMSetupWindow)
	}
	if st.State == runnerwire.MacVMBaseReady {
		fmt.Fprintf(out, "harnesses: %s; desktop: %s; guest minimum: %d CPUs, %d MiB\n", strings.Join(st.Harnesses, ", "), desktopMode(st.Desktop), st.minCPUs, st.minMemoryMiB)
		for _, l := range st.withheld {
			fmt.Fprintln(out, l)
		}
	}
	if st.Thermal != "" {
		fmt.Fprintf(out, "thermal: %s\n", st.Thermal)
	}
	if st.ToolsDial != "" {
		fmt.Fprintf(out, "tools_dial: %s\n", st.ToolsDial)
	}
	fmt.Fprintf(out, "macOS VMs on this Mac (every login): %d; this runner offers %d at once\n", st.counted, st.Max)
	if st.Leaked > 0 {
		fmt.Fprintf(out, "VM slots macOS holds until this Mac restarts: %d\n", st.Leaked)
	}
	if st.HeldDiskBytes > 0 {
		fmt.Fprintf(out, "disk held for ended logins' VMs until they log in again: %d GB\n", st.HeldDiskBytes>>30)
	}
}

// macOSVMSetupWindow is the short name of the setup-by-hand window the helper opens (internal/macvm wardynScreenShow):
// the window's title starts with it and goes on to name the account to create and its password.
const macOSVMSetupWindow = "Wardyn: set up this macOS VM"

// runToolsDialFile, in the VM directory, is the driver's record of how the last run boot's guest agent
// dial of the tools port ended (docker.MacVMConfig.ToolsDialFile).
const runToolsDialFile = "run-tools-dial"

// lastToolsDial is that record when it names one of the five classes, else "".
func lastToolsDial(root string) string {
	b, err := os.ReadFile(filepath.Join(root, runToolsDialFile))
	if s := strings.TrimSpace(string(b)); err == nil && macvm.ValidToolsDial(s) {
		return s
	}
	return ""
}

// macOSVMDoctorStages is doctor's macOS VM stage: only while macOS waits for setup by hand, it names the
// file that holds the one-time code, never the code.
func macOSVMDoctorStages() []doctor.Stage {
	root, err := macvm.DefaultRoot()
	if err != nil {
		return nil
	}
	if p := setupCodeWaiting(root); p != "" {
		return []doctor.Stage{doctor.SkipStage("macos-vm", "macOS asked for setup by hand in the window \""+macOSVMSetupWindow+"\"; the one-time code is in "+p)}
	}
	return nil
}
