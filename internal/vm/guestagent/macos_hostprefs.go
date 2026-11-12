// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

// Every run boots with a new machine identity (IdentityFresh), so settings macOS keeps per host stay with
// the base's. This file copies them at boot, when the guest agent starts: the agent account's
// ~/Library/Preferences/ByHost files named for the base's identity are copied to names for this boot's.
//
// The copy runs as the agent, so no link, hard link or swap the agent account plants can point it anywhere
// the agent could not already write. Root reads only ioreg and writes only the status file.
//
// It sends no signal and starts nothing in the agent's login session.
package guestagent

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// HostPrefsCarryVerb is the guest agent's binary under which the copy runs as the agent.
const HostPrefsCarryVerb = "host-prefs-carry"

const (
	// hostPrefsMaxFiles is how many files of the base's identity the copy takes; more copies nothing.
	hostPrefsMaxFiles = 256
	// hostPrefsMaxBytes is the most one file may hold; the read itself enforces it.
	hostPrefsMaxBytes = 1 << 20
	// hostPrefsOutCap is how much of the child's output Carry keeps.
	hostPrefsOutCap = 4 << 10
)

var (
	// hostPrefsID is a machine identity as ioreg prints it: IOPlatformUUID, upper-case.
	hostPrefsID = regexp.MustCompile(`^[0-9A-F]{8}(-[0-9A-F]{4}){3}-[0-9A-F]{12}$`)
	// hostPrefsUUIDLine is the ioreg line that holds it.
	hostPrefsUUIDLine = regexp.MustCompile(`(?m)^\s*"IOPlatformUUID"\s*=\s*"([^"]*)"\s*$`)
	// hostPrefsFile is a ByHost file name: the preference domain, the identity, .plist.
	hostPrefsFile = regexp.MustCompile(`^(.+)\.([0-9A-F]{8}(?:-[0-9A-F]{4}){3}-[0-9A-F]{12})\.plist$`)
	// hostPrefsAnswer is the only output the child may print: counts or a reason, never a name.
	hostPrefsAnswer = regexp.MustCompile(`^host_prefs=(carried files=[0-9]+ kept=[0-9]+ skipped=[0-9]+ failed=[0-9]+|skipped reason=[a-z_]+)$`)
)

// hostPrefsBeforeOpen runs between the copy's listing and its open of a source. A test hook.
var hostPrefsBeforeOpen = func(name string) {}

// HostPrefsCarry carries the agent's per-host settings from the base's machine identity to this boot's.
// It is off unless the base has the switch file (MacHostPrefsSwitchPath).
type HostPrefsCarry struct {
	Run     CmdRunner
	Console ConsoleUser
	// MarkerDir is where Init leaves its marker; a boot that has one is a restart of this service.
	MarkerDir string
	// Switch is the file that turns the carry on; "" is MacHostPrefsSwitchPath.
	Switch string
	// ByHost is the agent's ByHost folder; "" is MacAgentHome + "/Library/Preferences/ByHost".
	ByHost string
	// Cred is the agent's ids, which the child runs with; nil reads as the guest agent's own user (tests).
	Cred *syscall.Credential
	// Self is the guest agent's own binary, which the child runs under HostPrefsCarryVerb.
	Self string
	// argv is the child's command before the folder and the id; nil is Self and the verb (tests set a
	// shell line).
	argv []string
	// bound is how long the child may take; zero is 15 s (tests shorten it).
	bound time.Duration
}

// Carry runs the copy when the gate allows it and returns the status line: host_prefs=off, skipped or
// failed with a reason, or carried with counts. Past off, every line ends with at=, the Unix time in
// seconds, and the ones from the console on carry console=, none or the console's uid. A line never holds
// a file's name or contents.
func (c HostPrefsCarry) Carry(ctx context.Context) string {
	if fi, err := os.Lstat(cmp.Or(c.Switch, MacHostPrefsSwitchPath)); err != nil || !fi.Mode().IsRegular() {
		return "host_prefs=off"
	}
	line := func(head, console string) string {
		if console != "" {
			head += " console=" + console
		}
		return head + " at=" + strconv.FormatInt(time.Now().Unix(), 10)
	}
	if !BootScrubDue(c.MarkerDir) {
		return line("host_prefs=skipped reason=not_first_start", "")
	}
	switch uid, err := c.Console.Console(ctx); {
	case errors.Is(err, ErrNoConsoleUser):
	case err != nil:
		return line("host_prefs=skipped reason=console_unread", "")
	default:
		return line("host_prefs=skipped reason=agent_signed_in", strconv.FormatUint(uint64(uid), 10))
	}
	id := c.thisBootID(ctx)
	if id == "" {
		return line("host_prefs=skipped reason=uuid_unread", "none")
	}
	switch out, how := c.runChild(ctx, id); how {
	case "":
		return line(out, "none")
	default:
		return line("host_prefs=failed reason="+how, "none")
	}
}

// thisBootID is this boot's IOPlatformUUID, or "" when ioreg gives none or one of another shape.
func (c HostPrefsCarry) thisBootID(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := c.Run.Run(ctx, "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	if err != nil {
		return ""
	}
	m := hostPrefsUUIDLine.FindSubmatch(out)
	if m == nil || !hostPrefsID.Match(m[1]) {
		return ""
	}
	return string(m[1])
}

// runChild runs the copy as the agent, in a session of its own, and returns its one answer line. When the
// child did not give one it returns the reason instead: child_failed, timeout or bad_output. Past the
// bound the child's whole process group is killed first, so nothing of it lingers; Wait reaps it in the
// background.
func (c HostPrefsCarry) runChild(ctx context.Context, id string) (answer, reason string) {
	argv := c.argv
	if argv == nil {
		argv = []string{c.Self, HostPrefsCarryVerb}
	}
	byHost := cmp.Or(c.ByHost, MacAgentHome+"/Library/Preferences/ByHost")
	cmd := exec.Command(argv[0], append(argv[1:len(argv):len(argv)], byHost, id)...)
	var out cappedBuffer
	cmd.Stdout = &out
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: c.Cred}
	if err := cmd.Start(); err != nil {
		return "", "child_failed"
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(cmp.Or(c.bound, 15*time.Second))
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return "", "child_failed"
		}
	case <-timer.C:
		// Setsid made the child its own process group, so this also ends whatever it started.
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		return "", "timeout"
	case <-ctx.Done():
		_ = unix.Kill(-cmd.Process.Pid, unix.SIGKILL)
		return "", "child_failed"
	}
	body, ok := bytes.CutSuffix(out.buf, []byte("\n"))
	if out.over || !ok || !hostPrefsAnswer.Match(body) {
		return "", "bad_output"
	}
	return string(body), ""
}

// cappedBuffer keeps the first hostPrefsOutCap bytes written to it and notes any more.
type cappedBuffer struct {
	buf  []byte
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := hostPrefsOutCap - len(b.buf); n > room {
		b.over = true
		p = p[:room]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

// HostPrefsCarryMain is the child: it runs as the agent under HostPrefsCarryVerb with exactly two args, the
// ByHost folder and this boot's id. Any other count, or an id of another shape, exits 2 and prints nothing.
func HostPrefsCarryMain(args []string, stdout io.Writer) int {
	if len(args) != 2 || !hostPrefsID.MatchString(args[1]) {
		return 2
	}
	fmt.Fprintln(stdout, carryByHost(args[0], args[1], os.Getuid()))
	return 0
}

// carryByHost copies the files of the one other machine identity in dir to thisID, as uid, and returns the
// answer line. Every step works on the opened folder's descriptor, never on a path. A destination is made
// by linking a finished temp file into place, so it is never half-written and never replaced; a source
// changed during its copy is not detected, and the copy may mix its old and new bytes.
func carryByHost(dir, thisID string, uid int) string {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "host_prefs=skipped reason=no_byhost"
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || int(st.Uid) != uid {
		return "host_prefs=skipped reason=no_byhost"
	}
	byID, err := listByHost(fd, thisID)
	if err != nil {
		return "host_prefs=skipped reason=no_byhost"
	}
	switch len(byID) {
	case 0:
		return "host_prefs=skipped reason=no_other_identity"
	case 1:
	default:
		return "host_prefs=skipped reason=several_identities"
	}
	var names []string
	for _, n := range byID {
		names = n
	}
	if len(names) > hostPrefsMaxFiles {
		return "host_prefs=skipped reason=too_many_files"
	}
	var copiedN, kept, skipped, failed int
	for _, name := range names {
		dest := hostPrefsFile.ReplaceAllString(name, "${1}."+thisID+".plist")
		switch copyOne(fd, name, dest, uid) {
		case copied:
			copiedN++
		case keptDest:
			kept++
		case skippedSrc:
			skipped++
		default:
			failed++
		}
	}
	return fmt.Sprintf("host_prefs=carried files=%d kept=%d skipped=%d failed=%d", copiedN, kept, skipped, failed)
}

// listByHost lists the folder behind fd and returns, by identity other than thisID, the names of its regular
// files named <domain>.<identity>.plist. A link is not a regular file here.
func listByHost(fd int, thisID string) (map[string][]string, error) {
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "byhost")
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	byID := map[string][]string{}
	for _, name := range names {
		m := hostPrefsFile.FindStringSubmatch(name)
		if m == nil || m[2] == thisID {
			continue
		}
		var st unix.Stat_t
		if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		byID[m[2]] = append(byID[m[2]], name)
	}
	return byID, nil
}

// copyResult is what became of one file.
type copyResult int

const (
	copied copyResult = iota
	keptDest
	skippedSrc
	failedCopy
)

// copyOne copies the file src to dest in the folder behind fd. A destination that exists, of any type, is
// kept as it is. The source is opened without following a link, and what was opened is checked: a regular
// file, the agent's, with one link and at most hostPrefsMaxBytes. The read is bounded too, whatever the
// check said. The bytes go to a temp file that is linked into place, which fails if dest appeared since.
func copyOne(fd int, src, dest string, uid int) copyResult {
	var st unix.Stat_t
	switch err := unix.Fstatat(fd, dest, &st, unix.AT_SYMLINK_NOFOLLOW); {
	case err == nil:
		return keptDest
	case !errors.Is(err, unix.ENOENT):
		return failedCopy
	}
	hostPrefsBeforeOpen(src)
	sfd, err := unix.Openat(fd, src, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return skippedSrc
	}
	in := os.NewFile(uintptr(sfd), "source")
	defer in.Close()
	if err := unix.Fstat(sfd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || int(st.Uid) != uid ||
		st.Nlink != 1 || st.Size > hostPrefsMaxBytes {
		return skippedSrc
	}
	data, err := io.ReadAll(io.LimitReader(in, hostPrefsMaxBytes+1))
	if err != nil {
		return failedCopy
	}
	if len(data) > hostPrefsMaxBytes {
		return skippedSrc
	}
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return failedCopy
	}
	tmp := "." + dest + ".wardyn-" + hex.EncodeToString(r[:])
	tfd, err := unix.Openat(fd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return failedCopy
	}
	defer unix.Unlinkat(fd, tmp, 0)
	out := os.NewFile(uintptr(tfd), "temp")
	_, werr := out.Write(data)
	if serr := out.Sync(); werr == nil {
		werr = serr
	}
	if cerr := out.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return failedCopy
	}
	switch err := unix.Linkat(fd, tmp, fd, dest, 0); {
	case err == nil:
		return copied
	case errors.Is(err, unix.EEXIST):
		return keptDest
	}
	return failedCopy
}

// WriteHostPrefsStatus writes line, and a newline, to path as root: the folder first, since the boot scrub
// removed it and only Init makes it again, then the file through replaceFile, so a link at path is replaced.
func WriteHostPrefsStatus(path, line string) error {
	if err := makeDirAll(filepath.Dir(path), 0o755, nil); err != nil {
		return err
	}
	return replaceFile(path, 0o644, func(f *os.File) error {
		_, err := f.WriteString(line + "\n")
		return err
	})
}
