// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Tools mode is the root channel of a macOS base's stage-2 boot (prepare and tools updates): the host
// pushes the harness CLIs and a new agent, and runs the fixed stage-2 steps, as root. The agent dials
// the host on VsockToolsPort beside the control and desktop ports and serves ToolsHandler, and nothing
// else, on a session that arrived there. The host listens on that port only for a stage-2 boot, so a
// run boot's dial is refused and none of this is reachable; the control session's handler has none of
// these requests. The agent reports how its dial ended on the control session (ToolsDial).
//
//	POST /v1/tools/push?dest=D&mode=M&size=N&sha256=H   body: exactly N bytes, written as root to D
//	POST /v1/tools/exec                 ToolsExec -> ToolsExecResult
//	GET  /v1/tools/pointer              Pointer, read in the console user's session
//	POST /v1/tools/desktop_password     DesktopPassword -> DesktopPasswordResult
//	POST /v1/tools/admin_code           AdminCode -> AdminCodeResult (edit-base)
//	GET  /v1/tools/posture              ToolsPosture (edit-base's recheck)
const (
	VsockToolsPort = 1026

	// MacToolsBinDir is where a push puts a harness CLI, and where the binaries a tools exec may run
	// as a version check live.
	MacToolsBinDir = "/usr/local/bin"
	// MacCLTGit is the Command Line Tools' git. Tools mode runs it with exactly one argv, CLTGitArgv.
	MacCLTGit = "/Library/Developer/CommandLineTools/usr/bin/git"
	// MacCLTTrigger is the file softwareupdate looks for before it offers the Command Line Tools to a
	// non-interactive caller; touch and rm may name this path only.
	MacCLTTrigger = "/tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress"

	// maxToolsPush bounds a push; the largest pinned CLI is about 274 MB.
	maxToolsPush = 1 << 30
	// maxToolsOutput bounds an exec's combined output in its answer, under the client's MaxFrame.
	maxToolsOutput = 32 << 10

	// MacAdminUser is the base's setup admin, which prepare makes and edit-base's one-time code signs in as.
	MacAdminUser = "wardyn-admin"
	// dsclPath is the directory tool admin_code and posture run, with fixed argv only.
	dsclPath = "/usr/bin/dscl"
	// sshAddr is where Remote Login answers when it is on: launchd listens there for sshd.
	sshAddr = "127.0.0.1:22"
)

// CLTGitArgv is the one argv of the Command Line Tools' git that tools mode runs: the CLT check.
var CLTGitArgv = []string{MacCLTGit, "--version"}

// ToolsExec is a tools-mode exec: argv, run as root with no shell, once ToolsExecAllowed passes it.
type ToolsExec struct {
	Argv []string `json:"argv"`
}

// ToolsExecResult is an exec's combined stdout and stderr (at most maxToolsOutput bytes) and its exit
// code. A refused argv is answered 403 and runs nothing.
type ToolsExecResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

// DesktopPassword sets Screen Sharing's VNC password by the same file write as Init's
// (Desktop.SetPassword): the stage-2 control check's credential. It prints redacted.
type DesktopPassword struct {
	Password string `json:"password"`
}

func (DesktopPassword) String() string   { return "{password:***}" }
func (DesktopPassword) GoString() string { return "guestagent.DesktopPassword{Password:***}" }

// DesktopPasswordResult says only whether the password was set.
type DesktopPasswordResult struct {
	Set bool `json:"desktop_password_set"`
}

// AdminCode sets MacAdminUser's password: edit-base's one-time code before its window session, and a
// fresh value no process keeps after it. It prints redacted.
type AdminCode struct {
	Code string `json:"code"`
}

func (AdminCode) String() string   { return "{code:***}" }
func (AdminCode) GoString() string { return "guestagent.AdminCode{Code:***}" }

// AdminCodeResult says only whether the password was set.
type AdminCodeResult struct {
	Set bool `json:"admin_code_set"`
}

// adminCodeForm is a code admin_code takes: letters and digits only, so it cannot end dscl's command line
// or start another.
var adminCodeForm = regexp.MustCompile(`^[A-Za-z0-9]{8,64}$`)

// ToolsPosture is what edit-base's recheck reads of the guest: its local accounts, and whether Remote
// Login answers. The host decides which accounts Wardyn made.
type ToolsPosture struct {
	Accounts    []GuestAccount `json:"accounts"`
	RemoteLogin bool           `json:"remote_login"`
}

// GuestAccount is one local account, as `dscl . -list /Users UniqueID` lists it.
type GuestAccount struct {
	Name string `json:"name"`
	UID  int    `json:"uid"`
}

// ErrToolsRefused is a push destination or an exec argv tools mode does not allow.
var ErrToolsRefused = errors.New("guestagent: tools mode refuses")

// cltLabel is the one form of label `softwareupdate -i` may install: a Command Line Tools label as
// `softwareupdate -l` lists it, which cannot read as an option.
var cltLabel = regexp.MustCompile(`^Command Line Tools[A-Za-z0-9 ._()-]{0,96}$`)

// ToolsExecAllowed says whether tools mode may run argv as root: softwareupdate as `-l` or as
// `-i <label>` with a cltLabel label (handleExec also requires a label this process's `-l` listed); touch or rm on MacCLTTrigger only (touch T, rm -f T); exactly
// CLTGitArgv; a base item's form (itemArgvAllowed); or a regular file (not a link) directly in binDir, a pushed CLI's version check. A path
// is compared as given, so the same binary by a relative path, a ".." component or a link is refused,
// and nothing names a shell or launchctl.
func ToolsExecAllowed(argv []string, binDir string) error {
	if len(argv) == 0 {
		return fmt.Errorf("%w: an exec needs argv", ErrToolsRefused)
	}
	if itemArgvAllowed(argv) || adminProfileArgvAllowed(argv) {
		return nil
	}
	name := argv[0]
	switch name {
	case "/usr/sbin/softwareupdate":
		if (len(argv) == 2 && argv[1] == "-l") || (len(argv) == 3 && argv[1] == "-i" && cltLabel.MatchString(argv[2])) {
			return nil
		}
	case "/usr/bin/touch":
		if len(argv) == 2 && argv[1] == MacCLTTrigger {
			return nil
		}
	case "/bin/rm":
		if len(argv) == 3 && argv[1] == "-f" && argv[2] == MacCLTTrigger {
			return nil
		}
	case MacCLTGit:
		if len(argv) == len(CLTGitArgv) && argv[1] == CLTGitArgv[1] {
			return nil
		}
	default:
		if filepath.IsAbs(name) && filepath.Clean(name) == name && filepath.Dir(name) == binDir {
			if fi, err := os.Lstat(name); err == nil && fi.Mode().IsRegular() {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: %s with %d arguments", ErrToolsRefused, name, len(argv)-1)
}

// Tools serves tools mode's requests (ToolsHandler). The zero value serves no desktop requests.
type Tools struct {
	Desktop *Desktop
	// AgentBinary and AgentPlist are the agent's own binary and its LaunchDaemon plist, the only files
	// outside BinDir a push may replace; "" allows neither. A rename over the running binary leaves the
	// running agent on its old inode until it restarts.
	AgentBinary, AgentPlist string
	// BinDir is MacToolsBinDir when "" (tests set it).
	BinDir string

	// run runs an allowed argv as root; nil is runCombined (tests record).
	run func(ctx context.Context, argv []string) ([]byte, int, error)
	// passwd sets an account's password; nil is setPassword (tests record). sshAddr is "" in use.
	passwd  func(ctx context.Context, user, password string) error
	sshAddr string

	// cltListed is the Command Line Tools labels this process's own `softwareupdate -l` runs printed,
	// in memory only, so a boot installs only what that boot listed; nil until a `-l` has run.
	mu        sync.Mutex
	cltListed map[string]bool
}

// ToolsHandler serves the requests the tools-mode comment above lists, and nothing else.
func (t *Tools) ToolsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/tools/push", t.handlePush)
	mux.HandleFunc("POST /v1/tools/exec", t.handleExec)
	mux.HandleFunc("POST /v1/tools/admin_code", t.handleAdminCode)
	mux.HandleFunc("GET /v1/tools/posture", t.handlePosture)
	if t.Desktop != nil {
		mux.HandleFunc("GET /v1/tools/pointer", t.Desktop.servePointer)
		mux.HandleFunc("POST /v1/tools/desktop_password", t.handleDesktopPassword)
	}
	return mux
}

func (t *Tools) binDir() string { return cmp.Or(t.BinDir, MacToolsBinDir) }

// pushAllowed says whether a push may write dest: a file directly in BinDir, the agent's own binary
// or plist, or a base item's file (itemPushAllowed).
func (t *Tools) pushAllowed(dest string) error {
	ok := filepath.IsAbs(dest) && filepath.Clean(dest) == dest &&
		((filepath.Dir(dest) == t.binDir() && !strings.HasPrefix(filepath.Base(dest), ".")) ||
			(t.AgentBinary != "" && dest == t.AgentBinary) || (t.AgentPlist != "" && dest == t.AgentPlist) ||
			itemPushAllowed(dest))
	if !ok {
		return fmt.Errorf("%w: a push to %q", ErrToolsRefused, dest)
	}
	return nil
}

func (t *Tools) handlePush(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dest := q.Get("dest")
	mode, merr := strconv.ParseUint(q.Get("mode"), 8, 32)
	size, serr := strconv.ParseInt(q.Get("size"), 10, 64)
	want, herr := hex.DecodeString(q.Get("sha256"))
	switch {
	case merr != nil || mode&^0o777 != 0 || mode&0o022 != 0:
		http.Error(w, "a push's mode is permission bits in octal, writable by its owner only", http.StatusBadRequest)
		return
	case serr != nil || size < 0 || size > maxToolsPush || (r.ContentLength >= 0 && r.ContentLength != size):
		http.Error(w, "a push's size is missing, too large, or not its body's", http.StatusBadRequest)
		return
	case herr != nil || len(want) != sha256.Size:
		http.Error(w, "a push needs its body's sha256 in hex", http.StatusBadRequest)
		return
	}
	if err := t.pushAllowed(dest); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if filepath.Dir(dest) == t.binDir() {
		if err := os.MkdirAll(t.binDir(), 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	var refused []string // a file item's dest is none of them by another name either
	if ItemDestAllowed(dest) == nil {
		refused = itemDestRefused
	}
	err := replaceFileOutside(dest, os.FileMode(mode), refused, func(f *os.File) error {
		h := sha256.New()
		if _, err := io.CopyN(io.MultiWriter(f, h), r.Body, size); err != nil {
			return fmt.Errorf("the body is shorter than %d bytes: %w", size, err)
		}
		if n, _ := r.Body.Read(make([]byte, 1)); n != 0 {
			return fmt.Errorf("the body is longer than %d bytes", size)
		}
		if !bytes.Equal(h.Sum(nil), want) {
			return errors.New("the body's sha256 is not the pin's")
		}
		return nil
	})
	if err != nil {
		http.Error(w, "push "+dest+": "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (t *Tools) handleExec(w http.ResponseWriter, r *http.Request) {
	var in ToolsExec
	if err := json.NewDecoder(io.LimitReader(r.Body, MaxFrame)).Decode(&in); err != nil {
		http.Error(w, "bad tools exec: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := ToolsExecAllowed(in.Argv, t.binDir()); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	update := in.Argv[0] == "/usr/sbin/softwareupdate"
	if update && in.Argv[1] == "-i" && !t.cltWasListed(in.Argv[2]) {
		http.Error(w, fmt.Sprintf("%v: softwareupdate -i %q, a label no softwareupdate -l of this boot listed",
			ErrToolsRefused, in.Argv[2]), http.StatusForbidden)
		return
	}
	if isItemDirForm(in.Argv) {
		mode, _ := strconv.ParseUint(in.Argv[3], 8, 32) // 0700 or 0755: ToolsExecAllowed checked it
		var refused []string                            // the stage directories are themselves refused paths
		if in.Argv[3] == "0755" {
			refused = itemDestRefused
		}
		if err := makeDirAll(in.Argv[4], uint32(mode), refused); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSONAny(w, ToolsExecResult{})
		return
	}
	run := t.run
	if run == nil {
		run = runCombined
	}
	out, code, err := run(r.Context(), in.Argv)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if update && in.Argv[1] == "-l" {
		t.listCLTLabels(string(out))
	}
	writeJSONAny(w, ToolsExecResult{Output: string(out), ExitCode: code})
}

// listCLTLabels records the Command Line Tools labels in a `softwareupdate -l` output, parsed as the
// host picks one (internal/macvm pickCLTLabel): a "* " line, less any "Label:" prefix, naming them.
func (t *Tools) listCLTLabels(list string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cltListed == nil {
		t.cltListed = map[string]bool{}
	}
	for _, line := range strings.Split(list, "\n") {
		l, ok := strings.CutPrefix(strings.TrimSpace(line), "* ")
		if !ok {
			continue
		}
		l = strings.TrimSpace(strings.TrimPrefix(l, "Label:"))
		if strings.Contains(l, "Command Line Tools") {
			t.cltListed[l] = true
		}
	}
}

// cltWasListed says whether a `softwareupdate -l` of this process listed label.
func (t *Tools) cltWasListed(label string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cltListed[label]
}

func (t *Tools) handleDesktopPassword(w http.ResponseWriter, r *http.Request) {
	var in DesktopPassword
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&in); err != nil {
		http.Error(w, "bad desktop_password request", http.StatusBadRequest)
		return
	}
	err := t.Desktop.SetPassword(in.Password)
	if err != nil {
		slog.Warn("guest agent: tools mode: desktop password not set", "err", err)
	}
	writeJSONAny(w, DesktopPasswordResult{Set: err == nil})
}

func (t *Tools) handleAdminCode(w http.ResponseWriter, r *http.Request) {
	var in AdminCode
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&in); err != nil || !adminCodeForm.MatchString(in.Code) {
		http.Error(w, "bad admin_code request", http.StatusBadRequest)
		return
	}
	passwd := t.passwd
	if passwd == nil {
		passwd = setPassword
	}
	err := passwd(r.Context(), MacAdminUser, in.Code)
	if err != nil {
		slog.Warn("guest agent: tools mode: admin code not set", "err", err)
	}
	writeJSONAny(w, AdminCodeResult{Set: err == nil})
}

func (t *Tools) handlePosture(w http.ResponseWriter, r *http.Request) {
	run := t.run
	if run == nil {
		run = runCombined
	}
	out, code, err := run(r.Context(), []string{dsclPath, ".", "-list", "/Users", "UniqueID"})
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d", code)
	}
	var p ToolsPosture
	if err == nil {
		p.Accounts, err = parseAccounts(string(out))
	}
	if err != nil {
		http.Error(w, "the guest's accounts were not read: "+err.Error(), http.StatusInternalServerError)
		return
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	if c, err := d.DialContext(r.Context(), "tcp", cmp.Or(t.sshAddr, sshAddr)); err == nil {
		_ = c.Close()
		p.RemoteLogin = true
	}
	writeJSONAny(w, p)
}

// parseAccounts reads `dscl . -list /Users UniqueID`: one "name uid" line per account.
func parseAccounts(out string) ([]GuestAccount, error) {
	accounts := []GuestAccount{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		uid, err := strconv.Atoi(f[len(f)-1])
		if len(f) != 2 || err != nil {
			return nil, fmt.Errorf("an account line %q is not a name and a uid", line)
		}
		accounts = append(accounts, GuestAccount{Name: f[0], UID: uid})
	}
	return accounts, nil
}

// setPassword sets user's password as root through dscl's own command line on stdin, so the password is
// in no argv. dscl's interactive mode exits 0 after a failed command, so its output decides; that output
// is never returned, as it could hold the line it read.
func setPassword(ctx context.Context, user, password string) error {
	cmd := exec.CommandContext(ctx, dsclPath, ".")
	cmd.Stdin = strings.NewReader("passwd /Users/" + user + " " + password + "\n")
	out := &capped{max: maxToolsOutput}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dscl passwd: %w", err)
	}
	if strings.Contains(strings.ToLower(out.b.String()), "error") {
		return errors.New("dscl passwd reported an error")
	}
	return nil
}

// runCombined runs argv as the agent's own user (root in a guest) and returns its combined output, at
// most maxToolsOutput bytes of it, and its exit code. err is a failure to run it, not a non-zero exit.
func runCombined(ctx context.Context, argv []string) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out := &capped{max: maxToolsOutput}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.b.Bytes(), ee.ExitCode(), nil
	}
	if err != nil {
		return nil, 0, err
	}
	return out.b.Bytes(), 0, nil
}

// capped keeps the first max bytes written to it and drops the rest.
type capped struct {
	b   bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// replaceFile writes dst through a temp file in dst's directory, created O_CREAT|O_EXCL|O_NOFOLLOW
// with mode perm under a random name, filled by fill, fsynced and then renamed over dst. Both are done
// on dst's directory as walkDir opened it, so no link, and no directory someone other than root can
// change, among dst's parents redirects the write; a link at dst itself is replaced, not followed. The
// agent runs as root, so the file is root's. On any failure the temp file is removed and dst is unchanged.
func replaceFile(dst string, perm os.FileMode, fill func(*os.File) error) error {
	return replaceFileOutside(dst, perm, nil, fill)
}

// replaceFileOutside is replaceFile that also refuses, with ErrItemDest, a dst that is, or whose walk
// passes through, an existing path of refused, by its inode however dst spells it.
func replaceFileOutside(dst string, perm os.FileMode, refused []string, fill func(*os.File) error) (err error) {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return err
	}
	inodes := refusedInodes(refused)
	dir, base := filepath.Dir(dst), filepath.Base(dst)
	dirFD, err := walkDir(dst, dir, 0, inodes)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	var st unix.Stat_t
	if unix.Fstatat(dirFD, base, &st, unix.AT_SYMLINK_NOFOLLOW) == nil {
		if err := refusedInode(&st, dst, inodes); err != nil {
			return err
		}
	}
	tmp := "." + base + ".wardyn-" + hex.EncodeToString(r[:])
	fd, err := unix.Openat(dirFD, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		return &os.PathError{Op: "open", Path: filepath.Join(dir, tmp), Err: err}
	}
	// The name only labels f's errors; the file is the one Openat made under dirFD.
	f := os.NewFile(uintptr(fd), "pushed file")
	defer func() {
		if f != nil {
			_ = f.Close()
		}
		if err != nil {
			_ = unix.Unlinkat(dirFD, tmp, 0)
		}
	}()
	if err = f.Chmod(perm); err != nil { // the umask narrows Openat's mode
		return err
	}
	if err = fill(f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	err = f.Close()
	f = nil
	if err != nil {
		return err
	}
	if err = unix.Renameat(dirFD, tmp, dirFD, base); err != nil {
		return &os.LinkError{Op: "rename", Old: filepath.Join(dir, tmp), New: dst, Err: err}
	}
	return nil
}

// makeDirAll is `mkdir -p -m mode dir` done by walkDir: each missing parent is made 0755 and dir itself
// mode, root's, and no link or unsafe directory on the way is followed.
func makeDirAll(dir string, mode uint32, refused []string) error {
	fd, err := walkDir(dir, dir, mode, refusedInodes(refused))
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// ErrUnsafeParent is a directory on a root write's path that is a link, or that someone other than root
// can change: another user's, writable by every user without the sticky bit, or writable by a group
// not in trustedGroupGIDs.
var ErrUnsafeParent = errors.New("a link or a directory not only root can change")

// The owner and groups walkDir trusts besides root: wheel (0) and admin (80), whose write the stock
// /Library directories allow (root:admin 0775). The agent user is not in admin: stage 1 creates it with
// `sysadminctl -addUser` and no -admin (internal/macvm/stage1 commands.go). Tests set both.
var (
	trustedOwnerUID  uint32
	trustedGroupGIDs = []uint32{0, 80}
)

// walkDir opens dir, absolute and clean, one component at a time from "/" with O_NOFOLLOW|O_DIRECTORY,
// and returns its fd. Each directory opened must be safe (ErrUnsafeParent) and none an inode of refused
// (ErrItemDest). With a mode, a missing component is made on its parent's fd, 0755 or, the last, mode
// (mkdir -p -m); with 0, none is. what names the write in errors.
func walkDir(what, dir string, mode uint32, refused map[devIno]string) (int, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return -1, fmt.Errorf("%q: not an absolute, clean path", what)
	}
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	at := ""
	for _, comp := range strings.Split(dir, "/") {
		if comp == "" {
			continue
		}
		comp = filepath.Base(comp)
		at += "/" + comp
		made, perm := false, uint32(0o755)
		if at == dir {
			perm = mode
		}
		next, err := unix.Openat(fd, comp, flags, 0)
		if errors.Is(err, unix.ENOENT) && mode != 0 {
			if err = unix.Mkdirat(fd, comp, perm); err == nil {
				made = true
				next, err = unix.Openat(fd, comp, flags, 0)
			}
		}
		_ = unix.Close(fd)
		switch {
		case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR): // a link: darwin says ELOOP, Linux ENOTDIR
			return -1, fmt.Errorf("%q: %q is a link or not a directory: %w", what, at, ErrUnsafeParent)
		case err != nil:
			return -1, fmt.Errorf("%q: %s: %w", what, at, err)
		}
		fd = next
		if made {
			err = unix.Fchmod(fd, perm) // the umask narrows Mkdirat's mode
		}
		if err == nil {
			err = checkDir(fd, what, at, refused)
		}
		if err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}

// checkDir says whether the directory fd, at on what's path, is safe for a root write to pass through.
func checkDir(fd int, what, at string, refused map[devIno]string) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("%q: %s: %w", what, at, err)
	}
	switch {
	case st.Uid != 0 && st.Uid != trustedOwnerUID:
		return fmt.Errorf("%q: %q is uid %d's: %w", what, at, st.Uid, ErrUnsafeParent)
	case st.Mode&unix.S_IWOTH != 0 && st.Mode&unix.S_ISVTX == 0:
		return fmt.Errorf("%q: %q is world-writable: %w", what, at, ErrUnsafeParent)
	case st.Mode&unix.S_IWGRP != 0 && !slices.Contains(trustedGroupGIDs, st.Gid):
		return fmt.Errorf("%q: %q is writable by gid %d: %w", what, at, st.Gid, ErrUnsafeParent)
	}
	return refusedInode(&st, what, refused)
}

type devIno struct{ dev, ino uint64 }

// refusedInodes stats each path of refused that exists, by its inode.
func refusedInodes(refused []string) map[devIno]string {
	m := map[devIno]string{}
	for _, p := range refused {
		var st unix.Stat_t
		if unix.Stat(p, &st) == nil {
			m[devIno{uint64(st.Dev), st.Ino}] = p //nolint:unconvert // Dev is int32 on darwin
		}
	}
	return m
}

// refusedInode is ErrItemDest when st is one of refused's inodes.
func refusedInode(st *unix.Stat_t, what string, refused map[devIno]string) error {
	if p, ok := refused[devIno{uint64(st.Dev), st.Ino}]; ok { //nolint:unconvert // Dev is int32 on darwin
		return fmt.Errorf("%q is %s: %w", what, p, ErrItemDest)
	}
	return nil
}

// Base items are the operator's install list (internal/macvm customise.go), which stage 2 installs as
// root with the fixed argv forms below and nothing else: no shell, and no flag the list can choose. Each
// item is staged in a directory of its own under MacItemStageDir, root's and mode 0700, by its name.
const (
	MacItemStageDir = "/Library/Wardyn/stage"
	// The staged file of each kind, in its item's directory; a dmg is attached at ItemMount there.
	ItemPkg     = "item.pkg"
	ItemZip     = "item.zip"
	ItemDmg     = "item.dmg"
	ItemMount   = "mnt"
	ItemProfile = "item.mobileconfig"
	ItemCert    = "item.cer"
	// MacApplications is where an app item goes.
	MacApplications = "/Applications"
)

// ItemNamePattern is an item's name, its stage directory's.
var ItemNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ItemSecretPattern is the name a pkg item's secret file (a licence, a registration token) has beside
// its item.pkg, where the vendor's installer reads it; it is pushed 0600 and goes with the stage directory.
var ItemSecretPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ItemAppPattern is the bundle an app item's dmg holds, as it lands in MacApplications.
var ItemAppPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,95}\.app$`)

// ItemDir is item name's stage directory.
func ItemDir(name string) string { return MacItemStageDir + "/" + name }

// itemDestRoots are where a file item may land (not /etc, whose files are the system's own), and
// itemDestRefused the paths under them it may not: the
// files a run's walls are made of, which a run's Init writes (macos_paths.go), Wardyn's own files, and the
// System keychain, which only a ca item changes. ItemDestAllowed compares the paths' spelling without
// ASCII case, as the guest's APFS volume does, and refuses any other byte, so no spelling it does not
// know folds onto one; the write's walk then refuses each by its inode (replaceFileOutside, makeDirAll).
var (
	itemDestRoots   = []string{"/Library", "/usr/local", "/opt"}
	itemDestRefused = []string{MacClaudeCodeManagedDir, MacCodexManagedDir, MacRunDir, MacItemStageDir, MacToolsBinDir,
		MacAgentBinaryPath, MacAgentPlistPath, "/Library/Keychains"}
)

// ErrItemDest is a file item's destination the guest does not allow.
var ErrItemDest = errors.New("not a place a base item may write")

// ItemDestAllowed says whether a file item may write dest, or make dest as its directory: an absolute,
// ASCII, clean path strictly under an itemDestRoots entry and neither in nor above any itemDestRefused
// path. The check is lexical. Harness managed settings are refused because a run writes them; an
// organisation's own go in its rules.
func ItemDestAllowed(dest string) error {
	if !filepath.IsAbs(dest) || filepath.Clean(dest) != dest || strings.IndexFunc(dest, func(r rune) bool { return r >= utf8.RuneSelf }) >= 0 {
		return fmt.Errorf("%q: %w", dest, ErrItemDest)
	}
	d := strings.ToLower(dest)
	under := func(p, root string) bool { return strings.HasPrefix(p, strings.ToLower(root)+"/") }
	ok := false
	for _, r := range itemDestRoots {
		ok = ok || under(d, r)
	}
	for _, r := range itemDestRefused {
		if l := strings.ToLower(r); d == l || under(d, r) || under(l, dest) {
			ok = false
		}
	}
	if !ok {
		return fmt.Errorf("%q: %w", dest, ErrItemDest)
	}
	return nil
}

// isItemDir says whether p is an item's stage directory.
func isItemDir(p string) bool {
	return filepath.Clean(p) == p && filepath.Dir(p) == MacItemStageDir && ItemNamePattern.MatchString(filepath.Base(p))
}

// isItemFile says whether p is file in an item's stage directory.
func isItemFile(p, file string) bool { return filepath.Base(p) == file && isItemDir(filepath.Dir(p)) }

// itemPushAllowed says whether a push may write dest for a base item: a staged file of its kind or a
// secret file in its stage directory, or a file item's destination.
func itemPushAllowed(dest string) bool {
	if isItemDir(filepath.Dir(dest)) {
		return slices.Contains([]string{ItemPkg, ItemZip, ItemDmg, ItemProfile, ItemCert}, filepath.Base(dest)) ||
			(ItemSecretPattern.MatchString(filepath.Base(dest)) && filepath.Base(dest) != ItemMount)
	}
	return ItemDestAllowed(dest) == nil
}

// itemArgvAllowed says whether argv is one of the base items' forms:
//
//	/bin/mkdir -p -m 0700 S|S/<name>                  the stage directories
//	/bin/mkdir -p -m 0755 D                           a file item's directory (ItemDestAllowed)
//	/bin/rm -rf S/<name>                              an item's stage directory, after its install
//	/usr/sbin/installer -pkg S/<name>/item.pkg -target /
//	/usr/bin/ditto -x -k S/<name>/item.zip /Applications
//	/usr/bin/hdiutil attach -nobrowse -readonly -noautoopen -mountpoint S/<name>/mnt S/<name>/item.dmg
//	/usr/bin/ditto S/<name>/mnt/<App>.app /Applications/<App>.app
//	/usr/bin/hdiutil detach -force S/<name>/mnt
//	/usr/bin/profiles list                            the profiles installed, read back
//	/usr/bin/security add-trusted-cert -d -r trustRoot -p ssl -k /Library/Keychains/System.keychain S/<name>/item.cer
//	/usr/bin/security delete-certificate -t -Z <SHA-1> /Library/Keychains/System.keychain
//	/usr/bin/security verify-cert -L -p ssl -c S/<name>/item.cer
//	/usr/bin/sw_vers -productVersion|-buildVersion     the guest posture's reads
//	/usr/bin/csrutil status
//	/usr/sbin/spctl --status
//	/usr/bin/profiles status -type enrollment
//
// The agent does both mkdir forms itself (makeDirAll), without mkdir. The two that change the admin
// trust settings run as root like the rest: macOS 26 refuses root a rewrite of the authorization rule, so a
// trust Apple will not make without a person reads not_trusted, for a trust by hand in edit-base.
func itemArgvAllowed(argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	if form, ok := itemForms[argv[0]+" "+argv[1]]; ok {
		return form(argv)
	}
	app := filepath.Base(argv[len(argv)-1])
	return len(argv) == 3 && argv[0] == "/usr/bin/ditto" && ItemAppPattern.MatchString(app) &&
		argv[1] == filepath.Dir(argv[1])+"/"+app && isItemFile(filepath.Dir(argv[1]), ItemMount) && argv[2] == MacApplications+"/"+app
}

// isItemDirForm says whether argv, an allowed item form, is a mkdir (a stage directory or a file item's
// directory), which the agent does itself (makeDirAll) because mkdir follows a link among its parents.
func isItemDirForm(argv []string) bool {
	return len(argv) == 5 && argv[0] == "/bin/mkdir" && argv[1] == "-p"
}

// itemForms checks each item form's whole argv, by its command and first argument.
var itemForms = map[string]func(a []string) bool{
	"/bin/mkdir -p": func(a []string) bool {
		return len(a) == 5 && a[2] == "-m" &&
			(a[3] == "0700" && (a[4] == MacItemStageDir || isItemDir(a[4])) || a[3] == "0755" && ItemDestAllowed(a[4]) == nil)
	},
	"/bin/rm -rf": func(a []string) bool { return len(a) == 3 && isItemDir(a[2]) },
	"/usr/sbin/installer -pkg": func(a []string) bool {
		return len(a) == 5 && isItemFile(a[2], ItemPkg) && a[3] == "-target" && a[4] == "/"
	},
	"/usr/bin/ditto -x": func(a []string) bool {
		return len(a) == 5 && a[2] == "-k" && isItemFile(a[3], ItemZip) && a[4] == MacApplications
	},
	"/usr/bin/hdiutil attach": func(a []string) bool {
		return len(a) == 8 && slices.Equal(a[2:6], []string{"-nobrowse", "-readonly", "-noautoopen", "-mountpoint"}) &&
			isItemFile(a[6], ItemMount) && a[7] == filepath.Dir(a[6])+"/"+ItemDmg
	},
	"/usr/bin/hdiutil detach": func(a []string) bool { return len(a) == 4 && a[2] == "-force" && isItemFile(a[3], ItemMount) },
	"/usr/bin/profiles list":  func(a []string) bool { return len(a) == 2 },
	"/usr/bin/security add-trusted-cert": func(a []string) bool {
		return len(a) == 10 && slices.Equal(a[2:9], []string{"-d", "-r", "trustRoot", "-p", "ssl", "-k", MacSystemKeychain}) && isItemFile(a[9], ItemCert)
	},
	"/usr/bin/security delete-certificate": func(a []string) bool {
		return len(a) == 6 && a[2] == "-t" && a[3] == "-Z" && sha1Hex.MatchString(a[4]) && a[5] == MacSystemKeychain
	},
	"/usr/bin/sw_vers -productVersion": func(a []string) bool { return len(a) == 2 },
	"/usr/bin/sw_vers -buildVersion":   func(a []string) bool { return len(a) == 2 },
	"/usr/bin/csrutil status":          func(a []string) bool { return len(a) == 2 },
	"/usr/sbin/spctl --status":         func(a []string) bool { return len(a) == 2 },
	"/usr/bin/profiles status":         func(a []string) bool { return len(a) == 4 && a[2] == "-type" && a[3] == "enrollment" },
	"/usr/bin/security verify-cert": func(a []string) bool {
		return len(a) == 7 && slices.Equal(a[2:6], []string{"-L", "-p", "ssl", "-c"}) && isItemFile(a[6], ItemCert)
	},
}

var sha1Hex = regexp.MustCompile(`^[0-9A-F]{40}$`)

// MacAdminProfileDir is where edit-base gives wardyn-admin its own copy of each profile it staged for the
// window, to open from the desktop session: wardyn-admin's alone (0700, its files 0600), removed by the
// recheck.
const MacAdminProfileDir = "/Users/" + MacAdminUser + "/Wardyn"

// AdminProfile is wardyn-admin's copy of the profile item name; name is an ItemNamePattern name.
func AdminProfile(name string) string { return MacAdminProfileDir + "/" + name + ".mobileconfig" }

// isAdminProfile says whether p is wardyn-admin's copy of a profile item with a valid name.
func isAdminProfile(p string) bool {
	name, ok := strings.CutSuffix(strings.TrimPrefix(p, MacAdminProfileDir+"/"), ".mobileconfig")
	return ok && ItemNamePattern.MatchString(name) && p == AdminProfile(name)
}

// adminProfileArgvAllowed says whether argv is one of edit-base's forms for wardyn-admin's copies, D being
// MacAdminProfileDir and S MacItemStageDir:
//
//	/bin/rm -rf D
//	/bin/mkdir -m 0700 D                              no -p: D must not be there already
//	/usr/bin/ditto S/<name>/item.mobileconfig D/<name>.mobileconfig
//	/usr/sbin/chown -h wardyn-admin D|D/<name>.mobileconfig
//	/usr/bin/stat D|S/<name>                          a removal's check, which must fail
func adminProfileArgvAllowed(argv []string) bool {
	switch {
	case len(argv) == 3 && argv[0] == "/bin/rm" && argv[1] == "-rf":
		return argv[2] == MacAdminProfileDir
	case len(argv) == 4 && argv[0] == "/bin/mkdir" && argv[1] == "-m" && argv[2] == "0700":
		return argv[3] == MacAdminProfileDir
	case len(argv) == 3 && argv[0] == "/usr/bin/ditto":
		return isItemFile(argv[1], ItemProfile) && argv[2] == AdminProfile(filepath.Base(filepath.Dir(argv[1])))
	case len(argv) == 4 && argv[0] == "/usr/sbin/chown" && argv[1] == "-h" && argv[2] == MacAdminUser:
		return argv[3] == MacAdminProfileDir || isAdminProfile(argv[3])
	case len(argv) == 2 && argv[0] == "/usr/bin/stat":
		return argv[1] == MacAdminProfileDir || isItemDir(argv[1])
	}
	return false
}
