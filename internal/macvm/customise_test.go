// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// itemGuest is guest with pushes of any mode, recorded in order beside the runs, and item commands that
// fail when fail names them.
type itemGuest struct {
	*guest
	steps []string // "push <dest> <mode>" and "run <argv>", in order
	argvs [][]string
	fail  func(argv []string) int
}

func newItemGuest() *itemGuest { return &itemGuest{guest: newGuest()} }

// itemStage is a ToolsStage with f's options whose boot hands out g.
func (f *toolsFixture) itemStage(g *itemGuest) *macvm.ToolsStage {
	return &macvm.ToolsStage{ToolsOptions: f.opts,
		Boot: func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
			return g, func(context.Context) error { return nil }, nil
		}}
}

func (g *itemGuest) Push(_ context.Context, dest string, mode fs.FileMode, r io.Reader, size int64) error {
	b, err := io.ReadAll(r)
	if err != nil || int64(len(b)) != size {
		return fmt.Errorf("push %s: %d bytes of %d: %v", dest, len(b), size, err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.files[dest] = b
	g.steps = append(g.steps, fmt.Sprintf("push %s %o", dest, mode))
	return nil
}

func (g *itemGuest) Run(ctx context.Context, argv []string) (string, int, error) {
	if !strings.HasPrefix(argv[0], "/usr/bin/security") && !slices.Contains([]string{"/bin/mkdir", "/bin/rm", "/usr/sbin/installer", "/usr/bin/ditto", "/usr/bin/hdiutil"}, argv[0]) {
		return g.guest.Run(ctx, argv)
	}
	g.mu.Lock()
	g.steps = append(g.steps, "run "+strings.Join(argv, " "))
	g.argvs = append(g.argvs, argv)
	g.mu.Unlock()
	if g.fail != nil {
		return "", g.fail(argv), nil
	}
	return "", 0, nil
}

// itemFile writes body to a file in dir and returns its path and pin.
func itemFile(t *testing.T, dir, name, body string) (string, string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, shaHex([]byte(body))
}

func listJSON(t *testing.T, items ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"version": 1, "items": items})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var pin64 = strings.Repeat("a", 64)

func TestParseBaseListAcceptsEachKind(t *testing.T) {
	l, err := macvm.ParseBaseList(listJSON(t,
		map[string]any{"kind": "pkg", "name": "edr", "path": "/opt/x/edr.pkg", "sha256": pin64},
		map[string]any{"kind": "app", "name": "tool-zip", "path": "/opt/x/Tool.zip", "sha256": pin64},
		map[string]any{"kind": "app", "name": "tool-dmg", "path": "/opt/x/Tool.dmg", "sha256": pin64, "app": "Tool.app"},
		map[string]any{"kind": "file", "name": "conf", "path": "/opt/x/conf.json", "sha256": pin64, "dest": "/Library/Application Support/Org/conf.json"},
	))
	if err != nil || len(l.Items) != 4 {
		t.Fatalf("%v %+v", err, l)
	}
	if l.Digest() == "" || (macvm.BaseList{}).Digest() != "" {
		t.Fatal("a list has a digest and no list has none")
	}
}

func TestParseBaseListRefuses(t *testing.T) {
	ok := map[string]any{"kind": "pkg", "name": "edr", "path": "/opt/edr.pkg", "sha256": pin64}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for a, b := range ok {
			m[a] = b
		}
		m[k] = v
		return m
	}
	file := func(dest string) map[string]any {
		return map[string]any{"kind": "file", "name": "f", "path": "/opt/f", "sha256": pin64, "dest": dest}
	}
	for name, b := range map[string][]byte{
		"version 2":                []byte(`{"version":2,"items":[]}`),
		"an unknown field":         []byte(`{"version":1,"items":[{"kind":"pkg","name":"a","path":"/a.pkg","sha256":"` + pin64 + `","flags":"-allowUntrusted"}]}`),
		"an unknown kind":          listJSON(t, with("kind", "script")),
		"a bad name":               listJSON(t, with("name", "../x")),
		"a name twice":             listJSON(t, ok, ok),
		"a relative path":          listJSON(t, with("path", "edr.pkg")),
		"a short pin":              listJSON(t, with("sha256", "abc")),
		"a pkg not .pkg":           listJSON(t, with("path", "/opt/edr.zip")),
		"a pkg with dest":          listJSON(t, with("dest", "/Library/x")),
		"a dmg with no app":        listJSON(t, map[string]any{"kind": "app", "name": "a", "path": "/opt/a.dmg", "sha256": pin64}),
		"a zip with an app":        listJSON(t, map[string]any{"kind": "app", "name": "a", "path": "/opt/a.zip", "sha256": pin64, "app": "A.app"}),
		"a profile, no identifier": listJSON(t, map[string]any{"kind": "profile", "name": "p", "path": "/opt/p.mobileconfig", "sha256": pin64}),
		"a pkg with an identifier": listJSON(t, with("identifier", "com.example.x")),
		"a ca not a certificate":   listJSON(t, map[string]any{"kind": "ca", "name": "c", "path": "/opt/c.p12", "sha256": pin64}),
		"a secret on an app": listJSON(t, map[string]any{"kind": "app", "name": "a", "path": "/opt/a.zip", "sha256": pin64,
			"secret_files": []map[string]any{{"path": "/opt/k", "name": "k", "sha256": pin64}}}),
		"a secret named item.pkg": listJSON(t, with("secret_files", []map[string]any{{"path": "/opt/k", "name": "item.pkg", "sha256": pin64}})),
		"a secret with no pin":    listJSON(t, with("secret_files", []map[string]any{{"path": "/opt/k", "name": "k"}})),
		"a secret path relative":  listJSON(t, with("secret_files", []map[string]any{{"path": "k", "name": "k", "sha256": pin64}})),
		// A harness's managed settings are written per run (P0), so no item may write them.
		"claude managed settings":      listJSON(t, file(guestagent.MacClaudeCodeManagedSettingsPath)),
		"claude managed, another case": listJSON(t, file("/library/application support/claudecode/managed-settings.json")),
		"codex requirements":           listJSON(t, file(guestagent.MacCodexRequirementsPath)),
		"the run secrets dir":          listJSON(t, file(guestagent.MacRunDir+"/x")),
		"the stage dir":                listJSON(t, file(guestagent.MacItemStageDir+"/edr/item.pkg")),
		"a harness CLI":                listJSON(t, file("/usr/local/bin/claude")),
		"the agent":                    listJSON(t, file(guestagent.MacAgentBinaryPath)),
		"the system keychain":          listJSON(t, file("/Library/Keychains/System.keychain")),
		"outside the roots":            listJSON(t, file("/Users/agent/.zshrc")),
		"a root itself":                listJSON(t, file("/Library")),
		"not clean":                    listJSON(t, file("/Library/Org/../Keychains/x")),
	} {
		if _, err := macvm.ParseBaseList(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The argv sequence of each kind, in order, each one a form the guest agent allows, and every staged file
// in its item's directory.
func TestInstallItemsArgvSequence(t *testing.T) {
	f, dir := newTools(t), t.TempDir()
	pkg, pkgPin := itemFile(t, dir, "edr.pkg", "pkg-bytes")
	zip, zipPin := itemFile(t, dir, "Tool.zip", "zip-bytes")
	dmg, dmgPin := itemFile(t, dir, "Tool.dmg", "dmg-bytes")
	conf, confPin := itemFile(t, dir, "conf.json", "{}")
	l, err := macvm.ParseBaseList(listJSON(t,
		map[string]any{"kind": "pkg", "name": "edr", "path": pkg, "sha256": pkgPin},
		map[string]any{"kind": "app", "name": "zip", "path": zip, "sha256": zipPin},
		map[string]any{"kind": "app", "name": "dmg", "path": dmg, "sha256": dmgPin, "app": "Tool.app"},
		map[string]any{"kind": "file", "name": "conf", "path": conf, "sha256": confPin, "dest": "/Library/Application Support/Org/conf.json"},
	))
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Items = l
	g := newItemGuest()
	res := runStage(t, f.itemStage(g))
	S := guestagent.MacItemStageDir
	want := []string{
		"run /bin/mkdir -p -m 0700 " + S,
		"run /bin/mkdir -p -m 0700 " + S + "/edr", "push " + S + "/edr/item.pkg 600",
		"run /usr/sbin/installer -pkg " + S + "/edr/item.pkg -target /", "run /bin/rm -rf " + S + "/edr",
		"run /bin/mkdir -p -m 0700 " + S + "/zip", "push " + S + "/zip/item.zip 600",
		"run /usr/bin/ditto -x -k " + S + "/zip/item.zip /Applications", "run /bin/rm -rf " + S + "/zip",
		"run /bin/mkdir -p -m 0700 " + S + "/dmg", "push " + S + "/dmg/item.dmg 600",
		"run /usr/bin/hdiutil attach -nobrowse -readonly -noautoopen -mountpoint " + S + "/dmg/mnt " + S + "/dmg/item.dmg",
		"run /usr/bin/ditto " + S + "/dmg/mnt/Tool.app /Applications/Tool.app",
		"run /usr/bin/hdiutil detach -force " + S + "/dmg/mnt", "run /bin/rm -rf " + S + "/dmg",
		"run /bin/mkdir -p -m 0755 /Library/Application Support/Org", "push /Library/Application Support/Org/conf.json 644",
	}
	steps := g.steps[slices.Index(g.steps, want[0]):] // after the harnesses
	if !slices.Equal(steps, want) {
		t.Fatalf("steps:\n%s\nwant:\n%s", strings.Join(steps, "\n"), strings.Join(want, "\n"))
	}
	for _, r := range g.argvs {
		if err := guestagent.ToolsExecAllowed(r, guestagent.MacToolsBinDir); err != nil {
			t.Errorf("the guest refuses %q: %v", r, err)
		}
	}
	if len(res.Items) != 4 || res.Items[0] != (macvm.ItemRecord{Kind: "pkg", Name: "edr", SHA256: pkgPin, State: macvm.ItemInstalled}) || res.ItemsDigest != l.Digest() {
		t.Fatalf("items %+v digest %q", res.Items, res.ItemsDigest)
	}
}

// A file that does not match its pin is never pushed, and stops stage 2; an installer that fails stops it
// too, and its stage directory is still removed.
func TestInstallItemsFailures(t *testing.T) {
	dir := t.TempDir()
	pkg, pkgPin := itemFile(t, dir, "edr.pkg", "pkg-bytes")
	for name, tc := range map[string]struct {
		pin  string
		fail func([]string) int
	}{
		"pin mismatch":    {pin: pin64},
		"installer fails": {pin: pkgPin, fail: func(argv []string) int { return map[bool]int{true: 1}[argv[0] == "/usr/sbin/installer"] }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTools(t)
			f.opts.Items = macvm.BaseList{Version: 1, Items: []macvm.BaseItem{{Kind: "pkg", Name: "edr", Path: pkg, SHA256: tc.pin}}}
			g := newItemGuest()
			g.fail = tc.fail
			_, err := f.itemStage(g).Run(context.Background(), macvm.Stage2Input{})
			if err == nil || !strings.Contains(err.Error(), "base item edr (pkg)") {
				t.Fatalf("stage 2: %v", err)
			}
			if last := g.steps[len(g.steps)-1]; last != "run /bin/rm -rf "+guestagent.MacItemStageDir+"/edr" {
				t.Fatalf("the stage dir was not removed last: %q", g.steps)
			}
			if _, pushed := g.files[guestagent.MacItemStageDir+"/edr/item.pkg"]; pushed == (tc.fail == nil) {
				t.Fatalf("pushed %v with fail %v", pushed, tc.fail != nil)
			}
			if len(g.agentRuns) != 0 {
				t.Fatal("the self-test ran after a failed item")
			}
		})
	}
}

// The record keeps the list's digest; a changed list is a changed digest, which is what starts a tools update.
func TestItemsDigestRecordedAndCompared(t *testing.T) {
	dir := t.TempDir()
	pkg, pkgPin := itemFile(t, dir, "edr.pkg", "pkg-bytes")
	l := macvm.BaseList{Version: 1, Items: []macvm.BaseItem{{Kind: "pkg", Name: "edr", Path: pkg, SHA256: pkgPin}}}
	base := t.TempDir()
	if macvm.ItemsChanged(base, "") {
		t.Fatal("a base with no record was built with no list")
	}
	rec := macvm.ToolsRecord{ItemsDigest: l.Digest()}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	l2 := l
	l2.Items = []macvm.BaseItem{{Kind: "pkg", Name: "edr", Path: pkg, SHA256: pin64}}
	if macvm.ItemsChanged(base, l.Digest()) || !macvm.ItemsChanged(base, l2.Digest()) || !macvm.ItemsChanged(base, "") {
		t.Fatal("ItemsChanged")
	}
}

// profilesGuest answers `profiles list` with the identifiers in installed.
type profilesGuest struct {
	*itemGuest
	installed []string
}

func (g *profilesGuest) Run(ctx context.Context, argv []string) (string, int, error) {
	if slices.Equal(argv, []string{"/usr/bin/profiles", "list"}) {
		g.mu.Lock()
		g.argvs = append(g.argvs, argv)
		g.mu.Unlock()
		var b strings.Builder
		for i, id := range g.installed {
			fmt.Fprintf(&b, "_computerlevel[%d] attribute: name: x\n_computerlevel[%d] attribute: profileIdentifier: %s\n", i+1, i+1, id)
		}
		fmt.Fprintf(&b, "There are %d system configuration profiles installed\n", len(g.installed))
		return b.String(), 0, nil
	}
	return g.itemGuest.Run(ctx, argv)
}

// A profile is staged in its root-only directory and kept there; `profiles list` reads back whether it is
// installed. Until the person approves it in edit-base it is staged, and StagedProfiles names its file;
// ReadBackItems, edit-base's recheck, records it installed once it is.
func TestProfileItemsStagedForApproval(t *testing.T) {
	f, dir := newTools(t), t.TempDir()
	wifi, wifiPin := itemFile(t, dir, "wifi.mobileconfig", "<plist/>")
	vpn, vpnPin := itemFile(t, dir, "vpn.mobileconfig", "<plist>vpn</plist>")
	l, err := macvm.ParseBaseList(listJSON(t,
		map[string]any{"kind": "profile", "name": "wifi", "path": wifi, "sha256": wifiPin, "identifier": "com.example.wifi"},
		map[string]any{"kind": "profile", "name": "vpn", "path": vpn, "sha256": vpnPin, "identifier": "com.example.vpn"},
	))
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Items = l
	g := &profilesGuest{itemGuest: newItemGuest(), installed: []string{"com.example.vpn"}}
	s := f.itemStage(g.itemGuest)
	s.Boot = func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
		return g, func(context.Context) error { return nil }, nil
	}
	base := t.TempDir()
	res := runStage(t, s)
	S := guestagent.MacItemStageDir
	if slices.Contains(g.steps, "run /bin/rm -rf "+S+"/wifi") || !slices.Contains(g.steps, "push "+S+"/wifi/item.mobileconfig 600") {
		t.Fatalf("a profile is pushed to its stage dir and kept: %q", g.steps)
	}
	for _, r := range g.argvs {
		if err := guestagent.ToolsExecAllowed(r, guestagent.MacToolsBinDir); err != nil {
			t.Errorf("the guest refuses %q: %v", r, err)
		}
	}
	if res.Items[0].State != macvm.ItemStaged || res.Items[1].State != macvm.ItemInstalled || res.Items[0].Identifier != "com.example.wifi" {
		t.Fatalf("%+v", res.Items)
	}
	writeToolsRecord := func(rec macvm.ToolsRecord) {
		b, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(base, macvm.ToolsRecordFile), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeToolsRecord(macvm.ToolsRecord{Items: res.Items})
	if got, err := macvm.StagedProfiles(base); err != nil || !slices.Equal(got, []string{S + "/wifi/item.mobileconfig"}) {
		t.Fatalf("staged %q %v", got, err)
	}
	g.installed = append(g.installed, "com.example.wifi")
	if err := macvm.ReadBackItems(context.Background(), g, base); err != nil {
		t.Fatal(err)
	}
	rec, _ := macvm.ReadToolsRecord(base)
	if got, _ := macvm.StagedProfiles(base); len(got) != 0 || rec.Items[0].State != macvm.ItemInstalled {
		t.Fatalf("after approval: %q %+v", got, rec.Items)
	}
}

// caPEM writes a self-signed certificate, a CA or not, as PEM and returns its path, pin and DER.
func caPEM(t *testing.T, dir, name string, isCA bool) (string, string, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p, pin := itemFile(t, dir, name+".pem", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	return p, pin, der
}

// A ca item: staged and kept, trusted for SSL in the System keychain, then verified; its fingerprints are
// recorded. A verify that fails reads not_trusted, which is not an error. A tools update first removes a
// CA the list no longer has, with its trust settings, by its SHA-1, and the stage directory of an item the
// list no longer names. A certificate that is not a CA is refused.
func TestCAItemTrustVerifyAndRotation(t *testing.T) {
	dir := t.TempDir()
	ca, caPin, der := caPEM(t, dir, "org-ca", true)
	leaf, leafPin, _ := caPEM(t, dir, "leaf", false)
	S, K := guestagent.MacItemStageDir, guestagent.MacSystemKeychain
	sum, sum1 := sha256.Sum256(der), sha1.Sum(der)
	fp, fp1 := hex.EncodeToString(sum[:]), strings.ToUpper(hex.EncodeToString(sum1[:]))
	old := []macvm.ItemRecord{
		{Kind: "ca", Name: "org-ca", SHA256: pin64, State: macvm.ItemTrusted, Fingerprint: strings.Repeat("0", 64), SHA1: strings.Repeat("0A", 20)},
		{Kind: "profile", Name: "gone", SHA256: pin64, State: macvm.ItemStaged},
	}
	for name, verifyExit := range map[string]int{"trusted": 0, "not trusted": 1} {
		t.Run(name, func(t *testing.T) {
			f := newTools(t)
			f.opts.Items = macvm.BaseList{Version: 1, Items: []macvm.BaseItem{{Kind: "ca", Name: "org-ca", Path: ca, SHA256: caPin}}}
			g := newItemGuest()
			g.fail = func(argv []string) int { return map[bool]int{true: verifyExit}[argv[1] == "verify-cert"] }
			res, err := f.itemStage(g).Run(context.Background(), macvm.Stage2Input{Previous: old})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{
				"run /usr/bin/security delete-certificate -t -Z " + strings.Repeat("0A", 20) + " " + K,
				"run /bin/rm -rf " + S + "/gone",
				"run /bin/mkdir -p -m 0700 " + S,
				"run /bin/mkdir -p -m 0700 " + S + "/org-ca", "push " + S + "/org-ca/item.cer 600",
				"run /usr/bin/security add-trusted-cert -d -r trustRoot -p ssl -k " + K + " " + S + "/org-ca/item.cer",
				"run /usr/bin/security verify-cert -L -p ssl -c " + S + "/org-ca/item.cer",
			}
			steps := g.steps[slices.Index(g.steps, want[0]):]
			if !slices.Equal(steps, want) {
				t.Fatalf("steps:\n%s", strings.Join(steps, "\n"))
			}
			for _, r := range g.argvs {
				if err := guestagent.ToolsExecAllowed(r, guestagent.MacToolsBinDir); err != nil {
					t.Errorf("the guest refuses %q: %v", r, err)
				}
			}
			wantState := map[int]string{0: macvm.ItemTrusted, 1: macvm.ItemNotTrusted}[verifyExit]
			if r := res.Items[0]; r.State != wantState || r.Fingerprint != fp || r.SHA1 != fp1 {
				t.Fatalf("%+v", r)
			}
		})
	}
	f := newTools(t)
	f.opts.Items = macvm.BaseList{Version: 1, Items: []macvm.BaseItem{{Kind: "ca", Name: "leaf", Path: leaf, SHA256: leafPin}}}
	g := newItemGuest()
	if _, err := f.itemStage(g).Run(context.Background(), macvm.Stage2Input{}); err == nil || !strings.Contains(err.Error(), "not a CA certificate") {
		t.Fatalf("a leaf: %v", err)
	}
	if slices.ContainsFunc(g.steps, func(s string) bool { return strings.Contains(s, "add-trusted-cert") }) {
		t.Fatal("a leaf was trusted")
	}
}

// edit-base's recheck reads a CA's trust again: one the person trusted by hand reads trusted.
func TestReadBackItemsVerifiesTheCA(t *testing.T) {
	base := t.TempDir()
	b, _ := json.Marshal(macvm.ToolsRecord{Items: []macvm.ItemRecord{{Kind: "ca", Name: "org-ca", SHA256: pin64, State: macvm.ItemNotTrusted}}})
	if err := os.WriteFile(filepath.Join(base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := macvm.ReadBackItems(context.Background(), newItemGuest(), base); err != nil {
		t.Fatal(err)
	}
	if rec, _ := macvm.ReadToolsRecord(base); rec.Items[0].State != macvm.ItemTrusted {
		t.Fatalf("%+v", rec.Items)
	}
}

// A pkg item's secret file: pushed 0600 beside the pkg before the installer runs, removed with the stage
// directory after it on every path, and its value never in an argv, the log, the error, tools.json, or the
// list's digest. A secret file others can read is refused before anything is pushed.
func TestSecretFilesNeverLeak(t *testing.T) {
	const value = "LICENCE-VALUE-7Q2X9"
	dir := t.TempDir()
	pkg, pkgPin := itemFile(t, dir, "edr.pkg", "pkg-bytes")
	lic, licPin := itemFile(t, dir, "licence.key", value)
	S := guestagent.MacItemStageDir
	for name, tc := range map[string]struct {
		installExit int
		pin         string
		mode        os.FileMode
		wantErr     string
	}{
		"installed":         {pin: licPin, mode: 0o600},
		"installer fails":   {installExit: 1, pin: licPin, mode: 0o600, wantErr: "installer -pkg exited 1"},
		"pin mismatch":      {pin: pin64, mode: 0o600, wantErr: "does not match its pinned SHA-256"},
		"readable by group": {pin: licPin, mode: 0o640, wantErr: "chmod 600"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.Chmod(lic, tc.mode); err != nil {
				t.Fatal(err)
			}
			l, err := macvm.ParseBaseList(listJSON(t, map[string]any{"kind": "pkg", "name": "edr", "path": pkg, "sha256": pkgPin,
				"secret_files": []map[string]any{{"path": lic, "name": "com.vendor.registration-token", "sha256": tc.pin}}}))
			if err != nil {
				t.Fatal(err)
			}
			f := newTools(t)
			var logs strings.Builder
			f.opts.Items, f.opts.Log = l, slog.New(slog.NewTextHandler(&logs, nil))
			g := newItemGuest()
			g.fail = func(argv []string) int { return map[bool]int{true: tc.installExit}[argv[0] == "/usr/sbin/installer"] }
			res, err := f.itemStage(g).Run(context.Background(), macvm.Stage2Input{})
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("stage 2: %v", err)
			}
			pushed := slices.Index(g.steps, "push "+S+"/edr/com.vendor.registration-token 600")
			installer := slices.IndexFunc(g.steps, func(s string) bool { return strings.HasPrefix(s, "run /usr/sbin/installer") })
			if (tc.wantErr == "" || tc.installExit != 0) && (pushed < 0 || installer < pushed) {
				t.Fatalf("the secret is pushed before the installer: %q", g.steps)
			}
			if pushed >= 0 && g.steps[len(g.steps)-1] != "run /bin/rm -rf "+S+"/edr" {
				t.Fatalf("the stage dir, and the secret with it, was not removed last: %q", g.steps)
			}
			base := t.TempDir()
			if err := macvm.WriteToolsRecord(base, res, "1", time.Now()); err != nil {
				t.Fatal(err)
			}
			rec, _ := os.ReadFile(filepath.Join(base, macvm.ToolsRecordFile))
			errText := ""
			if err != nil {
				errText = err.Error()
			}
			for what, s := range map[string]string{"argv": fmt.Sprint(g.argvs), "steps": strings.Join(g.steps, "\n"), "log": logs.String(),
				"error": errText, "tools.json": string(rec), "digest": l.Digest()} {
				if strings.Contains(s, value) {
					t.Errorf("the secret's value is in the %s", what)
				}
			}
		})
	}
}

// postureGuest answers the posture reads from answers (argv joined by spaces); a missing one is a
// transport error.
type postureGuest struct {
	*itemGuest
	answers map[string]string
}

func (g *postureGuest) Run(ctx context.Context, argv []string) (string, int, error) {
	if out, ok := g.answers[strings.Join(argv, " ")]; ok {
		return out, 0, nil
	}
	if strings.HasPrefix(argv[0], "/usr/bin/sw_vers") || strings.HasPrefix(argv[0], "/usr/bin/csrutil") || argv[0] == "/usr/sbin/spctl" || argv[0] == "/usr/bin/profiles" {
		return "", 0, errors.New("gone")
	}
	return g.itemGuest.Run(ctx, argv)
}

// The guest posture, read at the end of stage 2 and recorded; every read is tolerant: output it does not
// know, or a read that fails, leaves its field empty and fails nothing.
func TestGuestPostureReads(t *testing.T) {
	two := 2
	for name, tc := range map[string]struct {
		answers map[string]string
		want    macvm.GuestPosture
	}{
		"a managed-looking guest": {answers: map[string]string{
			"/usr/bin/sw_vers -productVersion": "26.6.2\n", "/usr/bin/sw_vers -buildVersion": "25G83\n",
			"/usr/bin/csrutil status":                   "System Integrity Protection status: enabled.\n",
			"/usr/sbin/spctl --status":                  "assessments enabled\n",
			"/usr/bin/profiles status -type enrollment": "Enrolled via DEP: No\nMDM enrollment: Yes (User Approved)\n",
			"/usr/bin/profiles list": "_computerlevel[1] attribute: profileIdentifier: com.example.a\n" +
				"_computerlevel[2] attribute: profileIdentifier: com.example.b\nThere are 2 system configuration profiles installed\n",
		}, want: macvm.GuestPosture{OSVersion: "26.6.2", OSBuild: "25G83", SIP: "on", Gatekeeper: "on", MDM: "enrolled", Profiles: &two}},
		"off and unenrolled": {answers: map[string]string{
			"/usr/bin/csrutil status": "System Integrity Protection status: disabled.\n", "/usr/sbin/spctl --status": "assessments disabled\n",
			"/usr/bin/profiles status -type enrollment": "Enrolled via DEP: No\nMDM enrollment: No\n",
		}, want: macvm.GuestPosture{SIP: "off", Gatekeeper: "off", MDM: "not_enrolled"}},
		"nothing it knows": {answers: map[string]string{
			"/usr/bin/sw_vers -productVersion": "twenty-six; rm -rf /", "/usr/bin/sw_vers -buildVersion": "25G83 extra",
			"/usr/bin/csrutil status":  "System Integrity Protection status: unknown (Custom Configuration).",
			"/usr/sbin/spctl --status": "", "/usr/bin/profiles status -type enrollment": "Error",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newTools(t)
			f.opts.Tooling = macvm.Tooling{Version: 1}
			g := &postureGuest{itemGuest: newItemGuest(), answers: tc.answers}
			s := f.itemStage(g.itemGuest)
			s.Boot = func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
				return g, func(context.Context) error { return nil }, nil
			}
			res := runStage(t, s)
			got, _ := json.Marshal(res.Guest)
			want, _ := json.Marshal(tc.want)
			if string(got) != string(want) {
				t.Fatalf("posture %s, want %s", got, want)
			}
		})
	}
}

// edit-base's recheck reads the posture again; the organisation CA is trusted only when every ca item is.
func TestReadBackItemsRecordsThePosture(t *testing.T) {
	base := t.TempDir()
	b, _ := json.Marshal(macvm.ToolsRecord{Items: []macvm.ItemRecord{
		{Kind: "ca", Name: "root-a", SHA256: pin64, State: macvm.ItemTrusted}, {Kind: "ca", Name: "root-b", SHA256: pin64, State: macvm.ItemTrusted}}})
	if err := os.WriteFile(filepath.Join(base, macvm.ToolsRecordFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	g := newItemGuest()
	g.fail = func(argv []string) int {
		return map[bool]int{true: 1}[slices.Contains(argv, guestagent.ItemDir("root-a")+"/"+guestagent.ItemCert)]
	}
	if err := macvm.ReadBackItems(context.Background(), g, base); err != nil {
		t.Fatal(err)
	}
	if rec, _ := macvm.ReadToolsRecord(base); rec.Guest == nil || rec.Guest.OrgCA != macvm.ItemNotTrusted {
		t.Fatalf("%+v", rec.Guest)
	}
}
