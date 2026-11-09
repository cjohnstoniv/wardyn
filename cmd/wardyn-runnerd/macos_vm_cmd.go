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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// newMacOSVMCommand is `wardyn-runnerd macos-vm`: this Mac's macOS VM state, its base's preparation, and
// the base's removal.
func newMacOSVMCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "macos-vm", Short: "Show, prepare, update or remove this Mac's macOS VM base", Args: cobra.NoArgs}
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
		m := openMacOSVMs(macOSVMSetup{ledgerDir: admission.DefaultDir, root: root, runnerID: id.RunnerID.String(), version: version.String(), uid: os.Getuid(),
			host: admission.SystemHost(), owner: fileOwner, limitsFile: macOSVMLimitsFile(stateDir), settings: cfg.macOSVMSettings()})
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
	var ipsw, ipswURL, sha256 string
	prepare := &cobra.Command{Use: "prepare", Short: "Build this Mac's macOS base image from Apple's restore image (hours; it continues in the background)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkServingHelper(); err != nil {
				return err
			}
			m, runnerID, err := open(cmd)
			if err != nil {
				return err
			}
			cpus, memMiB, err := prepareLimits(m.limitsFile, cpu, memGB)
			if err != nil {
				return err
			}
			imgURL, imgSHA, err := prepareMirror(cmd, ipsw, ipswURL, sha256)
			if err != nil {
				return err
			}
			err = m.startPrepare(cmd.Context(), prepareArgs{helper: m.helper, runnerID: runnerID, cpus: cpus, memoryMiB: memMiB, keepIPSW: keep, ipsw: ipsw, ipswURL: imgURL, sha256: imgSHA, newImage: newImage})
			if errors.Is(err, errMacOSBaseReady) {
				if imgURL != "" {
					fmt.Fprintln(cmd.OutOrStdout(), "This Mac's macOS base is ready; nothing to prepare, and the mirror was not used. `--new-image` builds a new one, from the mirror's image.")
					return nil
				}
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
	prepare.Flags().StringVar(&sha256, "sha256", "", "the SHA-256 (64 hex digits) the --ipsw file or the --ipsw-url image must have, from a source you trust; prepare checks a copy in its cache and refuses an image that differs, before anything uses it")
	prepare.Flags().StringVar(&ipswURL, "ipsw-url", "", "download the restore image from this HTTPS mirror instead of Apple's; it must answer HTTP Range requests, and needs --sha256. Apple's image is never the fallback (default: the MacOSVMImageURL managed preference or macos_vm_image_url in runnerd.json)")
	prepare.Flags().BoolVar(&newImage, "new-image", false, "build a new base from Apple's newest restore image (or the --ipsw file) even when one is ready, and swap it in")
	remove := &cobra.Command{Use: "remove", Short: "Delete this Mac's macOS base images and download cache", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		m, _, err := open(cmd)
		if err != nil {
			return err
		}
		return m.remove(cmd.OutOrStdout())
	}}
	editBase := &cobra.Command{Use: "edit-base", Short: "Boot this Mac's macOS base once to edit it, and check it before runs use it", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		m, runnerID, err := open(cmd)
		if err != nil {
			return err
		}
		return m.editBase(cmd.Context(), cmd.OutOrStdout(), runnerID)
	}}
	updateTools := &cobra.Command{Use: "update-tools", Short: "Start a tools update of this Mac's macOS base now (it continues in the background)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if err := checkServingHelper(); err != nil {
			return err
		}
		m, _, err := open(cmd)
		if err != nil {
			return err
		}
		if err := m.updateTools(cmd.Context()); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Updating this Mac's macOS base tools in the background; progress is in %s and in `wardyn-runnerd macos-vm status`.\n", filepath.Join(m.layout.Root, toolsUpdateLog))
		return nil
	}}
	cmd.AddCommand(status, prepare, updateTools, editBase, remove, newMacOSVMMachinesCommand(open))
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

// checkServingHelper refuses a prepare the runner this person's setup LaunchAgent runs could not finish: that
// runner hands the prepare's stage 2 its network, and offers no macOS VM without the helper beside its own
// binary (enableMacOSVM, no_helper). It looks where that serve looks, before anything is written or downloaded.
// No setup agent is not refused: the package's managed agent then runs the package's runnerd, beside its helper
// (runnerd-managed-start stays idle while a setup agent is installed).
func checkServingHelper() error {
	h, err := osSetupHost()
	if err != nil {
		return nil
	}
	p, err := h.paths()
	if err != nil {
		return nil
	}
	if bin := p.serviceProgram(); bin != "" {
		return noHelperBeside(bin)
	}
	return nil
}

// macOSVMStatus is `macos-vm status`: its JSON (--json) and, with the unexported fields, its text. The
// one-time setup code is never in it: only the path of the file that holds it.
type macOSVMStatus struct {
	State         string   `json:"state"`                 // the base's state
	Phase         string   `json:"phase"`                 // prepare's last phase, "setup_by_hand" while macOS waits for it
	Base          string   `json:"base"`                  // the base's macOS
	PreparedAt    string   `json:"prepared_at,omitempty"` // when the base was prepared, RFC 3339; a ready base only
	HostOS        string   `json:"host_os,omitempty"`     // this Mac's macOS, beside the base's
	Desktop       string   `json:"desktop"`
	Harnesses     []string `json:"harnesses"`
	Thermal       string   `json:"thermal"` // the helper's probe's, "" when it reports none
	Max           int      `json:"max"`
	Identity      string   `json:"identity"`
	HeldDiskBytes int64    `json:"held_disk_bytes"` // disk held for ended logins' VMs
	Leaked        int      `json:"leaked"`          // framework slots held until a restart
	ToolsDial     string   `json:"tools_dial"`      // how the newest tools-port dial ended, "" before one
	// ToolsDialRead says where ToolsDial was read from and when, and any log that could not be read to its end.
	ToolsDialRead string `json:"tools_dial_read,omitempty"`
	Error         string `json:"error"`
	Failure       string `json:"failure,omitempty"` // runnerwire.MacVMFailure*, when the base has one
	// Bases are the Mac's other bases (macos_vm_bases_dir), with a failed one's reason.
	Bases []runnerwire.MacVMDerivedBase `json:"bases,omitempty"`
	// Kept is the kept machines this Mac holds and the disk they may take (`macos-vm machines` lists them).
	Kept *macOSVMKeptStatus `json:"kept,omitempty"`

	progress, counted, minCPUs, minMemoryMiB int
	setupCode                                string
	withheld                                 []string // selftestWithheld's lines, for a ready base
	editRead                                 []string // the last edit's recheck read (editReadLines), for a ready base that has one
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
	st.Failure, st.Bases = c.Base.Failure, c.Bases
	if perr != nil {
		st.Error = cmp.Or(st.Error, perr.Error())
	}
	if c.Base.State == runnerwire.MacVMBaseReady {
		st.PreparedAt, st.HostOS = c.Base.CreatedAt, macProductVersion()
		st.Desktop, st.Harnesses, st.minCPUs, st.minMemoryMiB = cmp.Or(c.Desktop, "none"), append(st.Harnesses, c.Harnesses...), c.MinCPUs, c.MinMemoryMiB
		st.withheld = selftestWithheld(m.layout.BaseDir(0))
		st.editRead = m.editReadLines()
	}
	if ev, ok, _, _ := lastHelperLines(m.layout.PrepareLog()); ok {
		st.Phase = ev.Phase
	}
	// During an edit the code file is the edit's, which the edit's own Terminal names.
	if p := setupCodeWaiting(m.layout.Root); p != "" && !editsCode(m.layout.Root) {
		st.setupCode, st.Phase = p, "setup_by_hand"
	}
	st.ToolsDial, st.ToolsDialRead = lastToolsDial(m.layout.Root)
	counted, err := m.ledger.Counted()
	if err != nil {
		return st, err
	}
	debts, err := m.ledger.Debts()
	if err != nil {
		return st, err
	}
	st.counted = len(counted)
	if st.Kept, err = m.keptStatus(); err != nil {
		return st, err
	}
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

// editReadLines are the status lines for what the last passing edit's recheck read in its boot: each
// certificate and Full Disk Access, with the time that boot read them. A read that failed says it could
// not be read, never trusted or allowed. None when the base has no edit record, or one that predates the
// read.
func (m *macOSVMs) editReadLines() []string {
	man, err := macvm.ReadManifest(m.layout.BaseDir(0))
	if err != nil {
		return nil
	}
	rec, ok := macvm.ReadEditRecord(m.layout.BaseDir(0), man)
	if !ok || rec.ReadAt.IsZero() {
		return nil
	}
	when := rec.ReadAt.Local().Format("2 Jan 15:04")
	var lines []string
	if len(rec.Certs) > 0 {
		var certs []string
		for _, name := range slices.Sorted(maps.Keys(rec.Certs)) {
			state := map[string]string{macvm.ItemTrusted: "trusted", macvm.ItemNotTrusted: "not trusted"}[rec.Certs[name]]
			certs = append(certs, name+" "+cmp.Or(state, "could not be read"))
		}
		lines = append(lines, fmt.Sprintf("certificates, read at the last edit (%s): %s", when, strings.Join(certs, ", ")))
	}
	return append(lines, fmt.Sprintf("Full Disk Access for the guest agent, read at the last edit (%s): %s", when, cmp.Or(rec.FullDiskAccess, "could not be read")))
}

// writeText prints the status for a person.
func (st macOSVMStatus) writeText(out io.Writer) {
	fmt.Fprintf(out, "base: %s", st.State)
	if st.Base != "" {
		fmt.Fprintf(out, ", %s", st.Base)
	}
	if st.State == runnerwire.MacVMBaseReady {
		if at, err := time.Parse(time.RFC3339, st.PreparedAt); err == nil {
			fmt.Fprintf(out, ", prepared %s (%s)", at.Local().Format(time.DateOnly), daysAgo(calendarDays(at, statusNow())))
		}
		if st.HostOS != "" {
			fmt.Fprintf(out, "; this Mac: macOS %s", st.HostOS)
		}
	}
	if st.State == runnerwire.MacVMBasePreparing {
		fmt.Fprintf(out, ", %d%%", st.progress)
	}
	if st.Error != "" {
		fmt.Fprintf(out, ": %s", st.Error)
	}
	fmt.Fprintln(out)
	if st.setupCode != "" {
		fmt.Fprintf(out, "macOS asked for setup by hand. In the window %q on this Mac, create the account wardyn-admin with the one-time code in %s as its password, turn on Screen Sharing with control, and turn on Remote Login.\n", macOSVMSetupWindow, st.setupCode)
	} else if st.byHand && st.State != runnerwire.MacVMBaseReady {
		fmt.Fprintf(out, "This Mac's macOS has no guest provisioning (macOS %d has it), so preparing its base needs setup by hand once: after the install, the window %q opens on this Mac.\n", macvm.ProvisionHostMajor, macOSVMSetupWindow)
	}
	if st.State == runnerwire.MacVMBaseReady {
		fmt.Fprintf(out, "harnesses: %s; desktop: %s; guest minimum: %d CPUs, %d MiB\n", strings.Join(st.Harnesses, ", "), desktopMode(st.Desktop), st.minCPUs, st.minMemoryMiB)
		for _, l := range st.withheld {
			fmt.Fprintln(out, l)
		}
		for _, l := range st.editRead {
			fmt.Fprintln(out, l)
		}
	}
	if st.Kept != nil {
		st.Kept.writeText(out)
	}
	for _, b := range st.Bases {
		fmt.Fprintf(out, "other base %s: %s", b.Name, b.State)
		if b.Reason != "" {
			fmt.Fprintf(out, ": %s", b.Reason)
		}
		fmt.Fprintln(out)
	}
	if st.Thermal != "" {
		fmt.Fprintf(out, "thermal: %s\n", st.Thermal)
	}
	if st.ToolsDial != "" {
		fmt.Fprintf(out, "tools_dial: %s (read from %s)\n", st.ToolsDial, st.ToolsDialRead)
	}
	fmt.Fprintf(out, "macOS VMs on this Mac (every login): %d; this runner offers %d at once\n", st.counted, st.Max)
	fmt.Fprintln(out, "That number is this Mac's own sum (Apple's cap, memory, free disk). It does not read the organisation's limits (Runs at once, the CPU, memory and disk totals, other runs' reservations), which can hold fewer.")
	if st.Leaked > 0 {
		fmt.Fprintf(out, "VM slots macOS holds until this Mac restarts: %d\n", st.Leaked)
	}
	if st.HeldDiskBytes > 0 {
		fmt.Fprintf(out, "disk held for ended logins' VMs until they log in again: %d GB\n", st.HeldDiskBytes>>30)
	}
}

// macOSVMSetupWindow is the short name of the setup-by-hand window the helper opens (internal/macvm wardynScreenShow):
// the window's title is it.
const macOSVMSetupWindow = "Wardyn: set up this macOS VM"

// runToolsDialFile, in the VM directory, is the driver's record of how the last run boot's guest agent
// dial of the tools port ended (docker.MacVMConfig.ToolsDialFile).
const runToolsDialFile = "run-tools-dial"

// lastToolsDial is how the newest tools-port dial ended, when it is one of the five classes, else "", and
// where it was read from (read). The candidates are the run boot's record, the last dial prepare logged and
// the last one a tools update logged (both stage 2 boots reach the guest agent); the latest of the three
// wins. A prepare or an update does not rewrite a run boot's record, so reading that alone kept a failure
// from before a tools update that reached ready. A tools update stamps its dial (macvm.PrepareEvent.ToolsDialAt);
// an event from a helper that did not has no clock of its own, so its time is when its log was last written, which
// read says. A log that could not be read to its end is named, since a later dial may sit past the stop.
func lastToolsDial(root string) (class, read string) {
	type candidate struct {
		class, source string
		at            time.Time
		logTime       bool
	}
	var best candidate
	var unread []string
	if b, err := os.ReadFile(filepath.Join(root, runToolsDialFile)); err == nil {
		if s := strings.TrimSpace(string(b)); macvm.ValidToolsDial(s) {
			best = candidate{class: s, source: "the last run boot's record"}
			if fi, err := os.Stat(filepath.Join(root, runToolsDialFile)); err == nil {
				best.at = fi.ModTime()
			}
		}
	}
	for _, l := range []struct{ path, source string }{
		{macvm.Layout{Root: root}.PrepareLog(), "the prepare log"},
		{filepath.Join(root, toolsUpdateLog), "the tools update log"},
	} {
		pc, pat, err := lastPrepareToolsDial(l.path)
		if err != nil {
			unread = append(unread, l.source)
			continue
		}
		if pc == "" {
			continue
		}
		c := candidate{class: pc, source: l.source, at: pat}
		if pat.UnixNano() == 0 {
			fi, err := os.Stat(l.path)
			if err != nil {
				continue
			}
			c.at, c.logTime = fi.ModTime(), true
		}
		if c.at.After(best.at) {
			best = c
		}
	}
	if !macvm.ValidToolsDial(best.class) {
		return "", ""
	}
	read = best.source
	if !best.at.IsZero() {
		verb := "reported "
		if best.logTime {
			verb = "last written "
		}
		read += ", " + verb + best.at.Local().Format("2006-01-02 15:04 MST")
	}
	for _, u := range unread {
		read += "; " + u + " could not be read to its end, so a later dial may be missing"
	}
	return best.class, read
}

// editsCode: the one-time code file at root is an edit's, not setup by hand's. An edit writes its log
// before the code file, and a prepare writes prepare.log before its own, so the newer log says whose.
func editsCode(root string) bool {
	el, err := os.Stat(filepath.Join(root, editBaseLog))
	if err != nil {
		return false
	}
	pl, err := os.Stat(macvm.Layout{Root: root}.PrepareLog())
	return err != nil || el.ModTime().After(pl.ModTime())
}

// macOSVMDoctorStages is doctor's macOS VM stages. While macOS waits for setup by hand, not during an edit
// (editsCode), one names the file that holds the one-time code, never the code. With a base, guest-agent says
// how the last tools update signed the guest agent, and folder-shares whether a folder can be shared into a
// run (roots and their source are the drive roots in effect).
func macOSVMDoctorStages(roots []string, rootsSource string) []doctor.Stage {
	root, err := macvm.DefaultRoot()
	if err != nil {
		return nil
	}
	var stages []doctor.Stage
	if p := setupCodeWaiting(root); p != "" && !editsCode(root) {
		stages = append(stages, doctor.SkipStage("macos-vm", "macOS asked for setup by hand in the window \""+macOSVMSetupWindow+"\"; the one-time code is in "+p))
	}
	base := macvm.Layout{Root: root}.BaseDir(0)
	man, err := macvm.ReadManifest(base)
	if err != nil {
		return stages
	}
	tools, _ := macvm.ReadToolsRecord(base)
	var fda string
	if tools.Guest != nil {
		fda = tools.Guest.FullDiskAccess
		if st, ok := fileVaultStage(tools.Guest.FileVault); ok {
			stages = append(stages, st)
		}
	}
	var readAt time.Time
	if rec, ok := macvm.ReadEditRecord(base, man); ok && rec.FullDiskAccess == fda {
		readAt = rec.ReadAt
	}
	stages = append(stages, guestAgentStage(tools, guestAgentWarnings(tools)))
	return append(stages, folderSharesStage(roots, rootsSource, fda, tools.AgentSignature == macvm.AgentSignedAdHoc, readAt, tools.TriedAt))
}

// daysAgo is "1 day ago" for one and "<n> days ago" for every other count.
func daysAgo(n int) string {
	if n == 1 {
		return "1 day ago"
	}
	return fmt.Sprintf("%d days ago", n)
}

// prepareMirror is the mirror prepare downloads from: the flags, else the profile or runnerd.json. A restore
// image file named with --ipsw stands alone, and so does a mirror named with --ipsw-url: its hash is --sha256
// or prepare refuses, never the profile's, which was pinned for another image.
func prepareMirror(cmd *cobra.Command, ipsw, ipswURL, sha256 string) (url, hash string, err error) {
	if ipsw != "" || cmd.Flags().Changed("ipsw-url") {
		return ipswURL, sha256, nil
	}
	cfg, err := serveConfig(cmd, "")
	if err != nil {
		return "", "", err
	}
	return cfg.get("macos_vm_image_url"), cfg.get("macos_vm_image_sha256"), nil
}

// calendarDays is the days from the local date of from to the local date of to, never negative: a base
// prepared yesterday evening is "1 day ago" this morning, the date the status line prints beside it.
func calendarDays(from, to time.Time) int {
	day := func(t time.Time) time.Time {
		y, m, d := t.Local().Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	return max(0, int(day(to).Sub(day(from)).Hours()/24))
}
