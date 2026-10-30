// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// The lines `wardyn-runnerd macos-vm edit-base` prints, from the helper's edit-base (approved copy).
const (
	EditStartLine    = "Editing this Mac's macOS base. Runs keep using the current base until the edited one passes its checks."
	editCodeLine     = "Sign in as wardyn-admin with the one-time code in %s."
	editProfileLine  = "To install a staged profile: signed in as wardyn-admin, run sudo open %s in Terminal, then approve it in System Settings."
	EditCheckingLine = "The window closed and the guest shut down. Checking the edited base: desktop control, accounts, Remote Login and the launcher's self-test."
	editReadyLine    = "The edited base is ready, and runs now use it. Desktop: %s."
)

// ScreenText is a first boot's window text: its title and the strip of steps under the title bar.
// Closable gives the window a close button that asks the guest to shut down. The zero value is setup
// by hand's window, whose text lives in screen_darwin.m.
type ScreenText struct {
	Title, Steps string
	Closable     bool
}

// editScreen is the edit window's text.
var editScreen = ScreenText{
	Title: "Wardyn: edit this macOS VM base",
	Steps: "Sign in as wardyn-admin with the one-time code. Make your changes, then close this window. Wardyn checks the base before runs use it.\n" +
		"For desktop control: in System Settings > General > Sharing, turn on Screen Sharing. Under its (i) button, turn on \"VNC viewers may control screen with password\" (any password; Wardyn replaces it).\n" +
		"Leave Remote Login off and add no accounts: the check refuses a base with either.",
	Closable: true,
}

// screenTexter is a FirstBooter whose Boot's window can show other text than setup by hand's.
type screenTexter interface {
	WithScreen(ScreenText) FirstBooter
}

// EditRefusal is why edit-base starts nothing. Its text is what the person reads.
type EditRefusal string

func (r EditRefusal) Error() string { return string(r) }

// The edit-base refusals. runnerd answers the same before it starts the helper, which checks again.
const (
	ErrEditBusy      EditRefusal = "This Mac is preparing or updating its macOS base. Run edit-base when that finishes."
	ErrEditNoBase    EditRefusal = "This Mac has no ready macOS base to edit. Run `wardyn-runnerd macos-vm prepare` first."
	ErrEditNoDesktop EditRefusal = "edit-base needs this Mac's desktop: run it in Terminal on the Mac, signed in at its desktop, not over SSH and not with sudo."
	ErrEditSlots     EditRefusal = "Editing uses one of this Mac's 2 macOS VM slots, and both are in use. Try again when a run ends."
)

// An edit's recheck result (EditRecord.Recheck): passed, or why the edited copy was discarded.
const (
	EditRecheckPassed       = "passed"
	EditRecheckDesktopOff   = "desktop_control_off"
	EditRecheckExtraAccount = "extra_account"
	EditRecheckRemoteLogin  = "remote_login"
	EditRecheckSelftest     = "selftest_failed"
)

// EditDiscardedError is an edit whose copy failed its recheck and was discarded: runs keep the base from
// Since. Detail is the account or the harness the line names.
type EditDiscardedError struct {
	Recheck string
	Detail  string
	Since   time.Time
}

func (e *EditDiscardedError) Error() string {
	since := e.Since.Local().Format("2 Jan")
	switch e.Recheck {
	case EditRecheckDesktopOff:
		return "The edited base was discarded: desktop control is off, because Screen Sharing was not turned on with control. Runs keep using the base from " + since + "."
	case EditRecheckExtraAccount:
		return "The edited base was discarded: it has an account Wardyn didn't make, " + e.Detail + ". Runs keep using the base from " + since + "."
	case EditRecheckRemoteLogin:
		return "The edited base was discarded: Remote Login is on. Runs keep using the base from " + since + "."
	}
	return "The edited base was discarded: the launcher's self-test failed for " + e.Detail + ". Runs keep using the base from " + since + "."
}

// EditGuest is a ToolsGuest that can also take the setup admin's code and read the guest's posture: the
// stage-2 boot's guest (stage2boot.go).
type EditGuest interface {
	ToolsGuest
	// AdminCode sets the setup admin's password and says whether it was set. It is never logged.
	AdminCode(ctx context.Context, code []byte) (set bool, err error)
	Posture(ctx context.Context) (guestagent.ToolsPosture, error)
}

// EditOptions configures EditBase.
type EditOptions struct {
	Engine Engine
	Layout Layout
	Slot   int
	// Admit claims the host ledger entry the edit's boots run under, one at a time (nil: the caller holds one).
	Admit func(ctx context.Context) (release func(), err error)
	// Guest boots the staging copy for the guest agent's tools session: once to set the one-time code,
	// once for the recheck.
	Guest ToolsBoot
	// FirstBoot and PrepNet boot the staging copy plain, in a window on this Mac, on the no-egress prep
	// network, for the person's session.
	FirstBoot FirstBooter
	PrepNet   func() (PrepNetwork, error)
	// Leaked records a framework slot a refused window boot left (RunOptions.Leaked).
	Leaked func() error
	// Items is the install list (macos_vm_base_file), whose files stage again each profile still waiting
	// for the person's approval (restageProfiles).
	Items BaseList
	// Desktop is the control check's RFB client (nil: the boot's own).
	Desktop DesktopProbe
	// CPUs and MemoryMiB are the runner's per-run limits, never raised.
	CPUs      int
	MemoryMiB uint64
	// Out gets the person's lines.
	Out io.Writer
	Log *slog.Logger
	Now func() time.Time
}

// EditBase is `wardyn-macvm edit-base`: one attended session on a copy of the ready base, checked before
// runs use it. It clones the base to the update's staging dir under the update lock (a prepare or tools
// update holding it refuses the edit), sets a one-time code as the setup admin's password in a headless
// boot and writes it to the setup-code file, boots the copy in a window until the guest shuts down, then
// rechecks it in a second headless boot: the code is replaced by a value no process keeps, then the
// control check, the accounts, Remote Login and the launcher's self-test. Only a copy that passes is swapped
// in, by the tools update's one atomic exchange; any failure discards it, so runs keep the old base. The
// code is in no log and its file goes when the window closes, or when the edit ends.
func EditBase(ctx context.Context, o EditOptions) (Manifest, error) {
	if o.Engine == nil || o.Guest == nil || o.FirstBoot == nil || o.PrepNet == nil || o.Layout.Root == "" {
		return Manifest{}, errors.New("macvm: an edit needs an engine, its boots, a prep network and a root")
	}
	o.setDefaults()
	if err := o.FirstBoot.ScreenReady(); geteuid() == 0 || errors.Is(err, ErrNoDesktopSession) {
		return Manifest{}, ErrEditNoDesktop
	} else if err != nil {
		return Manifest{}, err
	}
	base := o.Layout.BaseDir(o.Slot)
	if _, err := ReadyBase(base); err != nil {
		return Manifest{}, ErrEditNoBase
	}
	// One try at the update lock: a prepare or a tools update holds it for its whole run.
	try, cancel := context.WithCancel(ctx)
	cancel()
	staging, done, err := beginUpdate(try, base, o.Log)
	if errors.Is(err, context.Canceled) && ctx.Err() == nil {
		return Manifest{}, ErrEditBusy
	}
	if err != nil {
		return Manifest{}, err
	}
	release := func() {}
	defer func() { done(); release() }()
	m, err := ReadyBase(base)
	if err != nil {
		return m, ErrEditNoBase
	}
	if o.Admit != nil {
		if release, err = o.Admit(ctx); err != nil {
			return m, err
		}
	}
	fmt.Fprintln(o.Out, EditStartLine)
	id, err := cloneRun(o.Engine, base, staging, IdentityTravel)
	if err != nil {
		return m, err
	}
	spec := MachineSpec{
		CPUs: o.CPUs, MemoryBytes: o.MemoryMiB * mib, MinCPUs: m.MinCPUs, MinMemoryBytes: m.MinMemoryMiB * mib,
		HardwareModel: m.HardwareModel, MachineIdentifier: id.MachineIdentifier,
		DiskPath: filepath.Join(staging, DiskFile), AuxPath: filepath.Join(staging, AuxFile),
	}
	// The copy carries the base's tools.json, which the recheck's read-back updates.
	if err := copyToolsRecord(base, staging); err != nil {
		return m, err
	}
	staged, err := StagedProfiles(base)
	if err != nil {
		return m, err
	}
	codeFile := o.Layout.SetupCode()
	removeCode := func() {
		if err := os.Remove(codeFile); err != nil && !os.IsNotExist(err) {
			o.Log.Warn("macvm: the setup-code file was not deleted", "path", codeFile, "err", err.Error())
		}
	}
	defer removeCode()
	offered, err := o.mintCode(ctx, spec, codeFile, staging)
	if err != nil {
		return m, err
	}
	fmt.Fprintf(o.Out, editCodeLine+"\n", codeFile)
	for _, p := range offered {
		fmt.Fprintf(o.Out, editProfileLine+"\n", p)
	}
	if err := o.window(ctx, spec); err != nil {
		return m, err
	}
	removeCode()
	fmt.Fprintln(o.Out, EditCheckingLine)
	last, _ := ReadEditRecord(base, m)
	desktop, err := o.recheck(ctx, spec, m, staging, staged)
	var discarded *EditDiscardedError
	if errors.As(err, &discarded) {
		discarded.Since = m.CreatedAt
		if !last.EditedAt.IsZero() {
			discarded.Since = last.EditedAt
		}
		last.Recheck = discarded.Recheck
		o.writeRecord(base, m, last)
	}
	if err != nil {
		return m, err
	}
	m.Desktop = desktop
	if err := writeJSONAtomic(filepath.Join(staging, ManifestFile), m, 0o600); err != nil {
		return m, err
	}
	if err := swapBase(ctx, staging, base); err != nil {
		return m, err
	}
	o.writeRecord(base, m, EditRecord{EditedAt: o.Now().UTC(), Recheck: EditRecheckPassed})
	fmt.Fprintf(o.Out, editReadyLine+"\n", desktop)
	return m, nil
}

func (o *EditOptions) setDefaults() {
	if o.Out == nil {
		o.Out = io.Discard
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewJSONHandler(io.Discard, nil))
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// guest boots spec for the guest agent's tools session and runs f on it, then stops the boot.
func (o *EditOptions) guest(ctx context.Context, spec MachineSpec, f func(EditGuest) error) error {
	g, stop, err := o.Guest(ctx, spec)
	if err != nil {
		return fmt.Errorf("macvm: the edit's guest agent boot: %w", err)
	}
	eg, ok := g.(EditGuest)
	if !ok {
		err = errors.New("macvm: this boot's guest cannot take the one-time code")
	} else {
		err = f(eg)
	}
	sctx, cancel := context.WithTimeout(context.Background(), DefaultStopTimeout)
	defer cancel()
	if serr := stop(sctx); err == nil && serr != nil {
		err = fmt.Errorf("macvm: the stage 2 VM did not shut down cleanly: %w", serr)
	}
	return err
}

// setAdminCode sets code as the setup admin's password; a guest that did not take it fails.
func setAdminCode(ctx context.Context, g EditGuest, code []byte) error {
	set, err := g.AdminCode(ctx, code)
	if err == nil && !set {
		err = errors.New("the guest did not take it")
	}
	if err != nil {
		return fmt.Errorf("macvm: the setup admin's one-time code: %w", err)
	}
	return nil
}

// mintCode sets a fresh one-time code in a headless boot, stages the waiting profiles again there
// (restageProfiles, from staging's tools.json) and, once the boot has stopped, writes the code to the
// setup-code file (0600), which only the person at this Mac reads. It returns the profiles' guest paths.
func (o *EditOptions) mintCode(ctx context.Context, spec MachineSpec, path, staging string) (offered []string, err error) {
	code, err := randomCode(8)
	if err != nil {
		return nil, err
	}
	defer clear(code)
	err = o.guest(ctx, spec, func(g EditGuest) error {
		if err := setAdminCode(ctx, g, code); err != nil {
			return err
		}
		offered, err = restageProfiles(ctx, g, staging, o.Items)
		return err
	})
	if err != nil {
		return nil, err
	}
	return offered, writeSetupCode(path, SetupCodeRecord{Account: provisionUser, Code: string(code)})
}

// window boots spec plain in a window on this Mac, with the edit's text, and waits until the guest shuts
// down. The window's close button asks the guest to.
func (o *EditOptions) window(ctx context.Context, spec MachineSpec) error {
	texter, ok := o.FirstBoot.(screenTexter)
	if !ok {
		return errors.New("macvm: the edit needs the guest's window, and this build cannot open one")
	}
	booter := texter.WithScreen(editScreen)
	nw, err := o.PrepNet()
	if err != nil {
		return err
	}
	defer nw.Close()
	guest, host, err := NewNetPair()
	if err != nil {
		return err
	}
	defer guest.Close()
	defer host.Close()
	go func() { _ = nw.Serve(host) }()
	var fb FirstBoot
	// The headless boot, just stopped, may hold the aux storage a moment longer.
	err = retryAuxLocked(ctx, o.Log, func() error {
		var err error
		fb, err = booter.Boot(spec.DiskPath, spec.AuxPath, spec.HardwareModel, spec.MachineIdentifier,
			uint(spec.CPUs), spec.MemoryBytes, guest, "", "")
		return err
	})
	if err != nil {
		reportVMLimit(err, o.Leaked, o.Log)
		return fmt.Errorf("macvm: the edit window's boot: %w", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			sctx, cancel := context.WithTimeout(context.Background(), DefaultStopTimeout)
			defer cancel()
			_ = fb.Stop(sctx)
		}
	}()
	if fb.Screen == nil {
		return errors.New("macvm: the edit needs the guest's window, and this build cannot open one")
	}
	closeWindow, err := fb.Screen()
	if err != nil {
		return fmt.Errorf("macvm: the edit window: %w", err)
	}
	defer closeWindow()
	select {
	case err := <-fb.Done:
		stopped = true
		if err != nil {
			return fmt.Errorf("macvm: the edit window's guest stopped: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recheck boots the edited copy headless: it replaces the one-time code first, whatever follows, then
// checks desktop control, the accounts, Remote Login and the launcher's self-test, in that order. The
// first that fails is an *EditDiscardedError. Right after the code it removes the staged profiles, and after
// Remote Login it reads the items back into staging's tools.json (ReadBackItems), a failure of either
// discarding the copy with no line of its own. It returns the desktop mode.
func (o *EditOptions) recheck(ctx context.Context, spec MachineSpec, m Manifest, staging string, staged []string) (desktop string, err error) {
	err = o.guest(ctx, spec, func(g EditGuest) error {
		reset, err := randomCode(24)
		if err != nil {
			return err
		}
		defer clear(reset)
		if err := setAdminCode(ctx, g, reset); err != nil {
			return err
		}
		if err := removeStagedProfiles(ctx, g, staged); err != nil {
			return fmt.Errorf("macvm: the staged profiles: %w", err)
		}
		probe := o.Desktop
		if dp, ok := g.(DesktopProbe); ok && probe == nil {
			probe = dp
		}
		if desktop, err = controlCheck(ctx, g, probe); err != nil {
			return err
		}
		if desktop != DesktopControl {
			return &EditDiscardedError{Recheck: EditRecheckDesktopOff}
		}
		p, err := g.Posture(ctx)
		if err != nil {
			return fmt.Errorf("macvm: the edited base's accounts: %w", err)
		}
		if a := extraAccount(p.Accounts); a != "" {
			return &EditDiscardedError{Recheck: EditRecheckExtraAccount, Detail: a}
		}
		if p.RemoteLogin {
			return &EditDiscardedError{Recheck: EditRecheckRemoteLogin}
		}
		if err := ReadBackItems(ctx, g, staging); err != nil {
			return fmt.Errorf("macvm: the edited base's items read-back: %w", err)
		}
		res := ToolsResult{Harnesses: slices.Clone(m.Harnesses), Withheld: map[string]error{}, Pins: map[string]PinRecord{}}
		var be *BaseSelftestError
		if err := selftestBase(ctx, g, &res, o.Log); errors.As(err, &be) {
			return &EditDiscardedError{Recheck: EditRecheckSelftest, Detail: HarnessNone}
		} else if err != nil {
			return err
		}
		for _, h := range m.Harnesses {
			if _, ok := res.Withheld[h]; ok {
				return &EditDiscardedError{Recheck: EditRecheckSelftest, Detail: h}
			}
		}
		return nil
	})
	return desktop, err
}

// macOSAccounts are macOS's own accounts below uid 500 that have no leading "_".
var macOSAccounts = []string{"root", "daemon", "nobody"}

// extraAccount is the first account Wardyn didn't make: anything but the setup admin, the agent user, and
// macOS's own (below uid 500, named with a leading "_" or one of macOSAccounts). "" when there is none.
// It is a tripwire, not prevention: the Mac's person is root on the host and can change the disk directly.
func extraAccount(accounts []guestagent.GuestAccount) string {
	for _, a := range accounts {
		switch {
		case a.Name == guestagent.MacAdminUser || a.Name == guestagent.MacAgentUser:
		case a.UID < 500 && (strings.HasPrefix(a.Name, "_") || slices.Contains(macOSAccounts, a.Name)):
		default:
			return a.Name
		}
	}
	return ""
}

// copyToolsRecord carries the base's tools.json into the edited copy: the edit installs no tools.
func copyToolsRecord(base, staging string) error {
	rec, err := ReadToolsRecord(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(staging, ToolsRecordFile), rec, 0o600)
}

// EditRecord is <base>.edit.json, beside the base so a tools update's swap keeps it: the base's last edit.
type EditRecord struct {
	// BaseCreatedAt is the edited base's manifest CreatedAt: a new prepare's base is another base, and the
	// record does not describe it.
	BaseCreatedAt time.Time `json:"base_created_at"`
	// EditedAt is when an edited copy last passed its recheck and was swapped in (zero: none has).
	EditedAt time.Time `json:"edited_at,omitzero"`
	// Recheck is the last edit's recheck: EditRecheckPassed, or why its copy was discarded.
	Recheck string `json:"recheck"`
}

func editRecordPath(baseDir string) string { return baseDir + ".edit.json" }

// ReadEditRecord is the edit record of the base in baseDir, whose manifest is m; ok is false when there is
// none for this base.
func ReadEditRecord(baseDir string, m Manifest) (rec EditRecord, ok bool) {
	b, err := os.ReadFile(editRecordPath(baseDir))
	if err != nil || json.Unmarshal(b, &rec) != nil || !rec.BaseCreatedAt.Equal(m.CreatedAt) {
		return EditRecord{}, false
	}
	return rec, true
}

// BaseEditedAt is when the base in baseDir, whose manifest is m, was last edited (nil: never).
func BaseEditedAt(baseDir string, m Manifest) *time.Time {
	rec, ok := ReadEditRecord(baseDir, m)
	if !ok || rec.EditedAt.IsZero() {
		return nil
	}
	return &rec.EditedAt
}

// writeRecord writes base's edit record for the base whose manifest is m. It is written after the swap:
// a crash between the two leaves an edited base that reads as not edited, never the reverse.
func (o *EditOptions) writeRecord(base string, m Manifest, rec EditRecord) {
	rec.BaseCreatedAt = m.CreatedAt
	if err := writeJSONAtomic(editRecordPath(base), rec, 0o600); err != nil {
		o.Log.Warn("macvm: the base's edit record was not written", "err", err.Error())
	}
}
