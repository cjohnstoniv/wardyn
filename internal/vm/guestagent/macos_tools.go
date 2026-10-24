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
	"strconv"
	"strings"

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

// ToolsExecAllowed says whether tools mode may run argv as root. argv[0] must be softwareupdate or
// launchctl; touch or rm on MacCLTTrigger only (touch T, rm -f T); exactly CLTGitArgv; or a regular file
// (not a link) directly in binDir, a pushed CLI's version check. A path is compared as given, so the
// same binary by a relative path, a ".." component or a link is refused, and nothing names a shell.
func ToolsExecAllowed(argv []string, binDir string) error {
	if len(argv) == 0 {
		return fmt.Errorf("%w: an exec needs argv", ErrToolsRefused)
	}
	name := argv[0]
	switch name {
	case "/usr/sbin/softwareupdate", "/bin/launchctl":
		return nil
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

// pushAllowed says whether a push may write dest: a file directly in BinDir, or the agent's own binary
// or plist.
func (t *Tools) pushAllowed(dest string) error {
	ok := filepath.IsAbs(dest) && filepath.Clean(dest) == dest &&
		((filepath.Dir(dest) == t.binDir() && !strings.HasPrefix(filepath.Base(dest), ".")) ||
			(t.AgentBinary != "" && dest == t.AgentBinary) || (t.AgentPlist != "" && dest == t.AgentPlist))
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
	case merr != nil || mode&^0o777 != 0:
		http.Error(w, "a push's mode is permission bits in octal", http.StatusBadRequest)
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
	run := t.run
	if run == nil {
		run = runCombined
	}
	out, code, err := run(r.Context(), in.Argv)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONAny(w, ToolsExecResult{Output: string(out), ExitCode: code})
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
