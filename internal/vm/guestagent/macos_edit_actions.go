// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The edit-base window's guest actions, served in tools mode beside the requests macos_tools.go lists. The
// tools session exists only on a base boot of the host's own, so a run's guest never reaches them. Each
// takes no input (a body is ignored). Each but shut-down answers EditActionResult and acts in the console
// user's session as that user, never as root: a script that Terminal ran as root would reach `su` without a
// password. Admin rights still need the setup admin's one-time code, typed by the person. Shut-down answers
// 202 and halts the guest as root, which the host can already do to the VM.
//
//	POST /edit/admin-terminal      open Terminal at `su - wardyn-admin`
//	POST /edit/trust-cas           open Terminal at one `sudo security add-trusted-cert` per staged ca item
//	POST /edit/full-disk-access    open Privacy & Security > Full Disk Access and show this agent in Finder
//	POST /edit/shut-down           shut the guest down as root, with no dialog (the window's close)
//
// A request with nobody at the console, or (trust-cas) no staged certificate, is refused with 409 and
// does nothing; one that fails answers 500 with the guest's reason. These are handlers, not allowed exec
// argv: ToolsExecAllowed is unchanged.
const (
	// MacEditDir holds the scripts the actions write: root's, mode 0755, in the directory stage 1 makes
	// root:wheel 0755 for the pairing key, so the console user can read and run them and no one but root can
	// change them or their parents. Not under /private/var/run, which a stock guest has as root:daemon 0775:
	// the root-only write walk (checkDir) refuses a parent the daemon group can change.
	MacEditDir = "/Library/Wardyn/edit"
	// EditAdminTerminalScript and EditTrustCAsScript are the files in MacEditDir.
	EditAdminTerminalScript = "admin.command"
	EditTrustCAsScript      = "trust-cas.command"

	// The two System Settings addresses of the Full Disk Access pane: macOS 13 and later, tried in this order.
	fdaPane         = "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"
	fdaPaneFallback = "com.apple.settings.PrivacySecurity.extension?Privacy_AllFiles"

	maxStagedCert = 1 << 20
)

// EditActionResult is what an edit action did. Certificates is the certificates trust-cas wrote a line
// for; Revealed says whether full-disk-access showed this agent in Finder (the pane is open either way).
type EditActionResult struct {
	Opened       bool `json:"opened"`
	Certificates int  `json:"certificates,omitempty"`
	Revealed     bool `json:"revealed,omitempty"`
	// Installed says install-rosetta ran to the end (macos_edit_rosetta.go).
	Installed bool `json:"installed,omitempty"`
}

// ErrEditRefused is an edit action the guest refused with nothing done: nobody at the console, or no
// staged certificate to trust.
var ErrEditRefused = errors.New("guestagent: edit action refused")

// ErrEditFailed is an edit action the guest agent tried and that failed there; it wraps the guest's reason.
var ErrEditFailed = errors.New("guestagent: edit action failed in the guest")

func (t *Tools) editDir() string { return cmp.Or(t.EditDir, MacEditDir) }

func (t *Tools) itemStageDir() string { return cmp.Or(t.ItemStageDir, MacItemStageDir) }

// consoleUID is the console user, or answers 409 and says false.
func (t *Tools) consoleUID(w http.ResponseWriter, r *http.Request) (uint32, bool) {
	if t.Desktop == nil || t.Desktop.Console == nil {
		http.Error(w, "nobody is signed in at the guest's console", http.StatusConflict)
		return 0, false
	}
	uid, err := t.Desktop.Console.Console(r.Context())
	switch {
	case errors.Is(err, ErrNoConsoleUser):
		http.Error(w, "nobody is signed in at the guest's console", http.StatusConflict)
		return 0, false
	case err != nil:
		slog.Warn("guest agent: tools mode: edit action: console user", "err", err)
		http.Error(w, "the guest's console user was not read", http.StatusInternalServerError)
		return 0, false
	}
	return uid, true
}

// runAsConsole runs argv in the console user's session as that user and says whether it exited 0.
func (t *Tools) runAsConsole(ctx context.Context, uid uint32, argv ...string) bool {
	run := t.run
	if run == nil {
		run = runCombined
	}
	cmd, args := asUserHome(uid, argv...)
	_, code, err := run(ctx, append([]string{cmd}, args...))
	return err == nil && code == 0
}

// writeEditScript writes content as MacEditDir's file name: root's, 0755, through the same
// link-refusing walk as a push. Terminal opens it by path, as the console user, who reads and runs it and
// can write neither it nor its directory. The walk trusts a parent the admin group can change; the
// directory itself must be root's alone, writable by no group.
func (t *Tools) writeEditScript(name, content string) (string, error) {
	dir := t.editDir()
	if err := makeDirAll(dir, 0o755, nil); err != nil {
		return "", err
	}
	var st unix.Stat_t
	if err := unix.Lstat(dir, &st); err != nil {
		return "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o022 != 0 || (st.Uid != 0 && st.Uid != trustedOwnerUID) {
		return "", fmt.Errorf("%q is not root's alone (uid %d, mode %o): %w", dir, st.Uid, st.Mode&0o7777, ErrUnsafeParent)
	}
	path := filepath.Join(dir, name)
	return path, replaceFile(path, 0o755, func(f *os.File) error {
		_, err := io.WriteString(f, content)
		return err
	})
}

// openInTerminal opens script in Terminal in the console user's session.
func (t *Tools) openInTerminal(ctx context.Context, uid uint32, script string) bool {
	return t.runAsConsole(ctx, uid, "/usr/bin/open", "-a", "Terminal", script)
}

func (t *Tools) handleEditAdminTerminal(w http.ResponseWriter, r *http.Request) {
	uid, ok := t.consoleUID(w, r)
	if !ok {
		return
	}
	script, err := t.writeEditScript(EditAdminTerminalScript, "#!/bin/sh\nexec /usr/bin/su - "+MacAdminUser+"\n")
	if err != nil {
		slog.Warn("guest agent: tools mode: admin terminal script not written", "err", err)
		http.Error(w, "the Terminal script was not written: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !t.openInTerminal(r.Context(), uid, script) {
		http.Error(w, "Terminal was not opened", http.StatusInternalServerError)
		return
	}
	writeJSONAny(w, EditActionResult{Opened: true})
}

// stagedCA is one ca item staged in the guest: its name, certificate subject and SHA-256.
type stagedCA struct{ name, subject, sha256 string }

// stagedCAs lists the stage directory's ca items in name order: a directory named as an item whose
// item.cer is a regular file holding one certificate, PEM or DER. Anything else is skipped, and so is
// AgentSigningItem, the agent's own signing certificate, which no one is asked to trust.
func (t *Tools) stagedCAs() []stagedCA {
	ents, err := os.ReadDir(t.itemStageDir())
	if err != nil {
		return nil
	}
	var out []stagedCA
	for _, e := range ents {
		if !e.IsDir() || !ItemNamePattern.MatchString(e.Name()) || e.Name() == AgentSigningItem {
			continue
		}
		if c, ok := t.stagedCert(e.Name()); ok {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b stagedCA) int { return strings.Compare(a.name, b.name) })
	return out
}

// stagedCert reads item name's staged certificate: a regular item.cer holding one certificate, PEM or DER.
func (t *Tools) stagedCert(name string) (stagedCA, bool) {
	p := filepath.Join(t.itemStageDir(), name, ItemCert)
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() || fi.Size() > maxStagedCert {
		return stagedCA{}, false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return stagedCA{}, false
	}
	der := raw
	if blk, _ := pem.Decode(raw); blk != nil {
		der = blk.Bytes
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return stagedCA{}, false
	}
	sum := sha256.Sum256(der)
	return stagedCA{name, cert.Subject.String(), colonHex(sum[:])}, true
}

func colonHex(b []byte) string {
	h := strings.ToUpper(hex.EncodeToString(b))
	parts := make([]string, 0, len(h)/2)
	for i := 0; i+1 < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// shellQuote is s as one single-quoted shell word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// trustScript prints each ca item's name, subject and SHA-256, then trusts it for SSL as the setup admin.
// A subject is the CA's own text, so it is printed quoted (strconv.Quote leaves no newline) and as a
// printf argument, never as shell.
//
// macOS asks a person to approve each admin trust, in a dialog it can show only in a GUI session. From the
// shell `su` opens, which pam_launchd moves into wardyn-admin's own launchd namespace, the trust is denied
// with "no user interaction possible". So root runs security through `launchctl asuser` for consoleUID,
// the account whose Terminal this is: that keeps root's credentials and adopts the user's bootstrap and
// security audit session (launchctl(1)), where the dialog can show on the guest's screen.
func (t *Tools) trustScript(cas []stagedCA, consoleUID uint32) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	line := func(c stagedCA, label string) {
		fmt.Fprintf(&b, "printf '%%s\\n' '' %s %s %s\n", shellQuote(label), shellQuote("  subject  "+strconv.Quote(c.subject)), shellQuote("  SHA-256  "+c.sha256))
		fmt.Fprintf(&b, "/usr/bin/su - %s -c '/usr/bin/sudo /bin/launchctl asuser %d /usr/bin/security add-trusted-cert -d -r trustRoot -p ssl -k %s %s'\n",
			MacAdminUser, consoleUID, MacSystemKeychain, filepath.Join(t.itemStageDir(), c.name, ItemCert))
	}
	for _, c := range cas {
		line(c, c.name)
	}
	b.WriteString("printf '%s\\n' '' 'Finished. You can close this window.'\n")
	return b.String()
}

func (t *Tools) handleEditTrustCAs(w http.ResponseWriter, r *http.Request) {
	uid, ok := t.consoleUID(w, r)
	if !ok {
		return
	}
	cas := t.stagedCAs()
	if len(cas) == 0 {
		http.Error(w, "the install list has no certificates staged", http.StatusConflict)
		return
	}
	script, err := t.writeEditScript(EditTrustCAsScript, t.trustScript(cas, uid))
	if err != nil {
		slog.Warn("guest agent: tools mode: trust script not written", "err", err)
		http.Error(w, "the Terminal script was not written: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if !t.openInTerminal(r.Context(), uid, script) {
		http.Error(w, "Terminal was not opened", http.StatusInternalServerError)
		return
	}
	writeJSONAny(w, EditActionResult{Opened: true, Certificates: len(cas)})
}

func (t *Tools) handleEditFullDiskAccess(w http.ResponseWriter, r *http.Request) {
	uid, ok := t.consoleUID(w, r)
	if !ok {
		return
	}
	if !t.runAsConsole(r.Context(), uid, "/usr/bin/open", fdaPane) && !t.runAsConsole(r.Context(), uid, "/usr/bin/open", fdaPaneFallback) {
		http.Error(w, "System Settings was not opened at Full Disk Access", http.StatusInternalServerError)
		return
	}
	revealed := t.runAsConsole(r.Context(), uid, "/usr/bin/open", "-R", cmp.Or(t.AgentBinary, MacAgentBinaryPath))
	writeJSONAny(w, EditActionResult{Opened: true, Revealed: revealed})
}

// shutDownArgv is the edit window's close: a halt by root through shutdown(8), which loginwindow does not
// confirm. Virtualization's stop request presses the guest's power button instead, and macOS then asks the
// person "Are you sure you want to shut down your computer now?".
var shutDownArgv = []string{"/sbin/shutdown", "-h", "now"}

// handleEditShutDown answers 202 and flushes it before it shuts the guest down, so the host has its answer
// before launchd stops this agent. A shutdown that does not start is only logged: the window's next close
// asks the VM instead.
func (t *Tools) handleEditShutDown(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusAccepted)
	_ = http.NewResponseController(w).Flush()
	run := t.run
	if run == nil {
		run = runCombined
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), time.Minute)
	defer cancel()
	if out, code, err := run(ctx, shutDownArgv); err != nil || code != 0 {
		slog.Warn("guest agent: tools mode: the guest did not shut down", "exit", code, "err", err, "output", strings.TrimSpace(string(out)))
	}
}

// ToolsEditShutDown asks the guest agent to shut the guest down; it returns once the guest has accepted.
func (c *Client) ToolsEditShutDown(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/edit/shut-down", nil)
	if err != nil {
		return err
	}
	hres, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer hres.Body.Close()
	if hres.StatusCode == http.StatusAccepted {
		return nil
	}
	return statusErr(hres)
}

// ToolsEditAdminTerminal, ToolsEditTrustCAs and ToolsEditFullDiskAccess ask the guest for the edit window's
// action of that name. A refusal with nothing done (nobody at the console, no staged certificate) is
// ErrEditRefused and a failure in the guest ErrEditFailed, each wrapping the guest's words.
func (c *Client) ToolsEditAdminTerminal(ctx context.Context) (EditActionResult, error) {
	return c.editAction(ctx, "/edit/admin-terminal")
}

func (c *Client) ToolsEditTrustCAs(ctx context.Context) (EditActionResult, error) {
	return c.editAction(ctx, "/edit/trust-cas")
}

func (c *Client) ToolsEditFullDiskAccess(ctx context.Context) (EditActionResult, error) {
	return c.editAction(ctx, "/edit/full-disk-access")
}

func (c *Client) editAction(ctx context.Context, path string) (EditActionResult, error) {
	var res EditActionResult
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, nil)
	if err != nil {
		return res, err
	}
	hres, err := c.hc.Do(req)
	if err != nil {
		return res, err
	}
	defer hres.Body.Close()
	if hres.StatusCode == http.StatusConflict || hres.StatusCode >= http.StatusInternalServerError {
		msg, _ := io.ReadAll(io.LimitReader(hres.Body, 1024))
		kind := ErrEditFailed
		if hres.StatusCode == http.StatusConflict {
			kind = ErrEditRefused
		}
		return res, fmt.Errorf("%w: %s", kind, strings.TrimSpace(string(msg)))
	}
	if err := statusErr(hres); err != nil {
		return res, err
	}
	return res, json.NewDecoder(io.LimitReader(hres.Body, MaxFrame)).Decode(&res)
}
