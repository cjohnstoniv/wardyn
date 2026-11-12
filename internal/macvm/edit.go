// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// The lines `wardyn-runnerd macos-vm edit-base` prints, from the helper's edit-base (approved copy).
const (
	EditStartLine    = "This edits a copy of the macOS base your VMs start from, not this Mac. Runs keep using the current base until the copy passes its checks."
	editCodeLine     = "The VM signs in as agent by itself. The one-time code is wardyn-admin's password, for its password prompts: click Type code in the window to type it, or Show code to read it."
	editProfileLine  = "While signed in as wardyn-admin, open %s, then review and install the staged profile in System Settings."
	EditCheckingLine = "The window closed and the guest shut down. Checking the edited base: desktop control, accounts, Remote Login and the launcher's self-test."
	editReadyLine    = "The edited base is ready, and runs now use it. Desktop: %s."
)

// ScreenText is a first boot's window text: its title and the strip of steps under the title bar.
// CodeFile, the setup-code file's path (not secret), gives the strip a Show code control that reads the
// code from it on a click; empty gives none. The zero value is setup by hand's window, whose text lives
// in screen_darwin.m.
type ScreenText struct {
	Title, Steps string
	CodeFile     string
}

// editScreen is the edit window's text. Its lines run in the order a person needs them: the code, the
// setup questions, who they are and the admin Terminal, Full Disk Access, the certificates, then the two
// checks the recheck makes.
var editScreen = WindowText{
	Title: "Wardyn: edit this macOS VM base",
	Strip: "The one-time code is wardyn-admin's password: click Type code to type it at a password prompt, or Show code to see it. Make your changes, then close this window: the VM shuts down (if macOS asks, click Shut Down). Wardyn checks the base before runs use it.\n" +
		"The first time, macOS may ask setup questions. Finish them without adding an Apple account. What you click stays for every run.\n" +
		"The VM signs in as agent by itself; stay signed in. For administrator steps click Open admin Terminal and type the code when it asks for wardyn-admin's password. If a login window shows instead, sign in there as wardyn-admin with the code.\n" +
		"Full Disk Access, for folder shares: click Full Disk Access, then drag the guest agent (" + guestagent.MacAgentBinaryPath + ") from Finder into the list. Grant it once: Wardyn signs the agent on this Mac so the grant can last through tools updates. It lets every program a run starts in this VM read protected folders, in every run.\n" +
		"Certificates that are not trusted are listed in the Terminal where you started edit-base. Click Trust the list's CAs, check each one, and type the one-time code when it asks; in macOS's dialog, enter wardyn-admin and the code.\n" +
		"For desktop control: in System Settings > General > Sharing, turn on Screen Sharing. Under its (i) button, turn on \"VNC viewers may control screen with password\" (any password; Wardyn replaces it).\n" +
		"Leave Remote Login off and add no accounts: the check refuses a base with either.",
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

// ErrEditNoAdminPassword refuses a base that keeps no AdminPasswordFile. Only the helper checks it,
// before anything boots.
const ErrEditNoAdminPassword EditRefusal = "This macOS base was prepared before Wardyn kept its admin password, so edit-base cannot open it. Run `wardyn-runnerd macos-vm prepare --new-image`, then edit-base again."

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
		return "The edited base was discarded: desktop control was lost: the base had it and the copy does not. Runs keep using the base from " + since + "."
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
	// AdminCode changes the setup admin's password from old to code and says whether it changed. Neither is
	// ever logged.
	AdminCode(ctx context.Context, old, code []byte) (set bool, err error)
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
	// WindowBoot and PrepNet boot the staging copy for the person's session: a tools boot like Guest's, on
	// the no-egress prep network, which Screen draws in a window on this Mac. Screen nil is the Mac's edit
	// window (nativeEditScreen); a build without one refuses the edit.
	WindowBoot WindowBoot
	PrepNet    func() (PrepNetwork, error)
	Screen     WindowScreen
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
// update holding it refuses the edit), changes the setup admin's password from the one the base keeps
// (AdminPasswordFile) to a one-time code in a headless boot and writes it to the setup-code file, boots the
// copy in a window until the guest shuts down, then rechecks it in a second headless boot: the code is
// replaced by a fresh value the copy keeps, then the control check, the accounts, Remote Login and the
// launcher's self-test. Only a copy that passes is swapped in, by the tools update's one atomic exchange,
// with the copy's AdminPasswordFile; any failure discards it, so runs keep the old base. The code is in no
// log and its file goes when the window closes, or when the edit ends.
func EditBase(ctx context.Context, o EditOptions) (Manifest, error) {
	if o.Engine == nil || o.Guest == nil || o.WindowBoot == nil || o.PrepNet == nil || o.Layout.Root == "" {
		return Manifest{}, errors.New("macvm: an edit needs an engine, its boots, a prep network and a root")
	}
	o.setDefaults()
	scr, err := o.editScreen()
	if err != nil {
		return Manifest{}, err
	}
	base := o.Layout.BaseDir(o.Slot)
	if _, err := ReadyBase(base); err != nil {
		return Manifest{}, ErrEditNoBase
	}
	pw, err := readAdminPassword(base, AdminPasswordFile)
	clear(pw)
	if err != nil {
		return Manifest{}, ErrEditNoAdminPassword
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
	id, err := cloneStaging(o.Engine, base, staging)
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
	fmt.Fprintln(o.Out, "Starting the copy in a window. This takes about 2 minutes.")
	offered, err := o.mintCode(ctx, spec, codeFile, staging)
	if err != nil {
		return m, err
	}
	fmt.Fprintln(o.Out, editCodeLine)
	for _, p := range offered {
		fmt.Fprintf(o.Out, editProfileLine+"\n", p)
	}
	o.printUntrusted(base)
	if err := o.window(ctx, spec, scr); err != nil {
		return m, err
	}
	removeCode()
	fmt.Fprintln(o.Out, EditCheckingLine)
	last, _ := ReadEditRecord(base, m)
	desktop, read, err := o.recheck(ctx, spec, m, staging, len(offered) > 0, staged)
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
	read.EditedAt, read.Recheck = o.Now().UTC(), EditRecheckPassed
	o.writeRecord(base, m, read)
	fmt.Fprintf(o.Out, editReadyLine+"\n", o.desktopWords(desktop))
	return m, nil
}

// editScreen is the window the edit shows the guest in: o.Screen, else the Mac's own. It refuses when this
// build cannot open one, when the process runs as root or no one is signed in at the Mac (ErrEditNoDesktop), and
// when the window check fails otherwise.
func (o *EditOptions) editScreen() (WindowScreen, error) {
	scr := o.Screen
	if scr == nil {
		scr = nativeEditScreen
	}
	if scr == nil {
		return nil, errors.New("macvm: the edit needs the guest's window, and this build cannot open one")
	}
	if err := scr.Ready(); geteuid() == 0 || errors.Is(err, ErrNoDesktopSession) {
		return nil, ErrEditNoDesktop
	} else if err != nil {
		return nil, err
	}
	return scr, nil
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

// setAdminCode changes the setup admin's password to code from the one staging keeps (rotateAdmin), so
// staging then keeps code; a guest that did not take it fails.
func setAdminCode(ctx context.Context, g EditGuest, staging string, code []byte) error {
	if err := rotateAdmin(ctx, g, staging, code); err != nil {
		return fmt.Errorf("macvm: the setup admin's one-time code: %w", err)
	}
	return nil
}

// editCodeLen is the edit's one-time code's length, in typableAlphabet: about 43 bits, for a code that only
// opens an offline copy at this Mac and that the recheck replaces.
const editCodeLen = 10

// mintCode changes the setup admin's password to a fresh one-time code in a headless boot, which staging's
// AdminPasswordFile then holds, offers the waiting profiles there (offerProfiles, from staging's tools.json)
// and, once the boot has stopped, writes the code to the setup-code file (0600), which only the person at
// this Mac reads. It returns wardyn-admin's copies' paths.
func (o *EditOptions) mintCode(ctx context.Context, spec MachineSpec, path, staging string) (offered []string, err error) {
	code, err := typableCode(editCodeLen)
	if err != nil {
		return nil, err
	}
	defer clear(code)
	err = o.guest(ctx, spec, func(g EditGuest) error {
		if err := setAdminCode(ctx, g, staging, code); err != nil {
			return err
		}
		offered, err = offerProfiles(ctx, g, staging, o.Items, o.Log)
		return err
	})
	if err != nil {
		return nil, err
	}
	return offered, writeSetupCode(path, SetupCodeRecord{Account: provisionUser, Code: string(code)})
}

// window boots spec for the person's session on a window boot (a tools boot like the headless ones) on the
// no-egress prep network, and draws its VM in scr's window with the edit's text and the Show code control,
// until the guest shuts down. The window's close button asks the guest to (WindowText.AskStop), and the
// window stays up until it has. Once the VM has stopped, the boot's stop answers whether that was the
// guest's own shutdown: a stop through the boot's ctl.sock (ErrStage2Stopped) is the edit's error, so the
// copy is never rechecked or swapped in.
func (o *EditOptions) window(ctx context.Context, spec MachineSpec, scr WindowScreen) error {
	nw, err := o.PrepNet()
	if err != nil {
		return err
	}
	defer nw.Close()
	nic, host, err := NewNetPair()
	if err != nil {
		return err
	}
	defer nic.Close()
	defer host.Close()
	go func() { _ = nw.Serve(host) }()
	g, stop, err := o.WindowBoot(ctx, spec, nic)
	if err != nil {
		return fmt.Errorf("macvm: the edit window's boot: %w", err)
	}
	stopped := false
	stopBoot := func() error {
		stopped = true
		sctx, cancel := context.WithTimeout(context.Background(), DefaultStopTimeout)
		defer cancel()
		return stop(sctx)
	}
	defer func() {
		if !stopped {
			if err := stopBoot(); err != nil {
				o.Log.Warn("macvm: the edit window's VM did not shut down cleanly", "err", err.Error())
			}
		}
	}()
	wg, ok := g.(WindowGuest)
	if !ok {
		return errors.New("macvm: the edit window's boot gives no VM to draw")
	}
	ptr, err := wg.ViewHandle()
	if err != nil || ptr == nil {
		return fmt.Errorf("macvm: the edit window: the VM's pointer is unreadable: %v", err)
	}
	text := editScreen
	text.CodeFile = o.Layout.SetupCode()
	// The first close asks the guest agent to shut the guest down. A guest without that action or whose
	// agent did not accept, and any later close, gets the VM's stop request, which macOS confirms with
	// its shut-down dialog.
	var askedAgent atomic.Bool
	text.AskStop = func() {
		if sg, ok := g.(ShutDownGuest); ok && !askedAgent.Swap(true) {
			sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := sg.EditShutDown(sctx)
			cancel()
			if err == nil {
				return
			}
			o.Log.Warn("macvm: the guest agent did not shut the edit window's guest down; asking the VM", "err", err.Error())
		}
		if err := wg.RequestStop(); err != nil {
			o.Log.Warn("macvm: the edit window's close did not reach the guest", "err", err.Error())
		}
	}
	bctx, bcancel := context.WithCancel(ctx)
	defer bcancel()
	buttons := &editButtons{ctx: bctx, g: g, staging: filepath.Dir(spec.DiskPath), o: o}
	buttons.wire(&text)
	w, err := scr.Open(ptr, text)
	// The window's block retains the VM; this keeps wg (and so ptr's boot) reachable for the Open call only.
	runtime.KeepAlive(wg)
	if err != nil {
		return fmt.Errorf("macvm: the edit window: %w", err)
	}
	defer func() { bcancel(); buttons.wait(); w.Close() }() // the buttons and the live line end before the window does
	if lw, ok := w.(LiveWindow); ok {
		buttons.watch(lw)
	}
	select {
	case <-wg.Done():
		if err := stopBoot(); err != nil {
			return fmt.Errorf("macvm: the edit window's boot: %w", err)
		}
		if st := wg.State(); st == StateError {
			return errors.New("macvm: the edit window's guest stopped with an error")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// recheck boots the edited copy headless: it replaces the one-time code first, whatever follows, then
// checks that desktop control the base had is kept, then the accounts, Remote Login and the launcher's
// self-test, in that order. The first that fails is an *EditDiscardedError. Right after the code it
// removes the profiles' copies
// (removeProfiles: copied, and staged), and after Remote Login it reads the items back into staging's
// tools.json (ReadBackItems), a failure of either discarding the copy with no line of its own. The reset is
// what staging's AdminPasswordFile holds after it, and what a passing copy's swap carries into the base. It
// returns the desktop mode and, in an EditRecord, what the boot read of the certificates and Full Disk Access
// (reportRead prints it as the boot read it; a read that failed is empty, never trusted or allowed).
func (o *EditOptions) recheck(ctx context.Context, spec MachineSpec, m Manifest, staging string, copied bool, staged []string) (desktop string, read EditRecord, err error) {
	err = o.guest(ctx, spec, func(g EditGuest) error {
		reset, err := randomCode(24)
		if err != nil {
			return err
		}
		defer clear(reset)
		if err := setAdminCode(ctx, g, staging, reset); err != nil {
			return err
		}
		if err := removeProfiles(ctx, g, copied, staged); err != nil {
			return fmt.Errorf("macvm: the staged profiles: %w", err)
		}
		probe := o.Desktop
		if dp, ok := g.(DesktopProbe); ok && probe == nil {
			probe = dp
		}
		if desktop, err = controlCheck(ctx, g, probe); err != nil {
			return err
		}
		// An edit must not lose control the base had; a base that never had it keeps its mode.
		if m.Desktop == DesktopControl && desktop != DesktopControl {
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
		o.warnFileVault(p.FileVault)
		cleanTerminal(ctx, g, o.Log)
		if err := ReadBackItems(ctx, g, staging); err != nil {
			return fmt.Errorf("macvm: the edited base's items read-back: %w", err)
		}
		read = o.reportRead(staging, desktop, p.FullDiskAccess)
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
	return desktop, read, err
}

// desktopWords is a desktop mode as the person reads it: observe is "view only".
func (o *EditOptions) desktopWords(mode string) string {
	if mode == DesktopObserve {
		return "view only"
	}
	return mode
}

// reportRead prints what the recheck's boot read: each ca item in staging's tools.json (ReadBackItems has
// just rewritten it), Full Disk Access as the guest agent answered, and the desktop mode. It returns the
// same as an EditRecord for the status. A certificate that reads neither trusted nor not trusted, and a
// Full Disk Access answer that is neither allowed nor refused, are "could not be read".
func (o *EditOptions) reportRead(staging, desktop, fda string) EditRecord {
	rec, err := ReadToolsRecord(staging)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		o.Log.Warn("macvm: the edited base's tools record was not read for the report", "err", err.Error())
	}
	read := EditRecord{ReadAt: o.Now().UTC(), Certs: map[string]string{}}
	row := func(what, state string) {
		words := map[string]string{ItemTrusted: "trusted", ItemNotTrusted: "not trusted", "allowed": "allowed", "refused": "refused"}[state]
		fmt.Fprintf(o.Out, "  %-27s %s\n", what, cmp.Or(words, "could not be read"))
	}
	fmt.Fprintln(o.Out, "Read from the edited base in this boot:")
	for _, r := range rec.Items {
		if r.Kind != ItemKindCA {
			continue
		}
		if r.State == ItemTrusted || r.State == ItemNotTrusted {
			read.Certs[r.Name] = r.State
		} else {
			read.Certs[r.Name] = ""
		}
		row("certificate "+r.Name, read.Certs[r.Name])
	}
	if fda == "allowed" || fda == "refused" {
		read.FullDiskAccess = fda
	}
	row("Full Disk Access", read.FullDiskAccess)
	fmt.Fprintf(o.Out, "  %-27s %s\n", "desktop", o.desktopWords(desktop))
	return read
}

// printUntrusted prints, for each ca item the base's tools.json reads as not trusted, its name, subject and
// SHA-256 and the command that trusts it once a person signed in as wardyn-admin runs it. Nothing runs it
// for them. With no such item it prints nothing. The subject and hash come from the install list's file for
// that item when the file is the one the base was built from; otherwise the base's recorded fingerprint
// and no subject. The guest agent's signing certificate is never listed: the base does not need to trust it.
func (o *EditOptions) printUntrusted(base string) {
	rec, err := ReadToolsRecord(base)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			o.Log.Warn("macvm: the base's tools record was not read for the certificate list", "err", err.Error())
		}
		return
	}
	colons := func(h string) string {
		if h == "" {
			return "not recorded"
		}
		var b strings.Builder
		for i, c := range strings.ToUpper(h) {
			if i > 0 && i%2 == 0 {
				b.WriteByte(':')
			}
			b.WriteRune(c)
		}
		return b.String()
	}
	sum256 := func(b []byte) string {
		h := sha256.Sum256(b)
		return hex.EncodeToString(h[:])
	}
	header := false
	for _, r := range rec.Items {
		if r.Kind != ItemKindCA || r.State != ItemNotTrusted {
			continue
		}
		if !header {
			fmt.Fprintln(o.Out)
			fmt.Fprintln(o.Out, "Not yet trusted in this base. Click Trust the list's CAs in the window, or run each command in the admin Terminal. Check that the subject and SHA-256 are the ones you expect, then confirm macOS's prompt.")
			header = true
		}
		subject, sum := "not available: the install list's file is not the one this base was built from", r.Fingerprint
		for _, it := range o.Items.Items {
			if it.Kind != ItemKindCA || it.Name != r.Name || it.SHA256 != r.SHA256 {
				continue
			}
			if b, err := os.ReadFile(it.Path); err != nil || sum256(b) != r.SHA256 {
				break
			}
			if cert, der, err := readCA(it.Path); err == nil {
				// A DN can carry a newline or an escape sequence; the person's Terminal reads what is printed.
				subject, sum = strconv.Quote(cert.Subject.String()), sum256(der)
			}
		}
		// launchctl asuser runs security in the console's GUI session, where macOS can show the dialog that
		// approves the trust; from the admin Terminal's su shell alone it is denied (guestagent's trustScript).
		fmt.Fprintf(o.Out, "%s\n  subject  %s\n  SHA-256  %s\n  sudo launchctl asuser $(stat -f %%u /dev/console) security add-trusted-cert -d -r trustRoot -p ssl -k %s %s/%s\n",
			r.Name, subject, colons(sum), guestagent.MacSystemKeychain, guestagent.ItemDir(r.Name), guestagent.ItemCert)
	}
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
	// ReadAt is when the last passing edit's recheck read the two fields below, in that boot (zero: none
	// has, or the record predates them).
	ReadAt time.Time `json:"read_at,omitzero"`
	// Certs is each ca item's state as that boot read it: ItemTrusted, ItemNotTrusted, or "" for one it
	// could not read.
	Certs map[string]string `json:"certs,omitempty"`
	// FullDiskAccess is "allowed" or "refused" for the guest agent as that boot read it, "" when it could
	// not be read (a guest agent that predates the probe included).
	FullDiskAccess string `json:"full_disk_access,omitempty"`
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
