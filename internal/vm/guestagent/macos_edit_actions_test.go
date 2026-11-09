// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// editRig is a toolsRig whose edit-action files and staged items are in the test's own directories.
func newEditRig(t *testing.T) (*toolsRig, string, string) {
	t.Helper()
	rig := newToolsRig(t)
	rig.t.EditDir = filepath.Join(t.TempDir(), "wardyn-edit")
	rig.t.ItemStageDir = filepath.Join(t.TempDir(), "stage")
	if err := os.MkdirAll(rig.t.ItemStageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return rig, rig.t.EditDir, rig.t.ItemStageDir
}

// stageCA writes a self-signed CA with subject cn as stage/<name>/item.cer, DER or PEM, and returns
// its DER's SHA-256 in upper-case colon form.
func stageCA(t *testing.T, stage, name, cn string, asPEM bool) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	raw := der
	if asPEM {
		raw = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	if err := os.MkdirAll(filepath.Join(stage, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, name, ItemCert), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

func userArgv(uid uint32, argv ...string) []string {
	cmd, args := asUserHome(uid, argv...)
	return append([]string{cmd}, args...)
}

func readEditFile(t *testing.T, path string) (string, os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), fi.Mode().Perm()
}

// Open admin Terminal writes a one-line script that signs in as the setup admin (which asks for its
// password, the one-time code) and opens it in Terminal in the console user's session, as that user: a
// script opened as root would run `su` without a password. The request takes no input.
func TestEditAdminTerminalOpensSuInTheConsoleSession(t *testing.T) {
	rig, dir, _ := newEditRig(t)
	w := rig.do("POST", "/edit/admin-terminal", `{"argv":["/bin/sh"],"file":"/etc/passwd"}`)
	if w.Code != 200 {
		t.Fatalf("admin-terminal = %d %s", w.Code, w.Body)
	}
	var res EditActionResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil || !res.Opened {
		t.Fatalf("answer %s (%v)", w.Body, err)
	}
	script := filepath.Join(dir, "admin.command")
	body, mode := readEditFile(t, script)
	if want := "#!/bin/sh\nexec /usr/bin/su - " + MacAdminUser + "\n"; body != want || mode != 0o755 {
		t.Errorf("admin.command = %q mode %o", body, mode)
	}
	if want := [][]string{userArgv(502, "/usr/bin/open", "-a", "Terminal", script)}; !slices.EqualFunc(rig.ran, want, slices.Equal[[]string]) {
		t.Fatalf("ran %q, want %q", rig.ran, want)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the edit directory = %v %v, want 0755", fi, err)
	}
}

// With nobody signed in at the console there is no session to open Terminal in: nothing runs and the
// answer says why.
func TestEditActionsWithNobodyAtTheConsoleRunNothing(t *testing.T) {
	for _, path := range []string{"/edit/admin-terminal", "/edit/trust-cas", "/edit/full-disk-access"} {
		rig, dir, stage := newEditRig(t)
		stageCA(t, stage, "corp-ca", "Corp Root", false)
		rig.t.Desktop.Console = fakeConsole{err: ErrNoConsoleUser}
		w := rig.do("POST", path, "")
		if w.Code != 409 || len(rig.ran) != 0 {
			t.Errorf("%s with no console user = %d %s, ran %q", path, w.Code, w.Body, rig.ran)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("%s wrote %v with no console user", path, ents)
		}
	}
	rig := newToolsRig(t)
	rig.t.Desktop = nil
	if w := rig.do("POST", "/edit/full-disk-access", ""); w.Code != 409 || len(rig.ran) != 0 {
		t.Errorf("full-disk-access with no desktop = %d, ran %q", w.Code, rig.ran)
	}
}

// Trust the list's CAs writes, for each staged ca item in name order, its subject and SHA-256 and one
// sudo line that trusts that file as wardyn-admin, then opens the script in Terminal as the console user.
// A directory with no item.cer, and an item.cer that is a link, are not staged CAs. PEM and DER both read.
func TestEditTrustCAsWritesOneSudoLinePerStagedCA(t *testing.T) {
	rig, dir, stage := newEditRig(t)
	sumB := stageCA(t, stage, "b-corp", "Corp Issuing", true)
	sumA := stageCA(t, stage, "a-corp", "Corp Root", false)
	if err := os.MkdirAll(filepath.Join(stage, "tool"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "tool", ItemPkg), []byte("pkg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stage, "linked"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(stage, "a-corp", ItemCert), filepath.Join(stage, "linked", ItemCert)); err != nil {
		t.Fatal(err)
	}
	w := rig.do("POST", "/edit/trust-cas", "")
	var res EditActionResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &res) != nil || !res.Opened || res.Certificates != 2 {
		t.Fatalf("trust-cas = %d %s", w.Code, w.Body)
	}
	script := filepath.Join(dir, "trust-cas.command")
	body, mode := readEditFile(t, script)
	if mode != 0o755 || !strings.HasPrefix(body, "#!/bin/sh\n") {
		t.Fatalf("trust-cas.command mode %o:\n%s", mode, body)
	}
	var su []string
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "/usr/bin/su ") {
			su = append(su, l)
		}
	}
	line := func(name string) string {
		return "/usr/bin/su - " + MacAdminUser + " -c '/usr/bin/sudo /bin/launchctl asuser 502 /usr/bin/security add-trusted-cert -d -r trustRoot -p ssl -k " +
			MacSystemKeychain + " " + filepath.Join(stage, name, ItemCert) + "'"
	}
	if want := []string{line("a-corp"), line("b-corp")}; !slices.Equal(su, want) {
		t.Errorf("su lines:\n%q\nwant\n%q", su, want)
	}
	// What a person is asked to trust is printed before the sudo line asks: each subject and hash.
	var printed strings.Builder
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "printf ") {
			out, err := exec.Command("/bin/sh", "-c", l).Output()
			if err != nil {
				t.Fatalf("%q: %v", l, err)
			}
			printed.Write(out)
		}
	}
	for _, want := range []string{"a-corp", strconv.Quote("CN=Corp Root"), sumA, "b-corp", strconv.Quote("CN=Corp Issuing"), sumB} {
		if !strings.Contains(printed.String(), want) {
			t.Errorf("the script printed %q, not %q", printed.String(), want)
		}
	}
	if strings.Index(body, "a-corp") > strings.Index(body, "b-corp") {
		t.Error("the staged items are not in name order")
	}
	if want := [][]string{userArgv(502, "/usr/bin/open", "-a", "Terminal", script)}; !slices.EqualFunc(rig.ran, want, slices.Equal[[]string]) {
		t.Errorf("ran %q, want %q", rig.ran, want)
	}
}

// A subject is the CA's own text: one with a quote, a newline or a command in it is printed quoted and
// never becomes a line of the script.
func TestEditTrustCAsPrintsAHostileSubjectAsText(t *testing.T) {
	rig, dir, stage := newEditRig(t)
	cn := "x'; touch /tmp/pwned; echo '\n/usr/bin/su - root"
	stageCA(t, stage, "evil", cn, false)
	if w := rig.do("POST", "/edit/trust-cas", ""); w.Code != 200 {
		t.Fatalf("trust-cas = %d %s", w.Code, w.Body)
	}
	body, _ := readEditFile(t, filepath.Join(dir, "trust-cas.command"))
	var cmds []string
	for _, l := range strings.Split(body, "\n") {
		if l != "" && !strings.HasPrefix(l, "#!") && !strings.HasPrefix(l, "printf ") {
			cmds = append(cmds, l)
		}
	}
	if len(cmds) != 1 || !strings.HasPrefix(cmds[0], "/usr/bin/su - "+MacAdminUser+" -c '/usr/bin/sudo /bin/launchctl asuser 502 /usr/bin/security add-trusted-cert ") {
		t.Fatalf("script commands %q", cmds)
	}
	for _, l := range strings.Split(body, "\n") {
		if !strings.HasPrefix(l, "printf ") {
			continue
		}
		out, err := exec.Command("/bin/sh", "-c", l).Output()
		if err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		if strings.Contains(string(out), "\n/usr/bin/su - root") {
			t.Errorf("a newline in the subject started a line: %q", out)
		}
	}
}

// With no staged ca item the request is refused, writes nothing and runs nothing: the window disables the
// button with "The install list has no certificates."
func TestEditTrustCAsWithNoCAIsRefused(t *testing.T) {
	rig, dir, stage := newEditRig(t)
	if err := os.MkdirAll(filepath.Join(stage, "tool"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "tool", ItemPkg), []byte("pkg"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []bool{false, true} {
		if missing {
			rig.t.ItemStageDir = filepath.Join(t.TempDir(), "none")
		}
		w := rig.do("POST", "/edit/trust-cas", "")
		if w.Code != 409 || !strings.Contains(w.Body.String(), "no certificates") {
			t.Errorf("trust-cas with no staged CA (stage missing %v) = %d %s", missing, w.Code, w.Body)
		}
		if len(rig.ran) != 0 {
			t.Errorf("ran %q", rig.ran)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Error("the edit directory was made for a refused request")
		}
	}
	// A staged file that is not a certificate is skipped, not trusted.
	if err := os.WriteFile(filepath.Join(stage, "tool", ItemCert), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	rig.t.ItemStageDir = stage
	if w := rig.do("POST", "/edit/trust-cas", ""); w.Code != 409 || len(rig.ran) != 0 {
		t.Errorf("trust-cas with a non-certificate = %d %s, ran %q", w.Code, w.Body, rig.ran)
	}
}

// Full Disk Access opens the Privacy pane in the console session, then shows the agent in Finder. The
// first pane address that exits non-zero falls back to the second; if neither opens, the agent is not
// revealed and the request fails.
func TestEditFullDiskAccessOpensThePaneAndRevealsTheAgent(t *testing.T) {
	const first = "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"
	const second = "com.apple.settings.PrivacySecurity.extension?Privacy_AllFiles"
	open := func(arg string) []string { return userArgv(502, "/usr/bin/open", arg) }
	reveal := userArgv(502, "/usr/bin/open", "-R", "/usr/local/libexec/wardyn-guest-agent")

	rig, dir, _ := newEditRig(t)
	rig.t.AgentBinary = "" // the installed agent's own path
	if w := rig.do("POST", "/edit/full-disk-access", `{"url":"file:///"}`); w.Code != 200 {
		t.Fatalf("full-disk-access = %d %s", w.Code, w.Body)
	}
	if want := [][]string{open(first), reveal}; !slices.EqualFunc(rig.ran, want, slices.Equal[[]string]) {
		t.Errorf("ran %q, want %q", rig.ran, want)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("full-disk-access wrote %v", ents)
	}

	rig, _, _ = newEditRig(t)
	rig.t.AgentBinary = "/opt/agent"
	rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
		rig.ran = append(rig.ran, argv)
		if slices.Equal(argv, open(first)) {
			return []byte("The file does not exist"), 1, nil
		}
		return nil, 0, nil
	}
	if w := rig.do("POST", "/edit/full-disk-access", ""); w.Code != 200 {
		t.Fatalf("full-disk-access, first address refused = %d %s", w.Code, w.Body)
	}
	if want := [][]string{open(first), open(second), userArgv(502, "/usr/bin/open", "-R", "/opt/agent")}; !slices.EqualFunc(rig.ran, want, slices.Equal[[]string]) {
		t.Errorf("fallback ran %q, want %q", rig.ran, want)
	}

	rig, _, _ = newEditRig(t)
	rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
		rig.ran = append(rig.ran, argv)
		return nil, 1, nil
	}
	if w := rig.do("POST", "/edit/full-disk-access", ""); w.Code != 500 || len(rig.ran) != 2 {
		t.Errorf("neither address opening = %d, ran %q; want 500 after the two tries and no reveal", w.Code, rig.ran)
	}

	rig, _, _ = newEditRig(t)
	rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
		rig.ran = append(rig.ran, argv)
		return nil, 0, errors.New("launchctl failed")
	}
	if w := rig.do("POST", "/edit/full-disk-access", ""); w.Code != 500 {
		t.Errorf("a failed run = %d, want 500", w.Code)
	}
}

// The actions are handlers, not new allowed argv: tools exec still refuses the argv they run, and the
// actions answer POST only.
func TestEditActionsAddNoExecFormAndTakePostOnly(t *testing.T) {
	rig, dir, _ := newEditRig(t)
	for _, argv := range [][]string{
		{"/usr/bin/open", "-a", "Terminal", filepath.Join(dir, "admin.command")},
		{"/usr/bin/open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"},
		{"/usr/bin/su", "-", MacAdminUser},
		{"/bin/launchctl", "asuser", "502", "/usr/bin/open", "-a", "Terminal", "/tmp/x.command"},
		{"/bin/sh", filepath.Join(dir, "trust-cas.command")},
	} {
		if err := ToolsExecAllowed(argv, rig.bin); err == nil {
			t.Errorf("tools exec allows %q", argv)
		}
		if w := rig.exec(argv...); w.Code != 403 {
			t.Errorf("exec %q = %d, want 403", argv, w.Code)
		}
	}
	for _, path := range []string{"/edit/admin-terminal", "/edit/trust-cas", "/edit/full-disk-access"} {
		if w := rig.do("GET", path, ""); w.Code != 405 {
			t.Errorf("GET %s = %d, want 405", path, w.Code)
		}
	}
	if len(rig.ran) != 0 {
		t.Errorf("ran %q", rig.ran)
	}
}

// The edit scripts live in the directory stage 1 makes root:wheel 0755 for the pairing key, not under
// /private/var/run, which a stock guest has as root:daemon 0775. Laid out with those modes under a test
// root, the new parents pass the root-only write walk and the script is written; a parent that a group the
// walk does not trust can change, as daemon's /private/var/run, is refused, and the answer says why.
func TestEditScriptsLiveUnderARootOnlyParent(t *testing.T) {
	if MacEditDir != "/Library/Wardyn/edit" || filepath.Dir(MacEditDir) != filepath.Dir(MacPairKeyPath) {
		t.Fatalf("MacEditDir = %s, want /Library/Wardyn/edit, beside the pairing key", MacEditDir)
	}
	root := t.TempDir()
	mkdirMode(t, mkdirMode(t, filepath.Join(root, "Library"), 0o755)+"/Wardyn", 0o755)
	rig, _, _ := newEditRig(t)
	rig.t.EditDir = root + MacEditDir
	if w := rig.do("POST", "/edit/admin-terminal", ""); w.Code != 200 {
		t.Fatalf("admin-terminal under %s = %d %s", rig.t.EditDir, w.Code, w.Body)
	}
	// The directory and the script are root's (the tests' stand-in) and 0755: the console user reads and
	// runs the script, and no group can write either.
	for _, p := range []string{rig.t.EditDir, filepath.Join(rig.t.EditDir, EditAdminTerminalScript)} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st := fi.Sys().(*syscall.Stat_t); st.Uid != trustedOwnerUID || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s is uid %d mode %o, want root's 0755", p, st.Uid, fi.Mode().Perm())
		}
	}
	// A script directory a group can write is refused, even a group the walk trusts for a parent (admin).
	if err := os.Chmod(rig.t.EditDir, 0o775); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(rig.t.EditDir)
	if err != nil {
		t.Fatal(err)
	}
	setVar(t, &trustedGroupGIDs, []uint32{0, 80, fi.Sys().(*syscall.Stat_t).Gid})
	rig.ran = nil
	if w := rig.do("POST", "/edit/admin-terminal", ""); w.Code != 500 || !strings.Contains(w.Body.String(), "is not root's alone") || len(rig.ran) != 0 {
		t.Errorf("admin-terminal with a group-writable script directory = %d %q, ran %q", w.Code, w.Body, rig.ran)
	}
	trustedGroupGIDs = []uint32{0, 80}

	run := mkdirMode(t, mkdirMode(t, mkdirMode(t, filepath.Join(root, "private"), 0o755)+"/var", 0o755)+"/run", 0o775)
	if fi, err = os.Stat(run); err != nil {
		t.Fatal(err)
	}
	if gid := fi.Sys().(*syscall.Stat_t).Gid; slices.Contains(trustedGroupGIDs, gid) {
		t.Skipf("the test's group %d is one the walk trusts", gid)
	}
	rig.ran = nil
	rig.t.EditDir = filepath.Join(run, "wardyn-edit")
	w := rig.do("POST", "/edit/admin-terminal", "")
	if body := w.Body.String(); w.Code != 500 || !strings.HasPrefix(body, "the Terminal script was not written: ") ||
		!strings.Contains(body, "is writable by gid") || !strings.Contains(body, ErrUnsafeParent.Error()) {
		t.Errorf("admin-terminal under a group-writable parent = %d %q", w.Code, body)
	}
	if len(rig.ran) != 0 {
		t.Errorf("ran %q after the script was refused", rig.ran)
	}
}

// The trust line runs security as root through `launchctl asuser` for the console user, so macOS can show
// the dialog that approves the trust in that user's GUI session; `su` alone leaves it no session to show it
// in. The line is exact, with the console user's own uid.
func TestEditTrustCAsRunsSecurityInTheConsoleSession(t *testing.T) {
	got := (&Tools{}).trustScript([]stagedCA{{name: "corp", subject: "CN=Corp Root", sha256: "AA:BB"}}, 503)
	want := "/usr/bin/su - wardyn-admin -c '/usr/bin/sudo /bin/launchctl asuser 503 /usr/bin/security add-trusted-cert" +
		" -d -r trustRoot -p ssl -k /Library/Keychains/System.keychain /Library/Wardyn/stage/corp/item.cer'\n"
	if !strings.Contains(got, want) || strings.Count(got, "add-trusted-cert") != 1 {
		t.Errorf("trust script:\n%s\nwant the line\n%s", got, want)
	}

	rig, dir, stage := newEditRig(t)
	stageCA(t, stage, "corp", "Corp Root", false)
	rig.t.Desktop.Console = fakeConsole{uid: 503}
	if w := rig.do("POST", "/edit/trust-cas", ""); w.Code != 200 {
		t.Fatalf("trust-cas = %d %s", w.Code, w.Body)
	}
	body, _ := readEditFile(t, filepath.Join(dir, EditTrustCAsScript))
	if !strings.Contains(body, " -c '/usr/bin/sudo /bin/launchctl asuser 503 /usr/bin/security add-trusted-cert ") {
		t.Errorf("the trust line is not for the console user 503:\n%s", body)
	}
}

// The window's close shuts the guest down as root with shutdown(8), never through a dialog: the agent
// answers 202 and flushes it before the shutdown runs, so the host has its answer before the agent is
// stopped. A shutdown that fails still answers 202 (the window's next close asks the VM).
func TestEditShutDownAnswersThenHaltsAsRoot(t *testing.T) {
	for _, code := range []int{0, 1} {
		rig := newToolsRig(t)
		w := httptest.NewRecorder()
		var ran [][]string
		rig.t.run = func(_ context.Context, argv []string) ([]byte, int, error) {
			if !w.Flushed || w.Code != http.StatusAccepted {
				t.Errorf("%q ran before the 202 was flushed (code %d, flushed %v)", argv, w.Code, w.Flushed)
			}
			ran = append(ran, argv)
			return nil, code, nil
		}
		rig.t.ToolsHandler().ServeHTTP(w, httptest.NewRequest("POST", "/edit/shut-down", strings.NewReader(`{"argv":["/bin/sh"]}`)))
		if w.Code != http.StatusAccepted {
			t.Errorf("shut-down (exit %d) = %d %s", code, w.Code, w.Body)
		}
		if want := [][]string{{"/sbin/shutdown", "-h", "now"}}; !slices.EqualFunc(ran, want, slices.Equal[[]string]) {
			t.Errorf("shut-down ran %q, want %q", ran, want)
		}
	}
	rig := newToolsRig(t)
	if w := rig.do("GET", "/edit/shut-down", ""); w.Code != 405 || len(rig.ran) != 0 {
		t.Errorf("GET shut-down = %d, ran %q", w.Code, rig.ran)
	}
	if err := ToolsExecAllowed([]string{"/sbin/shutdown", "-h", "now"}, rig.bin); err == nil {
		t.Error("tools exec allows the shutdown argv")
	}
}

// Over the tools session the host gets the guest's own words: a failure in the guest is ErrEditFailed with
// its reason, never a bare status, and the shut-down answer is accepted.
func TestEditActionClientSurfacesTheGuestsError(t *testing.T) {
	rig, _, _ := newEditRig(t)
	ww := mkdirMode(t, filepath.Join(t.TempDir(), "ww"), 0o777)
	rig.t.EditDir = filepath.Join(ww, "wardyn-edit")
	c := rawGuest(t, rig.t.ToolsHandler())
	ctx := context.Background()
	_, err := c.ToolsEditAdminTerminal(ctx)
	if !errors.Is(err, ErrEditFailed) || errors.Is(err, ErrEditRefused) || !strings.Contains(err.Error(), "the Terminal script was not written: ") ||
		!strings.Contains(err.Error(), "is world-writable") {
		t.Errorf("admin-terminal under a world-writable parent: %v", err)
	}
	nobody := newToolsRig(t)
	nobody.t.Desktop.Console = fakeConsole{err: ErrNoConsoleUser}
	if _, err := rawGuest(t, nobody.t.ToolsHandler()).ToolsEditTrustCAs(ctx); !errors.Is(err, ErrEditRefused) || errors.Is(err, ErrEditFailed) {
		t.Errorf("trust-cas with nobody at the console: %v", err)
	}
	if err := c.ToolsEditShutDown(ctx); err != nil {
		t.Errorf("shut-down: %v", err)
	}
	old := rawGuest(t, http.NotFoundHandler())
	if err := old.ToolsEditShutDown(ctx); !errors.Is(err, ErrNoExec) {
		t.Errorf("shut-down on an agent without it: %v, want ErrNoExec", err)
	}
}
