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
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A kept machine is one person's macOS guest disk kept on this Mac between runs (internal/macvm/kept.go).
// The runner reports them in its caps and settles them periodically; `wardyn-runnerd macos-vm machines`
// lists, updates and deletes them from this Mac, in its own process, from the kept folder alone.

// keptDriver is what the macOS VM handle takes from the Docker driver for kept machines (the same driver
// as macVMDriver; a driver without them leaves the caps without them).
type keptDriver interface {
	KeptMachines() (*runnerwire.MacVMKept, error)
	SweepKeptMachines() error
	EraseKeptMachines(ctx context.Context) error
}

// macOSVMKeptSweepEvery is how many recompute ticks pass between the kept machines sweeps (the first
// runs at start): 5 minutes at the 15 s tick. A sweep deletes the machines past their expiry, so this is
// how long past it one can stay.
const macOSVMKeptSweepEvery = 20

// macOSVMKept is the handle's part for kept machines. driver is the Docker driver's, nil when it keeps
// none (useDriver). limit is the disk they may take, in bytes (macos_vm_kept_disk_limit_gb; 0 keeps none),
// and limitErr why that setting was refused, which turns kept machines off. eraseAgain says an erase of
// them failed and the periodic sweep retries it (under the handle's mu). sweepEvery is the recompute ticks
// between sweeps (0 is macOSVMKeptSweepEvery; tests set it).
type macOSVMKept struct {
	driver     keptDriver
	limit      int64
	limitErr   error
	eraseAgain bool
	sweepEvery int
}

// openKept takes the kept limit setting as resolved by the caller (it never reads a settings file itself,
// so the file serve was given is the one that counts). One that cannot be read keeps no machines on this
// Mac, never more than the person set.
func (m *macOSVMs) openKept(setting string) {
	if m.kept.limit, m.kept.limitErr = parseKeptDiskLimit(setting); m.kept.limitErr != nil {
		m.kept.limit = 0
		slog.Warn("macOS VM sandboxes: this Mac keeps no machines", "error", m.kept.limitErr.Error())
	}
}

// keptCaps sets the caps' kept_machines (this runner's driver keeps machines) and kept (the Mac's limit and
// its machines), from the driver. A Mac whose limit is 0 reports a limit of 0 and no machines, which says
// it keeps none; a count that fails keeps the last one.
func (m *macOSVMs) keptCaps(c, last *runnerwire.MacVMCaps) {
	if m.kept.driver == nil {
		return
	}
	c.KeptMachines = true
	k, err := m.kept.driver.KeptMachines()
	switch {
	case err != nil:
		slog.Warn("macOS VM kept machines: count", "error", err.Error())
		if last != nil {
			c.Kept = last.Kept
		}
	case k == nil:
		// The driver reports nothing at a limit of 0; the machines already on the Mac are still reported,
		// listed from the folder, so they can be seen, deleted and expired.
		c.Kept = m.keptFromFolder()
	default:
		c.Kept = k
	}
}

// keptFromFolder is the kept caps read from the kept folder: this Mac's limit, every machine's worst case and
// what they take now, and each machine with the run that boots it.
func (m *macOSVMs) keptFromFolder() *runnerwire.MacVMKept {
	out := &runnerwire.MacVMKept{LimitBytes: m.kept.limit}
	rows, err := m.keptRows()
	if err != nil {
		slog.Warn("macOS VM kept machines: read the kept folder", "error", err.Error())
	}
	for _, r := range rows {
		out.WorstBytes += r.worst
		out.AllocatedBytes += r.written
		out.Machines = append(out.Machines, runnerwire.MacVMKeptMachine{ID: r.rec.ID, Bytes: r.written, InUse: r.inUse, RunID: r.run,
			Scrubbed: r.rec.Scrubbed, ExpiresAt: r.rec.ExpiresAt})
	}
	return out
}

// keptSweepTick runs the kept machines sweep on every keptSweepEvery-th recompute tick.
func (m *macOSVMs) keptSweepTick(ctx context.Context) {
	if m.ticks%cmp.Or(m.kept.sweepEvery, macOSVMKeptSweepEvery) == 0 {
		m.sweepKept(ctx)
	}
}

// sweepKept settles this Mac's kept machines (the driver's SweepKeptMachines: expired ones go, an
// interrupted session ends, another registration's machines go), and first finishes an erase that failed.
// It runs at start, offline too, and on the sweep's ticks. A nil handle or a driver without kept machines
// does nothing.
func (m *macOSVMs) sweepKept(ctx context.Context) {
	if m == nil || m.kept.driver == nil {
		return
	}
	m.mu.Lock()
	again := m.kept.eraseAgain
	m.mu.Unlock()
	if again {
		if err := m.eraseKept(ctx); err != nil {
			slog.Warn("macOS VM kept machines: the erase will retry on the next sweep", "error", err.Error())
		}
		return
	}
	if err := m.kept.driver.SweepKeptMachines(); err != nil {
		slog.Warn("macOS VM kept machines: sweep", "error", err.Error())
	}
}

// eraseKept deletes every kept machine on this Mac, for a runner the organisation revoked (when it learns
// of it, and again when it starts revoked). A failure is remembered for the sweep to retry. A runner
// without the driver (macOS VMs off, a nil handle) deletes the kept folder directly.
func (m *macOSVMs) eraseKept(ctx context.Context) error {
	if m == nil || m.kept.driver == nil {
		// macOS VMs are off, or the driver keeps none: a machine folder from an earlier run of them is still
		// on this Mac, and the revoked runner's erase does not need the driver to delete it.
		return m.eraseKeptFolder()
	}
	err := m.kept.driver.EraseKeptMachines(ctx)
	m.mu.Lock()
	m.kept.eraseAgain = err != nil
	m.mu.Unlock()
	return err
}

// eraseKeptFolder deletes every kept machine folder under this Mac's VM directory, and their locks, update
// staging and reservations, without the driver: a machine a boot or an update holds is waited for briefly
// (keptLockWait) and then reported as not erased, with the rest erased. A nil handle uses the default VM
// directory; a Mac with none, or a host that is not a Mac, has nothing to erase.
func (m *macOSVMs) eraseKeptFolder() error {
	root := ""
	if m != nil {
		root = m.layout.Root
	}
	if root == "" {
		var err error
		if root, err = macvm.DefaultRoot(); err != nil {
			return nil // no home, so no VM directory in it
		}
	}
	kept := macvm.Layout{Root: root}.KeptDir()
	des, err := os.ReadDir(kept)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("read the kept machines: %w", err)
	}
	var errs []error
	held := map[string]bool{}
	for _, de := range des {
		if !de.IsDir() || !isKeptKey(de.Name()) {
			continue
		}
		dir := filepath.Join(kept, de.Name())
		unlock, err := macvm.LockKeptWait(dir, keptLockWait)
		if err == nil {
			unlock()
			_, err = macvm.RemoveKept(dir)
		}
		if err != nil {
			held[de.Name()] = true
			errs = append(errs, fmt.Errorf("kept machine %s was not erased: %w", de.Name(), err))
		}
	}
	// What is left beside the machines: a staging folder, a reservation, a lock of a machine already gone.
	for _, de := range des {
		key, _, _ := strings.Cut(de.Name(), ".")
		if held[de.Name()] || held[key] || isKeptKey(de.Name()) && de.IsDir() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(kept, de.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		slog.Info("macOS VM kept machines: erased this Mac's kept machines")
	}
	return errors.Join(errs...)
}

// keptRow is one kept machine on this Mac, read from its folder.
type keptRow struct {
	key  string // the folder's name: the first 12 hex digits of the id
	name string // the name its kept.json holds, "" when it holds none
	dir  string
	rec  macvm.KeptRecord
	// written is what the machine's files take now, worst what they could grow to (its disk's size plus a
	// run's reserve), and inUse says a live VM of this runner boots it, or its record still names a session
	// the sweep has not recovered. That is read from the run folders, the ledger and the record, never by
	// probing the machine's lock: a probe takes the lock the helper's boot and
	// the runner's stop write use, so it can refuse a boot or drop a write.
	written, worst int64
	inUse          bool
	run            string // the run whose VM boots the machine, else the session its record names; "" when none
}

// keptRunsByMachine maps each kept machine a live VM of this runner boots to that VM's run id, from the run
// folders and the ledger's entries (the driver's KeptMachines reads the same). It takes no machine's lock.
func (m *macOSVMs) keptRunsByMachine() map[string]string {
	out := map[string]string{}
	if m.ledger == nil {
		return out
	}
	counted, err := m.ledger.Counted()
	if err != nil {
		return out
	}
	live := map[string]bool{}
	for _, e := range counted {
		if e.RunnerID == m.runnerID && !m.vmGone(e) {
			live[e.ID] = true
		}
	}
	des, _ := os.ReadDir(m.layout.RunsDir())
	for _, de := range des {
		if cfg, err := macvm.ReadRunConfig(filepath.Join(m.layout.RunsDir(), de.Name())); err == nil && cfg.Kept != nil && live[cfg.LedgerEntry] {
			out[cfg.Kept.ID] = cfg.RunID
		}
	}
	return out
}

// label is how a machine is named to the person: its name, else its key.
func (r keptRow) label() string { return cmp.Or(r.name, r.key) }

// keptRows reads this Mac's kept machines by name then key. A folder with no readable record that names it
// is not a machine and is left out.
func (m *macOSVMs) keptRows() ([]keptRow, error) {
	des, err := os.ReadDir(m.layout.KeptDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var rows []keptRow
	runs := m.keptRunsByMachine()
	for _, de := range des {
		if !de.IsDir() || !isKeptKey(de.Name()) {
			continue
		}
		dir := filepath.Join(m.layout.KeptDir(), de.Name())
		var probe macvm.KeptRecord
		b, err := os.ReadFile(filepath.Join(dir, macvm.KeptFile))
		if err != nil || json.Unmarshal(b, &probe) != nil {
			continue
		}
		rec, err := macvm.ReadKept(dir, probe.ID)
		if err != nil || m.layout.KeptMachineDir(rec.ID) != dir {
			continue
		}
		man, _ := macvm.ReadManifest(dir)
		written, _ := macvm.KeptAllocatedBytes(dir)
		// A record that still names a session is in use until the sweep's recovery clears it and writes the
		// new deadline, even once its run folder is gone; its expiry is the boot's until then.
		run := cmp.Or(runs[rec.ID], rec.ActiveRun)
		rows = append(rows, keptRow{key: de.Name(), name: rec.Name, dir: dir, rec: rec, written: written, inUse: run != "", run: run,
			worst: admission.RunDiskReservation(cmp.Or(man.DiskBytes, macvm.BaseDiskBytes))})
	}
	slices.SortFunc(rows, func(a, b keptRow) int {
		return cmp.Or(strings.Compare(a.label(), b.label()), strings.Compare(a.key, b.key))
	})
	return rows, nil
}

// isKeptKey says name is a kept machine's folder: a key of macvm.RunKeyLen lowercase hex digits. The kept
// folder also holds each machine's lock and other files that are not machines.
func isKeptKey(name string) bool {
	return len(name) == macvm.RunKeyLen && strings.Trim(name, "0123456789abcdef") == ""
}

// macOSVMKeptStatus is `macos-vm status`'s kept machines: how many, the most they could take, the limit,
// and what they take now.
type macOSVMKeptStatus struct {
	Machines       int   `json:"machines"`
	WorstBytes     int64 `json:"worst_bytes"`
	LimitBytes     int64 `json:"limit_bytes"`
	AllocatedBytes int64 `json:"allocated_bytes"`
}

// keptStatus reads this Mac's kept machines for the status.
func (m *macOSVMs) keptStatus() (*macOSVMKeptStatus, error) {
	rows, err := m.keptRows()
	st := &macOSVMKeptStatus{Machines: len(rows), LimitBytes: m.kept.limit}
	for _, r := range rows {
		st.WorstBytes += r.worst
		st.AllocatedBytes += r.written
	}
	return st, err
}

// gbUp is n bytes in whole GB, rounded up: the most the machines could take is never understated.
func gbUp(n int64) int64 { return (n + 1<<30 - 1) >> 30 }

// gbNear is n bytes in whole GB, to the nearest: what the machines have written.
func gbNear(n int64) int64 { return (n + 1<<29) >> 30 }

// writeText prints the kept machines line of the status: "kept machines: 1, up to 51 GB of 110 GB (12 GB
// written)". A limit of 0 says the Mac keeps none, whatever it still holds.
func (k macOSVMKeptStatus) writeText(out io.Writer) {
	if k.LimitBytes <= 0 {
		fmt.Fprintf(out, "kept machines: off on this Mac (%s is 0); %d held, up to %d GB (%d GB written)\n",
			macOSVMKeptDiskLimitKey, k.Machines, gbUp(k.WorstBytes), gbNear(k.AllocatedBytes))
		return
	}
	fmt.Fprintf(out, "kept machines: %d, up to %d GB of %d GB (%d GB written)\n", k.Machines, gbUp(k.WorstBytes), k.LimitBytes>>30, gbNear(k.AllocatedBytes))
}

// resolveKept is the one machine arg names: its key; else its name, which two machines may share (another
// owner's, or a stale one), and then the person must use the key; else its id, or the start of its id (at
// least 4 digits, dashes left out) that no other machine's id starts with.
func resolveKept(rows []keptRow, arg string) (keptRow, error) {
	if i := slices.IndexFunc(rows, func(r keptRow) bool { return r.key == arg }); i >= 0 {
		return rows[i], nil
	}
	var named []keptRow
	for _, r := range rows {
		if r.name != "" && r.name == arg {
			named = append(named, r)
		}
	}
	switch len(named) {
	case 1:
		return named[0], nil
	case 0:
	default:
		keys := make([]string, len(named))
		for i, r := range named {
			keys[i] = r.key
		}
		//lint:ignore ST1005 the sentence the person reads at the terminal, as written
		return keptRow{}, fmt.Errorf("More than one kept machine is named %s (%s). Use the key.", arg, strings.Join(keys, ", "))
	}
	want := strings.ReplaceAll(strings.ToLower(arg), "-", "")
	var found []keptRow
	if len(want) >= 4 {
		for _, r := range rows {
			if strings.HasPrefix(strings.ReplaceAll(r.rec.ID, "-", ""), want) {
				found = append(found, r)
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return keptRow{}, fmt.Errorf("this Mac has no kept machine %q (`wardyn-runnerd macos-vm machines` lists them)", arg)
	}
	return keptRow{}, fmt.Errorf("%q matches %d kept machines; use more of the id", arg, len(found))
}

// stamp is a time as the list shows it, or "-" when it is unset.
func stamp(t time.Time, layout string) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format(layout)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// writeKeptList prints the machines, one per row.
func writeKeptList(out io.Writer, rows []keptRow) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "no kept machines")
		return
	}
	w := tabwriter.NewWriter(out, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tID\tBASE\tLAST STOP\tSTOP\tSCRUBBED\tEXPIRES\tWRITTEN\tIN USE")
	for _, r := range rows {
		scrubbed := "-"
		if !r.inUse {
			scrubbed = yesNo(r.rec.Scrubbed)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d GB\t%s\n", r.label(), r.key, cmp.Or(r.rec.Base, "-"),
			stamp(r.rec.LastStop, "2006-01-02 15:04"), cmp.Or(r.rec.StopKind, "-"), scrubbed, stamp(r.rec.ExpiresAt, time.DateOnly), gbNear(r.written), yesNo(r.inUse))
	}
	w.Flush()
}

// errKeptRunning is a machine a boot holds, in the person's words.
func errKeptRunning(r keptRow) error {
	return fmt.Errorf("machine %s is running on this Mac; try again when its run has stopped", r.label())
}

// keptLockWait is how long a person's delete or update waits for a machine's lock before it says the
// machine is running: a brief holder (a stop write, a probe) is not a boot (tests shorten it).
var keptLockWait = 2 * time.Second

// waitKeptFree waits up to keptLockWait for the machine's lock to be free, and holds nothing after: the
// action that follows takes the lock itself. ErrKeptLocked means a boot or another update holds it.
func waitKeptFree(r keptRow) error {
	unlock, err := macvm.LockKeptWait(r.dir, keptLockWait)
	if errors.Is(err, macvm.ErrKeptLocked) {
		return errKeptRunning(r)
	} else if err != nil {
		return err
	}
	unlock()
	return nil
}

// deleteKept deletes the machine on this Mac. It waits briefly for a holder of the machine's lock, then
// macvm.RemoveKept takes the lock (macvm.LockKept) and holds it throughout, so a machine a boot or an
// update holds is refused and no delete runs beside a boot.
func deleteKept(out io.Writer, r keptRow) error {
	if err := waitKeptFree(r); err != nil {
		return err
	}
	gone, err := macvm.RemoveKept(r.dir)
	switch {
	case errors.Is(err, macvm.ErrKeptLocked):
		return errKeptRunning(r)
	case err != nil:
		return fmt.Errorf("delete machine %s: %w", r.label(), err)
	case !gone:
		fmt.Fprintf(out, "machine %s was already gone\n", r.label())
		return nil
	}
	fmt.Fprintf(out, "deleted kept machine %s (%s); the organisation's record goes on the next report\n", r.label(), r.key)
	return nil
}

// updateKept brings the machine's tools up to this Mac's. Without force it does nothing for a machine that
// is current; with force it updates anyway and retries every withheld harness.
func updateKept(ctx context.Context, out io.Writer, u *keptToolsUpdater, r keptRow, force bool) error {
	if !force {
		behind, err := u.behind(r.dir)
		if err != nil {
			return fmt.Errorf("machine %s: %w", r.label(), err)
		}
		if !behind {
			fmt.Fprintf(out, "machine %s: tools are current\n", r.label())
			return nil
		}
	}
	fmt.Fprintln(out, "Updating this machine's tools…")
	update := u.UpdateKeptTools
	if force {
		update = u.ForceKeptTools
	}
	switch err := update(ctx, r.dir); {
	case errors.Is(err, macvm.ErrKeptLocked):
		return errKeptRunning(r)
	case err != nil:
		return fmt.Errorf("machine %s: %w", r.label(), err)
	}
	fmt.Fprintf(out, "machine %s: tools updated\n", r.label())
	return nil
}

// newMacOSVMMachinesCommand is `wardyn-runnerd macos-vm machines`: the kept machines on this Mac, and
// `update` and `delete` for one of them by name or id.
func newMacOSVMMachinesCommand(open func(*cobra.Command) (*macOSVMs, string, error)) *cobra.Command {
	machine := func(cmd *cobra.Command, arg string) (*macOSVMs, keptRow, error) {
		m, _, err := open(cmd)
		if err != nil {
			return nil, keptRow{}, err
		}
		rows, err := m.keptRows()
		if err != nil {
			return nil, keptRow{}, err
		}
		r, err := resolveKept(rows, arg)
		return m, r, err
	}
	list := &cobra.Command{Use: "machines", Short: "List, update or delete the machines this Mac keeps", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		m, _, err := open(cmd)
		if err != nil {
			return err
		}
		rows, err := m.keptRows()
		if err != nil {
			return err
		}
		writeKeptList(cmd.OutOrStdout(), rows)
		return nil
	}}
	var force bool
	update := &cobra.Command{Use: "update <machine>", Short: "Bring a kept machine's tools up to this Mac's", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		m, r, err := machine(cmd, args[0])
		if err != nil {
			return err
		}
		return updateKept(cmd.Context(), cmd.OutOrStdout(), newKeptToolsUpdater(m), r, force)
	}}
	update.Flags().BoolVar(&force, "force", false, "update even when the machine's tools look current, and retry every harness withheld from it")
	del := &cobra.Command{Use: "delete <machine>", Short: "Delete a kept machine from this Mac, with the files, tools and sign-ins on it", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		_, r, err := machine(cmd, args[0])
		if err != nil {
			return err
		}
		return deleteKept(cmd.OutOrStdout(), r)
	}}
	list.AddCommand(update, del)
	return list
}
