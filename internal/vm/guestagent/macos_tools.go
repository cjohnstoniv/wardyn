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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

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
	if itemArgvAllowed(argv) {
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
	err := replaceFile(dest, os.FileMode(mode), func(f *os.File) error {
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
// with mode perm under a random name, filled by fill, fsynced and then renamed over dst. The agent
// runs as root, so the file is root's. On any failure the temp file is removed and dst is unchanged.
func replaceFile(dst string, perm os.FileMode, fill func(*os.File) error) (err error) {
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".wardyn-"+hex.EncodeToString(r[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, perm)
	if err != nil {
		return err
	}
	defer func() {
		if f != nil {
			_ = f.Close()
		}
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil { // the umask narrows OpenFile's mode
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
	return os.Rename(tmp, dst)
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
	// MacApplications is where an app item goes.
	MacApplications = "/Applications"
)

// ItemNamePattern is an item's name, its stage directory's.
var ItemNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ItemAppPattern is the bundle an app item's dmg holds, as it lands in MacApplications.
var ItemAppPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,95}\.app$`)

// ItemDir is item name's stage directory.
func ItemDir(name string) string { return MacItemStageDir + "/" + name }

// itemDestRoots are where a file item may land (not /etc, whose files are the system's own), and
// itemDestRefused the paths under them it may not: the
// files a run's walls are made of, which a run's Init writes (macos_paths.go), Wardyn's own files, and the
// System keychain, which only a ca item changes. Paths compare without case, as the guest's APFS volume
// does.
var (
	itemDestRoots   = []string{"/Library", "/usr/local", "/opt"}
	itemDestRefused = []string{MacClaudeCodeManagedDir, MacCodexManagedDir, MacRunDir, MacItemStageDir, MacToolsBinDir,
		MacAgentBinaryPath, MacAgentPlistPath, "/Library/Keychains"}
)

// ErrItemDest is a file item's destination the guest does not allow.
var ErrItemDest = errors.New("not a place a base item may write")

// ItemDestAllowed says whether a file item may write dest, or make dest as its directory: an absolute,
// clean path strictly under an itemDestRoots entry and neither in nor above any itemDestRefused path.
// Harness managed settings are refused because a run writes them; an organisation's own go in its rules.
func ItemDestAllowed(dest string) error {
	if !filepath.IsAbs(dest) || filepath.Clean(dest) != dest {
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

// itemPushAllowed says whether a push may write dest for a base item: a staged file of its kind, or a
// file item's destination.
func itemPushAllowed(dest string) bool {
	if isItemDir(filepath.Dir(dest)) {
		return slices.Contains([]string{ItemPkg, ItemZip, ItemDmg, ItemProfile}, filepath.Base(dest))
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
}
