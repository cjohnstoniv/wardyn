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
	"strings"

	"github.com/spf13/cobra"

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
		id, err := runneridentity.Load(stateDir, cfg.get("org_url"))
		if err != nil {
			return nil, "", err
		}
		root, err := macvm.DefaultRoot()
		if err != nil {
			return nil, "", err
		}
		m := openMacOSVMs(macOSVMSetup{ledgerDir: admission.DefaultDir, root: root, runnerID: id.RunnerID.String(), uid: os.Getuid(),
			host: admission.SystemHost(), owner: fileOwner})
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
	prepare := &cobra.Command{Use: "prepare", Short: "Build this Mac's macOS base image from Apple's restore image (hours; it continues in the background)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cpu < 1 || memGB < 1 {
				return errors.New("pass --per-run-cpu and --per-run-memory: the runner's per-run limits, which prepare's own VM uses and never raises")
			}
			m, runnerID, err := open(cmd)
			if err != nil {
				return err
			}
			err = m.startPrepare(cmd.Context(), prepareArgs{helper: m.helper, runnerID: runnerID, cpus: int(cpu), memoryMiB: uint64(memGB) << 10, keepIPSW: keep, newImage: newImage})
			if errors.Is(err, errMacOSBaseReady) {
				fmt.Fprintln(cmd.OutOrStdout(), "This Mac's macOS base is ready; nothing to prepare. `--new-image` builds a new one from Apple's newest restore image.")
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Preparing this Mac's macOS base in the background; progress is in %s and in `wardyn-runnerd macos-vm status`.\n", m.layout.PrepareLog())
			return nil
		}}
	prepare.Flags().Float64Var(&cpu, "per-run-cpu", 0, "the runner's per-run CPUs")
	prepare.Flags().Int64Var(&memGB, "per-run-memory", 0, "the runner's per-run memory in GB")
	prepare.Flags().BoolVar(&keep, "keep-ipsw", false, "keep Apple's restore image after the base is ready")
	prepare.Flags().BoolVar(&newImage, "new-image", false, "build a new base from Apple's newest restore image even when one is ready, and swap it in")
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
	Error         string   `json:"error"`

	progress, counted, minCPUs, minMemoryMiB int
	setupCode                                string
}

// status reads the ledger, the base, the offer and the helper's probe.
func (m *macOSVMs) status(ctx context.Context) (macOSVMStatus, error) {
	st := macOSVMStatus{State: runnerwire.MacVMBaseNone, Identity: string(macOSVMIdentity), Harnesses: []string{}}
	if m.ledger == nil {
		st.Error = cmp.Or(m.refusal, m.why)
		return st, nil
	}
	pr, perr := probeHelper(ctx, m.helper)
	st.Thermal = pr.Thermal
	m.recompute()
	c := m.current()
	st.State, st.Base, st.Max, st.progress, st.Error = c.Base.State, c.Base.GuestOS, c.Max, c.Base.Progress, c.Base.Error
	if perr != nil {
		st.Error = cmp.Or(st.Error, perr.Error())
	}
	if c.Base.State == runnerwire.MacVMBaseReady {
		st.Desktop, st.Harnesses, st.minCPUs, st.minMemoryMiB = cmp.Or(c.Desktop, "none"), append(st.Harnesses, c.Harnesses...), c.MinCPUs, c.MinMemoryMiB
	}
	var ev macvm.PrepareEvent
	if json.Unmarshal([]byte(lastLine(m.layout.PrepareLog())), &ev) == nil {
		st.Phase = ev.Phase
	}
	if st.setupCode = setupCodeWaiting(m.layout.Root); st.setupCode != "" {
		st.Phase = "setup_by_hand"
	}
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
		fmt.Fprintf(out, "macOS asked for setup by hand. Open the macOS window on this Mac, create the account wardyn-admin with the one-time code in %s as its password, and turn on Remote Login.\n", st.setupCode)
	}
	if st.State == runnerwire.MacVMBaseReady {
		fmt.Fprintf(out, "harnesses: %s; desktop: %s; guest minimum: %d CPUs, %d MiB\n", strings.Join(st.Harnesses, ", "), desktopMode(st.Desktop), st.minCPUs, st.minMemoryMiB)
	}
	if st.Thermal != "" {
		fmt.Fprintf(out, "thermal: %s\n", st.Thermal)
	}
	fmt.Fprintf(out, "macOS VMs on this Mac (every login): %d; this runner offers %d at once\n", st.counted, st.Max)
	if st.Leaked > 0 {
		fmt.Fprintf(out, "VM slots macOS holds until this Mac restarts: %d\n", st.Leaked)
	}
	if st.HeldDiskBytes > 0 {
		fmt.Fprintf(out, "disk held for ended logins' VMs until they log in again: %d GB\n", st.HeldDiskBytes>>30)
	}
}
