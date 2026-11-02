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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

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
			return m.driveOpen(cmd.Context(), driveOpenArgs{name: args[0], deleteGone: deleteGone, in: cmd.InOrStdin(), out: cmd.OutOrStdout()})
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

func (m *macOSVMs) driveOpenRun(ctx context.Context, a driveOpenArgs) error {
	if m.ledger == nil {
		return errors.New(cmp.Or(m.refusal, m.why))
	}
	m.reconcileDriveOpen()
	object, label, err := m.driveOpenFind(a.name)
	if err != nil {
		return err
	}
	man, err := macvm.ReadyBase(m.layout.BaseDir(0))
	if err != nil {
		return errors.New("This Mac has no ready macOS base to open the drive with. `wardyn-runnerd macos-vm status` shows what it is doing.")
	}
	dest, err := m.openDriveDest(a.name)
	if err != nil {
		return err
	}
	defer dest.close()
	fmt.Fprintf(a.out, "Starting a throwaway macOS VM with the drive %q. It has no internet and runs no agent. This takes about a minute.\n", a.name)
	h, ready, err := m.startDriveOpen(ctx, man, object, label)
	if err != nil {
		dest.undo()
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer func() { h.stop(); m.driveOpenRelease(h.entry) }()
	var nd net.Dialer
	agent := guestagent.NewClient(func(ctx context.Context) (net.Conn, error) { return nd.DialContext(ctx, "unix", ready.AgentSock) })
	defer agent.Close()
	return driveOpenWork(ctx, driveOpenJob{agent: agent, dest: dest, limits: dest.limits, deleteGone: a.deleteGone, mark: driveOpenMark, in: a.in, out: a.out})
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
		return "", "", fmt.Errorf("Wardyn does not know a drive named %q on this Mac. drive open opens only a drive that a run on this Mac has used.", name)
	}
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
	leaf, shown  string
	created      bool
	limits       driveOpenLimits
}

func (d *driveOpenDest) close() {
	_ = d.root.Close()
	_ = d.parent.Close()
}

// undo removes what a failed copy left: the folder's contents, and the folder if the copy made it.
func (d *driveOpenDest) undo() {
	left, _ := fs.ReadDir(d.root.FS(), ".")
	for _, e := range left {
		_ = d.root.RemoveAll(e.Name())
	}
	if d.created {
		_ = d.parent.Remove(d.leaf)
	}
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
	if leaf = strings.TrimSpace(leaf); leaf == "" || leaf == "." || leaf == ".." || len(leaf) > driveOpenMaxName {
		return nil, fmt.Errorf("The drive's name %q cannot be a folder name.", name)
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, errors.New("This account's home folder is not known, so there is nowhere to copy the drive to.")
	}
	free, err := m.host.FreeDiskBytes(home)
	if err != nil {
		return nil, err
	}
	if free-macOSVMDiskFloor <= 0 {
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
	root, created, err := driveOpenSubRoot(parent, leaf, shown)
	if err != nil {
		_ = parent.Close()
		return nil, err
	}
	d := &driveOpenDest{root: root, parent: parent, leaf: leaf, shown: shown, created: created,
		limits: driveOpenLimits{entries: driveOpenMaxEntries, bytes: free - macOSVMDiskFloor}}
	if left, _ := fs.ReadDir(root.FS(), "."); len(left) > 0 {
		d.close()
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
		return nil, false, fmt.Errorf("%s is a link. Wardyn copies a drive only into a folder it can see is a real one: remove the link or move it away, then run drive open again.", shown)
	case !fi.IsDir():
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
func (m *macOSVMs) startDriveOpen(ctx context.Context, man macvm.Manifest, object, label string) (*driveOpenHelper, macvm.DriveOpenReady, error) {
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
		m.driveOpenRelease(e.ID)
		return nil, ready, errors.New(cmp.Or(tail.last(), "the macOS VM helper ended without opening the drive; see "+filepath.Join(m.layout.Root, driveOpenHelperLog)))
	case <-ctx.Done():
		h.stop()
		m.driveOpenRelease(e.ID)
		return nil, ready, ctx.Err()
	}
}

// stop ends the helper: stdin's end makes it stop the VM gracefully; a helper still there after
// driveOpenStopWait gets SIGTERM, and a kill 30 seconds after that.
func (h *driveOpenHelper) stop() {
	_ = h.stdin.Close()
	select {
	case <-h.exited:
		return
	case <-time.After(driveOpenStopWait):
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-h.exited:
	case <-time.After(30 * time.Second):
		_ = h.cmd.Process.Kill()
		<-h.exited
	}
}

// driveOpenRelease frees a finished drive open's ledger entry: an entry its helper claimed through
// ReclaimOwn (which waits out a VM process still going down), one it never claimed through Release. A
// failure is logged; the next drive open's reconcileDriveOpen tries again.
func (m *macOSVMs) driveOpenRelease(id string) {
	counted, err := m.ledger.Counted()
	claimed := err == nil && func() bool {
		for _, e := range counted {
			if e.ID == id {
				return e.HelperPID != 0
			}
		}
		return false
	}()
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
		time.Sleep(time.Second)
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

// driveOpenJob is what the guest session needs.
type driveOpenJob struct {
	agent      *guestagent.Client
	dest       *driveOpenDest
	limits     driveOpenLimits
	deleteGone bool
	mark       func(*os.File) error
	in         io.Reader
	out        io.Writer
}

// driveOpenWork copies the drive out, waits for Return, and writes the changes back through the guest
// agent's Exec. A guest that has the drive is synced before it returns, so the caller can end the VM.
func driveOpenWork(ctx context.Context, j driveOpenJob) error {
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), driveOpenSyncWait)
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
			return nil
		}
		return err
	}
	fmt.Fprintln(j.out, strings.TrimSpace(fmt.Sprintf("Copied %s (%s) to %s. %s", driveOpenCount(c.files, "file", "files"), driveOpenSize(c.bytes), j.dest.shown, c.skipped.text())))
	fmt.Fprintln(j.out, "Every file is marked as downloaded and has no execute permission.")
	fmt.Fprintln(j.out)
	fmt.Fprintln(j.out, "Change the files, then press Return to write them back, or press Ctrl-C to discard your changes.")
	if !driveOpenReturn(ctx, j.in) {
		fmt.Fprintln(j.out, "Discarded: nothing was written back.")
		return nil
	}
	return j.writeBack(ctx, c)
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

// copyOut runs tar in the guest as the agent user and extracts its stream.
func (j driveOpenJob) copyOut(ctx context.Context) (*driveOpenCopy, error) {
	s, err := j.agent.Exec(ctx, guestagent.ExecRequest{Argv: []string{"/usr/bin/tar", "-C", macvm.DriveOpenTarget, "-cf", "-", "."}, Env: []string{"COPYFILE_DISABLE=1"}})
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
	if code, err := s.Wait(); err != nil || code != 0 {
		return nil, fmt.Errorf("the throwaway VM could not read the whole drive (%w%s)", cmp.Or(err, fmt.Errorf("tar ended with code %d", code)), <-stderr)
	}
	return c, nil
}

// writeBack sends the new and changed files to the guest, where tar writes them as the agent user. The
// archive is made whole first, so an error while it is made writes nothing; with deleteGone, the files
// deleted from the copy are removed from the drive.
func (j driveOpenJob) writeBack(ctx context.Context, c *driveOpenCopy) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), driveOpenBackWait)
	defer cancel()
	archive, err := os.CreateTemp("", "wardyn-drive-*.tar")
	if err != nil {
		return err
	}
	defer func() { _ = archive.Close(); _ = os.Remove(archive.Name()) }()
	back, err := driveOpenPack(j.dest.root, c, archive)
	if err != nil {
		return fmt.Errorf("read the changed files: %w", err)
	}
	if back.changed+back.newDirs > 0 {
		if _, err := archive.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := j.run(ctx, archive, "/usr/bin/tar", "-C", macvm.DriveOpenTarget, "-xf", "-"); err != nil {
			return fmt.Errorf("write the changed files back: %w", err)
		}
	}
	if gone := back.gone; j.deleteGone && len(gone) > 0 {
		names := "./" + strings.Join(gone, "\x00./") + "\x00"
		if err := j.run(ctx, strings.NewReader(names), "/bin/sh", "-c", `cd "$1" && exec /usr/bin/xargs -0 /bin/rm -f --`, "sh", macvm.DriveOpenTarget); err != nil {
			return fmt.Errorf("delete the removed files from the drive: %w", err)
		}
	}
	fmt.Fprintln(j.out, strings.TrimSpace(fmt.Sprintf("Wrote back %s. The next run that uses this drive sees them, owned by the agent. %s",
		driveOpenCount(back.changed, "changed file", "changed files"), back.skipped.text())))
	return nil
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
		return fmt.Errorf("%w%s", cmp.Or(err, fmt.Errorf("%s ended with code %d", argv[0], code)), <-stderr)
	}
	return sendErr
}

// driveOpenDrain reads r to its end, which a session needs or its demux stalls, and answers the first 2 KiB as
// ": text" for an error message ("" when there is none).
func driveOpenDrain(r io.Reader) <-chan string {
	out := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(r, 2048))
		_, _ = io.Copy(io.Discard, r)
		if text := string(bytes.TrimSpace(b)); text != "" {
			out <- ": " + driveOpenSafe(text)
		}
		close(out)
	}()
	return out
}
