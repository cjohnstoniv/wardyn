// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/version"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// `drive open <name>` looks at a disk drive an agent wrote without letting this Mac's kernel parse it: a
// throwaway guest (macvm.DriveOpen, started as wardyn-macvm drive-open) mounts the drive, and the files
// come over the guest agent's socket as a tar stream that driveOpenExtract turns into plain files.

// kindDriveOpen is a drive open's ledger entry: one VM slot for the guest's life. The CLI releases it
// after its helper exits (driveOpenRelease) and reclaims a dead one's at its next start.
const kindDriveOpen admission.Kind = "drive_open"

const (
	// driveOpenFolder, in the person's home, holds a folder per drive.
	driveOpenFolder = "Wardyn Drives"
	// driveOpenNameFile is the suffix of the file beside a drive's image (<object>.img.name) that the
	// runner's driver writes when a run attaches the drive: JSON {"name": the drive's name, "label": its
	// volume's name}. It is how the name a person types finds the image; keep it equal to the driver's
	// macDiskDrives.
	driveOpenNameFile = ".img.name"
	// driveOpenHelperLog is where wardyn-macvm drive-open logs, in the VM directory.
	driveOpenHelperLog = "drive-open.log"
	// driveOpenCLILog is the CLI's own log, beside it.
	driveOpenCLILog = "drive-open-cli.log"

	driveOpenBackWait = 30 * time.Minute // the write back, once Return is pressed
	driveOpenStopWait = 2 * time.Minute  // the helper's graceful stop, before SIGTERM
	driveOpenSyncWait = time.Minute
)

// newDriveCommand is `wardyn-runnerd drive`: what a person can do with a disk drive a run on this Mac has used.
func newDriveCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "drive", Short: "Open the disk drives that runs on this Mac have used", Args: cobra.NoArgs}
	var deleteGone bool
	open := &cobra.Command{Use: "open <name>", Short: "Copy a drive's files out of a throwaway macOS VM, change them, and write the changes back", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := driveOpenHandle(cmd)
			if err != nil {
				return err
			}
			// The first Ctrl-C ends the soft ctx (nothing more is written back before Return, and a copy in
			// progress is removed); a second ends hard, which every long wait watches, so the command ends
			// and its closed stdin pipe makes the helper stop the VM on its own. main's own handler swallows
			// signals, so this one is the way out.
			ctx, soft := context.WithCancel(cmd.Context())
			defer soft()
			hard, stop := context.WithCancel(context.WithoutCancel(cmd.Context()))
			defer stop()
			sigs, done := make(chan os.Signal, 2), make(chan struct{})
			signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
			defer func() { signal.Stop(sigs); close(done) }()
			go func() {
				select {
				case <-sigs:
				case <-done:
					return
				}
				soft()
				fmt.Fprintln(cmd.ErrOrStderr(), "Stopping. Press Ctrl-C again to stop now.")
				select {
				case <-sigs:
					stop()
				case <-done:
				}
			}()
			return m.driveOpen(ctx, driveOpenArgs{name: args[0], deleteGone: deleteGone, in: cmd.InOrStdin(), out: cmd.OutOrStdout(), hard: hard})
		}}
	open.Flags().BoolVar(&deleteGone, "delete", false, "also delete from the drive the files you deleted from the copy")
	cmd.AddCommand(open)
	return cmd
}

// driveOpenHandle is `macos-vm`'s handle on this Mac's ledger, base and drives, opened as that command opens it.
func driveOpenHandle(cmd *cobra.Command) (*macOSVMs, error) {
	cfg, err := serveConfig(cmd, "")
	if err != nil {
		return nil, err
	}
	stateDir := cfg.get("state_dir")
	if stateDir == "" {
		if stateDir, err = runneridentity.DefaultDir(); err != nil {
			return nil, err
		}
	}
	org, err := macVMOrg(cfg, serviceOrgArg)
	if err != nil {
		return nil, err
	}
	id, err := runneridentity.Load(stateDir, org)
	if err != nil {
		return nil, err
	}
	root, err := macvm.DefaultRoot()
	if err != nil {
		return nil, err
	}
	m := openMacOSVMs(macOSVMSetup{ledgerDir: admission.DefaultDir, root: root, runnerID: id.RunnerID.String(), version: version.String(), uid: os.Getuid(),
		host: admission.SystemHost(), owner: fileOwner, limitsFile: macOSVMLimitsFile(stateDir)})
	m.helper, err = macVMHelperPath()
	return m, err
}

// driveOpenArgs is one `drive open`: in is where the person presses Return, out where the lines go.
type driveOpenArgs struct {
	name       string
	deleteGone bool
	in         io.Reader
	out        io.Writer
	// hard ends the waits that the first Ctrl-C leaves running; nil is never.
	hard context.Context
}

// driveOpen is `drive open`: it copies the drive's files to ~/Wardyn Drives/<name>, waits for Return, and writes
// what changed back. Ctrl-C (ctx ending) before Return writes nothing. The folder stays either way.
func (m *macOSVMs) driveOpen(ctx context.Context, a driveOpenArgs) error {
	err := m.driveOpenRun(ctx, a)
	if err == nil {
		return nil
	}
	return driveOpenError{text: driveOpenSafe(err.Error()), err: err}
}

// driveOpenError is an error whose text is safe to print: whatever a guest, a stream's names or the helper put
// into the original is escaped (driveOpenSafe). The original stays reachable for errors.Is.
type driveOpenError struct {
	text string
	err  error
}

func (e driveOpenError) Error() string { return e.text }
func (e driveOpenError) Unwrap() error { return e.err }

// driveOpenSafe is s with every control byte, escape, CR, newline and invalid byte written out as \x1b-style
// text, cut at 600 characters. The guest is hostile: its stderr, its protocol errors and the names in its stream
// must not reach this terminal as terminal commands or as extra lines.
func driveOpenSafe(s string) string {
	var b strings.Builder
	n := 0
	for i := 0; i < len(s); {
		if n++; n > 600 {
			b.WriteString("...")
			break
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unicode.IsPrint(r):
			b.WriteRune(r)
		default:
			q := strconv.QuoteToASCII(string(r))
			b.WriteString(q[1 : len(q)-1])
		}
		i += size
	}
	return b.String()
}

func (m *macOSVMs) driveOpenRun(ctx context.Context, a driveOpenArgs) (err error) {
	if m.ledger == nil {
		return errors.New(cmp.Or(m.refusal, m.why))
	}
	hard := cmp.Or(a.hard, context.WithoutCancel(ctx))
	m.reconcileDriveOpen()
	object, label, err := m.driveOpenFind(a.name)
	if err != nil {
		return err
	}
	man, err := macvm.ReadyBase(m.layout.BaseDir(0))
	if err != nil {
		//lint:ignore ST1005 the sentence the person reads at the terminal, as written
		return errors.New("This Mac has no ready macOS base to open the drive with. `wardyn-runnerd macos-vm status` shows what it is doing.")
	}
	dest, err := m.openDriveDest(a.name)
	if err != nil {
		return err
	}
	defer dest.close()
	// One line when the drive opens and one when it closes, on this Mac only: they name the drive and the
	// result, beside the helper's own drive-open.log in the VM directory (drive-open-cli.log, rotated the
	// same way). No row reaches the organisation's audit log.
	log, closeLog := m.driveOpenLogger()
	defer closeLog()
	result := "failed"
	defer func() {
		if err != nil {
			result = "failed: " + driveOpenSafe(err.Error())
		}
		log.Info("drive open: closed", "drive", a.name, "result", result)
	}()
	fmt.Fprintf(a.out, "Starting a throwaway macOS VM with the drive %q. It has no internet and runs no agent. This takes about a minute.\n", a.name)
	h, ready, err := m.startDriveOpen(ctx, hard, man, object, label)
	if err != nil {
		dest.undo()
		if ctx.Err() != nil {
			result = "stopped before the drive opened"
			return nil
		}
		return err
	}
	defer func() { h.stop(hard); m.driveOpenRelease(hard, h.entry) }()
	log.Info("drive open: opened", "drive", a.name, "object", object)
	var nd net.Dialer
	agent := guestagent.NewClient(func(ctx context.Context) (net.Conn, error) { return nd.DialContext(ctx, "unix", ready.AgentSock) })
	defer agent.Close()
	result, err = driveOpenWork(ctx, driveOpenJob{agent: agent, dest: dest, limits: dest.limits, deleteGone: a.deleteGone, mark: driveOpenMark, in: a.in, out: a.out, hard: hard})
	return err
}

// driveOpenLogger is the local log of drive opens, beside the helper's: JSON lines in the VM directory's
// drive-open-cli.log. A log that cannot be opened is no reason to refuse the drive, so it falls back to none.
func (m *macOSVMs) driveOpenLogger() (*slog.Logger, func()) {
	f, err := macvm.OpenRotatingLog(filepath.Join(m.layout.Root, driveOpenCLILog), macvm.HelperLogMax)
	if err != nil {
		return slog.New(slog.NewJSONHandler(io.Discard, nil)), func() {}
	}
	return slog.New(slog.NewJSONHandler(f, nil)), func() { _ = f.Close() }
}

// driveOpenFind is the image and volume label of the drive named name (or whose object name it is): only
// a drive a run on this Mac has attached has a name file, so the Mac knows no other.
func (m *macOSVMs) driveOpenFind(name string) (object, label string, err error) {
	entries, _ := os.ReadDir(m.layout.DrivesDir())
	var found []string
	for _, e := range entries {
		obj, ok := strings.CutSuffix(e.Name(), driveOpenNameFile)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		rec := m.driveOpenRecord(obj)
		if rec["label"] == "" || (rec["name"] != name && obj != name) {
			continue
		}
		found, object, label = append(found, obj), obj, rec["label"]
	}
	switch len(found) {
	case 1:
		return object, label, nil
	case 0:
		//lint:ignore ST1005 the sentence the person reads at the terminal, as written
		return "", "", fmt.Errorf("Wardyn does not know a drive named %q on this Mac. drive open opens only a drive that a run on this Mac has used.", name)
	}
	//lint:ignore ST1005 the sentence the person reads at the terminal, as written
	return "", "", fmt.Errorf("More than one drive on this Mac is named %q. Use one of these names instead: %s.", name, strings.Join(found, ", "))
}

// driveOpenRecord is the name file of the drive whose object name is obj, empty when the image or the file is
// missing or not what Wardyn wrote.
func (m *macOSVMs) driveOpenRecord(obj string) map[string]string {
	img, err := m.layout.DriveImage(obj)
	if err != nil {
		return nil
	}
	if fi, err := os.Lstat(img); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	file := filepath.Join(m.layout.DrivesDir(), obj+driveOpenNameFile)
	fi, err := os.Lstat(file)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return nil
	}
	b, err := os.ReadFile(file)
	var rec map[string]string
	if err != nil || json.Unmarshal(b, &rec) != nil {
		return nil
	}
	return rec
}

// driveOpenDest is the empty folder a drive's files are copied into, ~/Wardyn Drives/<name>, and what to
// do when the copy fails. Nothing in it or above it is followed if it is a link.
type driveOpenDest struct {
	root, parent *os.Root
	lock         *os.File // flock on the destination's lock file in the parent, held for the session
	leaf, shown  string
	created      bool
	trash        string // the person's Trash folder
	limits       driveOpenLimits
}

func (d *driveOpenDest) close() {
	_ = d.root.Close()
	_ = d.parent.Close()
	if d.lock != nil {
		_ = d.lock.Close() // the flock goes with the file
	}
}

// undo removes the folder's contents, which this run alone can have written (it holds the lock and found
// the folder empty, and the person has not been asked to touch it yet), and the folder if this run made it.
// It runs after a failed copy; what it cannot remove stays.
func (d *driveOpenDest) undo() {
	left, _ := fs.ReadDir(d.root.FS(), ".")
	for _, e := range left {
		_ = d.root.RemoveAll(e.Name())
	}
	if d.created {
		_ = d.parent.Remove(d.leaf)
	}
}

// toTrash moves the whole folder into the Trash with one rename, so a write that lands late is in the moved
// folder and can be got back, not lost. It never deletes. A folder that was there before the run is not moved.
func (d *driveOpenDest) toTrash() error {
	if !d.created {
		return errors.New("the folder was there before this run")
	}
	if err := os.Mkdir(d.trash, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return os.Rename(d.shown, filepath.Join(d.trash, d.leaf+" "+time.Now().UTC().Format("2006-01-02T150405Z")))
}

// finish ends a session that is over for the folder: it moves it to the Trash, or keeps it, and says which.
func (d *driveOpenDest) finish(out io.Writer) {
	if d.toTrash() != nil {
		fmt.Fprintf(out, "The folder %s is kept; delete it when you're done.\n", d.shown)
		return
	}
	fmt.Fprintf(out, "The folder %s was moved to the Trash.\n", d.shown)
}

// driveOpenSent is what the folder held when the write back began: its regular files as they were (size,
// time, identity) and its folders. Whatever differs afterwards was changed or made after it was sent.
type driveOpenSent struct {
	files map[string]fs.FileInfo
	dirs  map[string]bool
}

// sent records the folder before the pack; a change in between is sent and reads as changed here, so is kept.
func (d *driveOpenDest) sent() (driveOpenSent, error) {
	s := driveOpenSent{files: map[string]fs.FileInfo{}, dirs: map[string]bool{}}
	err := fs.WalkDir(d.root.FS(), ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil || p == "." {
			return err
		}
		fi, err := d.root.Lstat(p)
		if fi != nil && fi.IsDir() {
			s.dirs[p] = true
		} else if fi != nil && fi.Mode().IsRegular() {
			s.files[p] = fi
		}
		return err
	})
	return s, err
}

// changed is how many items under dir differ from what was sent: a file that differs or is new, and a folder
// made since (as one item). Nothing is removed here.
func (d *driveOpenDest) changed(s driveOpenSent, dir string) (n int) {
	entries, err := fs.ReadDir(d.root.FS(), dir)
	if err != nil {
		return 1
	}
	for _, e := range entries {
		p := path.Join(dir, e.Name())
		fi, err := d.root.Lstat(p)
		was := s.files[p]
		switch {
		case err != nil:
			n++
		case fi.IsDir() && s.dirs[p]:
			n += d.changed(s, p)
		case was != nil && fi.Mode().IsRegular() && fi.Size() == was.Size() && fi.ModTime().Equal(was.ModTime()) && os.SameFile(fi, was):
		default:
			n++
		}
	}
	return n
}

// finishSent is finish after a write back that sent everything: a folder that changed after the send stays.
func (d *driveOpenDest) finishSent(out io.Writer, s driveOpenSent) {
	if n := d.changed(s, "."); n > 0 {
		fmt.Fprintf(out, "The folder %s is kept: %s in it changed after it was sent and did not go back.\n", d.shown, driveOpenCount(n, "item", "items"))
		return
	}
	d.finish(out)
}

// openDriveDest makes or checks the folder for the drive named name, and sets the copy's limits from the
// room on this Mac. A symlink at either step, or a folder that holds anything, is refused.
func (m *macOSVMs) openDriveDest(name string) (*driveOpenDest, error) {
	leaf := strings.Map(func(r rune) rune {
		if r == '/' || r == ':' || r < ' ' || r == 0x7f {
			return '-'
		}
		return r
	}, name)
	if leaf = strings.TrimSpace(leaf); leaf == "" || leaf == "." || leaf == ".." || len(leaf) > driveOpenMaxName-12 {
		//lint:ignore ST1005 the sentence the person reads at the terminal, as written
		return nil, fmt.Errorf("The drive's name %q cannot be a folder name.", name)
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		//lint:ignore ST1005 the sentence the person reads at the terminal, as written
		return nil, errors.New("This account's home folder is not known, so there is nowhere to copy the drive to.")
	}
	free, err := m.host.FreeDiskBytes(home)
	if err != nil {
		return nil, err
	}
	if free-macOSVMDiskFloor <= 0 {
		//lint:ignore ST1005 the sentence the person reads at the terminal, as written
		return nil, fmt.Errorf("This Mac has %s free, too little to copy a drive onto.", driveOpenSize(free))
	}
	top, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	defer top.Close()
	parent, _, err := driveOpenSubRoot(top, driveOpenFolder, filepath.Join(home, driveOpenFolder))
	if err != nil {
		return nil, err
	}
	shown := filepath.Join(home, driveOpenFolder, leaf)
	// Two drive opens of one name must not meet: the second would find the folder empty while the first
	// boots, and its failure would remove the first's files. The lock lives beside the folder, not in it.
	lock, err := parent.OpenFile("."+leaf+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err == nil {
		if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			_ = lock.Close()
			//lint:ignore ST1005 the sentence the person reads at the terminal, as written
			err = fmt.Errorf("Another drive open of %q is running on this Mac and has %s. Finish it, or press Ctrl-C there, then run drive open again.", name, shown)
		}
	}
	if err != nil {
		_ = parent.Close()
		return nil, err
	}
	root, created, err := driveOpenSubRoot(parent, leaf, shown)
	if err != nil {
		_ = lock.Close()
		_ = parent.Close()
		return nil, err
	}
	d := &driveOpenDest{root: root, parent: parent, lock: lock, leaf: leaf, shown: shown, created: created, trash: filepath.Join(home, ".Trash"),
		limits: driveOpenLimits{entries: driveOpenMaxEntries, bytes: free - macOSVMDiskFloor}}
	if left, _ := fs.ReadDir(root.FS(), "."); len(left) > 0 {
		d.close()
		//lint:ignore ST1005 the sentence the person reads at the terminal, as written
		return nil, fmt.Errorf("%s already holds files. Move or delete them, then run drive open again: Wardyn copies a drive only into an empty folder.", shown)
	}
	return d, nil
}

// driveOpenSubRoot opens the folder name inside r, making it (private) if it is not there. A link, or
// anything that is not a folder, is refused; shown is the folder's path in the words.
func driveOpenSubRoot(r *os.Root, name, shown string) (sub *os.Root, created bool, err error) {
	fi, err := r.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err = r.Mkdir(name, 0o700); err != nil {
			return nil, false, err
		}
		created = true
	case err != nil:
		return nil, false, err
	case fi.Mode()&fs.ModeSymlink != 0:
		//lint:ignore ST1005 the sentence the person reads at the terminal, as written
		return nil, false, fmt.Errorf("%s is a link. Wardyn copies a drive only into a folder it can see is a real one: remove the link or move it away, then run drive open again.", shown)
	case !fi.IsDir():
		//lint:ignore ST1005 the sentence the person reads at the terminal, as written
		return nil, false, fmt.Errorf("%s is not a folder. Move it away, then run drive open again.", shown)
	}
	sub, err = r.OpenRoot(name)
	return sub, created, err
}

// driveOpenHelper is a started wardyn-macvm drive-open.
type driveOpenHelper struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	exited chan struct{}
	entry  string // the ledger entry the CLI took
}

// startDriveOpen takes a ledger entry, starts the helper in its own session with a pipe on its stdin, and
// waits for its ready line. The helper stops when that pipe ends. Without a ready line it answers the
// helper's last stderr line (a refusal, a drive in use among them) and releases the entry.
func (m *macOSVMs) startDriveOpen(ctx, hard context.Context, man macvm.Manifest, object, label string) (*driveOpenHelper, macvm.DriveOpenReady, error) {
	var ready macvm.DriveOpenReady
	if _, err := m.ledger.Sweep(); err != nil {
		return nil, ready, err
	}
	self := os.Getpid()
	start, err := admission.Self(m.host, self)
	if err != nil {
		return nil, ready, err
	}
	cpus, memMiB := max(man.MinCPUs, 1), m.guestMemory(man)>>20
	e, err := m.ledger.Admit(admission.Request{Kind: kindDriveOpen, GuestCPUs: cpus, GuestMemory: memMiB << 20,
		DiskReserve: admission.RunDiskReservation(cmp.Or(man.DiskBytes, macvm.BaseDiskBytes)), OwnerPID: self, OwnerStart: start})
	if err != nil {
		return nil, ready, err
	}
	never := func(err error) error {
		return errors.Join(err, m.ledger.Release(e.ID, admission.Destroyed{Stopped: true}))
	}
	args := []string{macvm.DriveOpenCommand, "--" + macvm.FlagRoot, m.layout.Root, "--" + macvm.FlagCPUs, strconv.Itoa(cpus),
		"--" + macvm.FlagMemoryMiB, strconv.FormatUint(memMiB, 10), "--" + macvm.FlagRunnerID, m.runnerID, "--" + macvm.FlagLedgerEntry, e.ID,
		"--" + macvm.FlagDrive, object, "--" + macvm.FlagDriveLabel, label}
	if m.ledgerDir != "" {
		args = append(args, "--"+macvm.FlagLedgerDir, m.ledgerDir)
	}
	if m.sockRoot != "" {
		args = append(args, "--"+macvm.FlagSockRoot, m.sockRoot)
	}
	cmd := exec.Command(m.helper, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	tail := &driveOpenTail{}
	cmd.Stderr = tail
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, ready, never(err)
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, ready, never(err)
	}
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		_, _ = pr.Close(), pw.Close()
		return nil, ready, never(fmt.Errorf("start the macOS VM helper: %w", err))
	}
	_ = pw.Close()
	h := &driveOpenHelper{cmd: cmd, stdin: stdin, exited: make(chan struct{}), entry: e.ID}
	go func() { _ = cmd.Wait(); close(h.exited) }()
	lines := make(chan macvm.DriveOpenReady, 1)
	go func() {
		defer pr.Close()
		for sc := bufio.NewScanner(pr); sc.Scan(); {
			var r macvm.DriveOpenReady
			if json.Unmarshal(sc.Bytes(), &r) == nil && r.AgentSock != "" {
				lines <- r
				break
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	select {
	case ready = <-lines:
		return h, ready, nil
	case <-h.exited:
		m.driveOpenRelease(hard, e.ID)
		return nil, ready, errors.New(cmp.Or(tail.last(), "the macOS VM helper ended without opening the drive; see "+filepath.Join(m.layout.Root, driveOpenHelperLog)))
	case <-ctx.Done():
		h.stop(hard)
		m.driveOpenRelease(hard, e.ID)
		return nil, ready, ctx.Err()
	}
}

// stop ends the helper: stdin's end makes it stop the VM gracefully; a helper still there after
// driveOpenStopWait gets SIGTERM, and a kill 30 seconds after that. The waits end with hard, the second
// Ctrl-C: the helper then stops on its own, as its stdin is closed.
func (h *driveOpenHelper) stop(hard context.Context) {
	_ = h.stdin.Close()
	select {
	case <-h.exited:
		return
	case <-hard.Done():
		return
	case <-time.After(driveOpenStopWait):
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-h.exited:
	case <-hard.Done():
	case <-time.After(30 * time.Second):
		_ = h.cmd.Process.Kill()
		<-h.exited
	}
}

// driveOpenRelease frees a finished drive open's ledger entry: an entry its helper claimed through
// ReclaimOwn (which waits out a VM process still going down), one it never claimed through Release. A
// failure is logged; the next drive open's reconcileDriveOpen tries again.
func (m *macOSVMs) driveOpenRelease(hard context.Context, id string) {
	counted, err := m.ledger.Counted()
	if err == nil && !slices.ContainsFunc(counted, func(e admission.Entry) bool { return e.ID == id }) {
		return // serve's reconcile got there first
	}
	claimed := slices.ContainsFunc(counted, func(e admission.Entry) bool { return e.ID == id && e.HelperPID != 0 })
	if !claimed {
		if err := m.ledger.Release(id, admission.Destroyed{Stopped: true}); err != nil {
			slog.Warn("macOS VM ledger: release a drive open that never started", "error", err.Error())
		}
		return
	}
	for try := 0; ; try++ {
		if err = m.ledger.ReclaimOwn(id, ""); err == nil {
			return
		}
		if try == 9 {
			slog.Warn("macOS VM ledger: release a finished drive open", "error", err.Error())
			return
		}
		select {
		case <-hard.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// reconcileDriveOpen releases this runner's drive open entries whose helper (or CLI, before a claim) is gone.
func (m *macOSVMs) reconcileDriveOpen() {
	for _, e := range m.own(kindDriveOpen) {
		if !m.vmGone(e) {
			continue
		}
		if err := m.ledger.ReclaimOwn(e.ID, ""); err != nil {
			slog.Warn("macOS VM ledger: release an earlier drive open", "error", err.Error())
		}
	}
}

// driveOpenTail keeps the end of what a helper wrote to stderr.
type driveOpenTail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *driveOpenTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	t.buf = t.buf[max(len(t.buf)-4096, 0):]
	return len(p), nil
}

// last is the helper's last line, without its prefix: its refusal.
func (t *driveOpenTail) last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.buf)), "\n")
	return driveOpenSafe(strings.TrimPrefix(lines[len(lines)-1], helperFailurePrefix))
}

// driveOpenJob is what the guest session needs. hard is the second Ctrl-C's ctx (nil: never).
type driveOpenJob struct {
	agent      *guestagent.Client
	dest       *driveOpenDest
	limits     driveOpenLimits
	deleteGone bool
	mark       func(*os.File) error
	in         io.Reader
	out        io.Writer
	hard       context.Context
}

// The session's results, as the local log says them.
const (
	driveOpenCopyFailed    = "copy failed"
	driveOpenStopped       = "stopped before the copy finished"
	driveOpenDiscarded     = "discarded"
	driveOpenWrittenBack   = "written back"
	driveOpenBackFailed    = "write back failed"
	driveOpenNamesShown    = 10
	driveOpenUnreadShown   = 5
	driveOpenStderrKept    = 64 << 10
	driveOpenStderrMessage = 2048
)

// driveOpenWork copies the drive out, waits for Return, and writes the changes back through the guest
// agent's Exec; the result is for the log. A guest that has the drive is synced before it returns, so the
// caller can end the VM. A copy the guest's tar could not finish reading (an unreadable file) is kept, and
// the person is told how many items are missing.
func driveOpenWork(ctx context.Context, j driveOpenJob) (result string, err error) {
	j.hard = cmp.Or(j.hard, context.WithoutCancel(ctx))
	defer func() {
		sctx, cancel := context.WithTimeout(j.hard, driveOpenSyncWait)
		defer cancel()
		if err := j.run(sctx, nil, "/bin/sync"); err != nil {
			fmt.Fprintf(j.out, "The throwaway VM did not confirm it had written the drive (%s).\n", driveOpenSafe(err.Error()))
		}
	}()
	c, err := j.copyOut(ctx)
	if err != nil {
		j.dest.undo()
		if ctx.Err() != nil {
			fmt.Fprintln(j.out, "Stopped before the copy finished. Nothing was kept and nothing was written back.")
			return driveOpenStopped, nil
		}
		return driveOpenCopyFailed, err
	}
	fmt.Fprintln(j.out, strings.TrimSpace(fmt.Sprintf("Copied %s (%s) to %s. %s", driveOpenCount(c.files, "file", "files"), driveOpenSize(c.bytes), j.dest.shown, c.skipped.text())))
	if c.unread > 0 {
		fmt.Fprintf(j.out, "%s could not be read from the drive and %s not in this copy. The throwaway VM said:\n", driveOpenCount(c.unread, "item", "items"), map[bool]string{true: "is", false: "are"}[c.unread == 1])
		for _, line := range c.unreadText {
			fmt.Fprintf(j.out, "  %s\n", line)
		}
		fmt.Fprintln(j.out, "Write back will not replace, and --delete will not remove, anything this copy does not hold.")
	}
	fmt.Fprintln(j.out, "Every file is marked as downloaded and has no execute permission.")
	fmt.Fprintln(j.out)
	fmt.Fprintln(j.out, "Change the files, then press Return to write them back, or press Ctrl-C to discard your changes.")
	if !driveOpenReturn(ctx, j.in) {
		fmt.Fprintln(j.out, "Discarded: nothing was written back.")
		j.dest.finish(j.out)
		return driveOpenDiscarded, nil
	}
	sent, err := j.dest.sent()
	var unsent int
	if err == nil {
		unsent, err = j.writeBack(c)
	}
	switch {
	case err != nil:
		// The folder holds the only copy of the changes that did not go back.
		fmt.Fprintf(j.out, "The folder %s is kept: it holds the changes that did not go back.\n", j.dest.shown)
		return driveOpenBackFailed, err
	case unsent > 0:
		// Links, devices, pipes and set-id files are never sent; removing the folder would lose them.
		fmt.Fprintf(j.out, "The folder %s is kept: %s in it could not be written back.\n", j.dest.shown, driveOpenCount(unsent, "item", "items"))
	default:
		j.dest.finishSent(j.out, sent)
	}
	return driveOpenWrittenBack, nil
}

// driveOpenReturn waits for a line on in. Ctrl-C (ctx ending) or an input that ends first is false.
func driveOpenReturn(ctx context.Context, in io.Reader) bool {
	got := make(chan bool, 1)
	go func() { _, err := bufio.NewReader(in).ReadString('\n'); got <- err == nil }()
	select {
	case ok := <-got:
		return ok
	case <-ctx.Done():
		return false
	}
}

// copyOut runs tar in the guest as the agent user and extracts its stream. A tar that read the drive's
// listing to its end but exited with an error left some items unread; the copy is kept, with the count.
func (j driveOpenJob) copyOut(ctx context.Context) (*driveOpenCopy, error) {
	s, err := j.agent.Exec(ctx, guestagent.ExecRequest{Argv: driveOpenTarArgv(), Env: []string{"COPYFILE_DISABLE=1"}})
	if err != nil {
		return nil, fmt.Errorf("the throwaway VM did not start a copy: %w", err)
	}
	defer s.Close()
	stderr := driveOpenDrain(s.Stderr)
	c, err := driveOpenExtract(s.Stdout, j.dest.root, j.limits, j.mark)
	if err != nil {
		return nil, err
	}
	// The listing ended; what follows is padding. A guest that keeps sending is not finishing.
	if n, _ := io.Copy(io.Discard, io.LimitReader(s.Stdout, 1<<20)); n == 1<<20 {
		return nil, errors.New("the throwaway VM kept sending after the drive's listing ended")
	}
	code, err := s.Wait()
	said := <-stderr
	if err != nil {
		return nil, fmt.Errorf("the throwaway VM could not read the whole drive (%w%s)", err, said.msg())
	}
	if code != 0 {
		said0 := len(said.lines)
		if said.more && said0 > 0 {
			said.lines = said.lines[:said0-1] // the last kept line was cut where the 64 KiB ended
		}
		said.lines = driveOpenUnreadLines(said.lines)
		switch {
		case len(said.lines) > 0:
			c.unread, c.unreadText = len(said.lines), said.shown(driveOpenUnreadShown)
		case said0 == 0:
			c.unread, c.unreadText = 1, []string{fmt.Sprintf("tar ended with code %d without saying why", code)}
		case said.more:
			c.unread, c.unreadText = 1, []string{"tar said more than was kept"}
		}
		// else: every line was about the volume's own folders or extended attributes, which are not items of the drive.
	}
	return c, nil
}

// driveOpenTarExcluded are the volume's own folders at the drive's root. They are not the person's files, and
// the guest's tar cannot read some of them.
var driveOpenTarExcluded = []string{".Trashes", ".Spotlight-V100", ".fseventsd", ".TemporaryItems"}

// driveOpenTarArgv is the guest's tar (macOS bsdtar) that lists the drive: no extended attributes, no Mac metadata
// (--no-xattrs alone still runs the copyfile path), and none of driveOpenTarExcluded.
func driveOpenTarArgv() []string {
	argv := []string{"/usr/bin/tar", "-C", macvm.DriveOpenTarget, "-cf", "-", "--no-xattrs", "--no-mac-metadata"}
	for _, name := range driveOpenTarExcluded {
		argv = append(argv, "--exclude", "./"+name)
	}
	return append(argv, ".")
}

// driveOpenQuoteMeta is names with their regexp metacharacters escaped.
func driveOpenQuoteMeta(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = regexp.QuoteMeta(n)
	}
	return out
}

// driveOpenTarNoise matches a tar complaint that names only one of driveOpenTarExcluded at the drive's root.
var driveOpenTarNoise = regexp.MustCompile(`(^|[\s:])\./(` + strings.Join(driveOpenQuoteMeta(driveOpenTarExcluded), "|") + `)(/\S*)?(:|$)`)

// driveOpenTarXattr matches a complaint whose message segment says an operation on extended attributes failed
// ("Couldn't list extended attributes", "Could not pack extended attributes"), with at most bsdtar's ": <strerror>"
// after it. Anchoring on the segment after ": " keeps a path that holds the words ("./xattr-notes.txt: Cannot open",
// "./Couldn't read xattr.txt: Cannot open") from matching.
var driveOpenTarXattr = regexp.MustCompile(`(?i): (couldn't|could not|cannot|can't|failed to|unable to) [^:]*\b(extended attributes?|xattrs?)\b[^:]*(: [^:]*)?$`)

// driveOpenUnreadLines is lines without the ones that are not unread items of the drive: a complaint about
// one of driveOpenTarExcluded, and one about extended attributes.
func driveOpenUnreadLines(lines []string) []string {
	var kept []string
	for _, l := range lines {
		if driveOpenTarNoise.MatchString(l) || driveOpenTarXattr.MatchString(l) {
			continue
		}
		kept = append(kept, l)
	}
	return kept
}

// writeBack sends the new and changed files to the guest, where tar writes them as the agent user. The
// archives are made whole first, so an error while they are made writes nothing. Files the copy held go in
// one stream that overwrites; files and folders it did not hold go in another that never overwrites (tar
// -k), so a name the copy never held, an unreadable item among them, is never replaced. With deleteGone,
// the copied files deleted here are removed from the drive, and no other name is. An error says which
// changes did not go back.
func (j driveOpenJob) writeBack(c *driveOpenCopy) (unsent int, err error) {
	ctx, cancel := context.WithTimeout(j.hard, driveOpenBackWait)
	defer cancel()
	var archives [2]*os.File
	for i := range archives {
		f, err := os.CreateTemp("", "wardyn-drive-*.tar")
		if err != nil {
			return 0, err
		}
		archives[i] = f
		defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	}
	back, err := driveOpenPack(j.dest.root, c, archives[0], archives[1])
	if err != nil {
		return 0, fmt.Errorf("read the changed files: %w", err)
	}
	send := func(f *os.File, names []string, argv ...string) error {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := j.run(ctx, f, argv...); err != nil {
			return fmt.Errorf("write the changed files back: %w. These changes did not go back, or are not confirmed: %s", err, driveOpenNames(names))
		}
		return nil
	}
	if len(back.known) > 0 {
		if err := send(archives[0], slices.Concat(back.known, back.fresh), "/usr/bin/tar", "-C", macvm.DriveOpenTarget, "-xf", "-"); err != nil {
			return 0, err
		}
	}
	if len(back.fresh)+back.newDirs > 0 {
		if err := send(archives[1], back.fresh, "/usr/bin/tar", "-C", macvm.DriveOpenTarget, "-k", "-xf", "-"); err != nil {
			return 0, err
		}
	}
	if gone := back.gone; j.deleteGone && len(gone) > 0 {
		names := "./" + strings.Join(gone, "\x00./") + "\x00"
		if err := j.run(ctx, strings.NewReader(names), "/bin/sh", "-c", `cd "$1" && exec /usr/bin/xargs -0 /bin/rm -f --`, "sh", macvm.DriveOpenTarget); err != nil {
			return 0, fmt.Errorf("delete the removed files from the drive: %w. These deletions did not go through, or are not confirmed: %s", err, driveOpenNames(gone))
		}
	}
	fmt.Fprintln(j.out, strings.TrimSpace(fmt.Sprintf("Wrote back %s. The next run that uses this drive sees them, owned by the agent. %s",
		driveOpenCount(back.changed(), "changed file", "changed files"), back.skipped.text())))
	for _, n := range back.skipped {
		unsent += n
	}
	return unsent, nil
}

// driveOpenNames lists up to driveOpenNamesShown names, each quoted, and how many more there are.
func driveOpenNames(names []string) string {
	var quoted []string
	for _, n := range names[:min(len(names), driveOpenNamesShown)] {
		quoted = append(quoted, strconv.Quote(n))
	}
	list := strings.Join(quoted, ", ")
	if more := len(names) - len(quoted); more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return list
}

// run is one command in the guest as the agent user, with stdin from in (nil: none), and its failure
// carrying the end of its stderr.
func (j driveOpenJob) run(ctx context.Context, in io.Reader, argv ...string) error {
	s, err := j.agent.Exec(ctx, guestagent.ExecRequest{Argv: argv})
	if err != nil {
		return err
	}
	defer s.Close()
	stderr := driveOpenDrain(s.Stderr)
	go func() { _, _ = io.Copy(io.Discard, s.Stdout) }()
	var sendErr error
	if in != nil {
		_, sendErr = io.Copy(s.Stdin, in)
	}
	// The end-of-input frame can lose a race with the exit it causes (the pipe closes under its empty
	// payload), so its error says nothing; the exit code does.
	_ = s.Stdin.Close()
	code, err := s.Wait()
	if err != nil || code != 0 {
		return fmt.Errorf("%w%s", cmp.Or(err, fmt.Errorf("%s ended with code %d", argv[0], code)), (<-stderr).msg())
	}
	return sendErr
}

// driveOpenStderr is what a guest command wrote to stderr: the first part of it, the non-empty lines in that
// part, and whether there was more.
type driveOpenStderr struct {
	text  string
	lines []string
	more  bool
}

// msg is ": <text>" for an error message, escaped and bounded, "" when the guest said nothing.
func (e driveOpenStderr) msg() string {
	text := strings.TrimSpace(e.text)
	if text == "" {
		return ""
	}
	return ": " + driveOpenSafe(text[:min(len(text), driveOpenStderrMessage)])
}

// shown is the first n lines, escaped.
func (e driveOpenStderr) shown(n int) []string {
	var out []string
	for _, l := range e.lines[:min(len(e.lines), n)] {
		out = append(out, driveOpenSafe(l))
	}
	return out
}

// driveOpenDrain reads r to its end, which a session needs or its demux stalls, keeping the first 64 KiB. Its
// lines leave out tar's own closing complaint, so their count is the items it could not read.
func driveOpenDrain(r io.Reader) <-chan driveOpenStderr {
	out := make(chan driveOpenStderr, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(r, driveOpenStderrKept))
		n, _ := io.Copy(io.Discard, r)
		e := driveOpenStderr{text: string(bytes.TrimSpace(b)), more: n > 0}
		for line := range strings.SplitSeq(e.text, "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.Contains(line, "Error exit delayed from previous errors") {
				e.lines = append(e.lines, line)
			}
		}
		out <- e
		close(out)
	}()
	return out
}
