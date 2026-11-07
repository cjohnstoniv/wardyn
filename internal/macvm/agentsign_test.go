// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// The signing identity is made on the first call and read back on every later one: regenerating it would
// drop every Full Disk Access grant made for the old one. Its directory is 0700, the key 0600.
func TestAgentSigningIdentityIsMadeOnceAndKept(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent-signing")
	first, err := macvm.AgentSigningIdentity(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(filepath.Join(dir, "key.pem"))
	second, err := macvm.AgentSigningIdentity(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || second == nil || !bytes.Equal(first.Raw, second.Raw) {
		t.Fatal("the second call made a new certificate")
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "key.pem")); len(key) == 0 || !bytes.Equal(key, again) {
		t.Fatal("the second call rewrote the key")
	}
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "key.pem"): 0o600, filepath.Join(dir, "cert.pem"): 0o644} {
		fi, err := os.Stat(path)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: mode %v (%v), want %v", path, fi, err, want)
		}
	}
	blk, _ := pem.Decode(key)
	if blk == nil {
		t.Fatal("key.pem is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if rk, ok := k.(*rsa.PrivateKey); !ok || !rk.PublicKey.Equal(first.PublicKey) {
		t.Fatal("key.pem is not the certificate's key")
	}
}

// The certificate is a code signing certificate only: RSA-3072, self-signed, KeyUsage digitalSignature
// (critical), ExtKeyUsage codeSigning, about 20 years, the name that says whose it is.
func TestAgentSigningCertIsACodeSigningCert(t *testing.T) {
	cert, err := macvm.AgentSigningIdentity(context.Background(), filepath.Join(t.TempDir(), "agent-signing"))
	if err != nil {
		t.Fatal(err)
	}
	if cert == nil {
		t.Fatal("no certificate")
	}
	if pk, ok := cert.PublicKey.(*rsa.PublicKey); !ok || pk.N.BitLen() != 3072 {
		t.Fatalf("public key %T, want RSA-3072", cert.PublicKey)
	}
	if err := cert.CheckSignatureFrom(cert); err != nil {
		t.Fatalf("not self-signed: %v", err)
	}
	if cert.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Fatalf("key usage %v, want digitalSignature only", cert.KeyUsage)
	}
	if !slices.Equal(cert.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}) {
		t.Fatalf("ext key usage %v, want codeSigning only", cert.ExtKeyUsage)
	}
	critical := false
	for _, e := range cert.Extensions {
		if e.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 15}) {
			critical = e.Critical
		}
	}
	if !critical {
		t.Fatal("key usage is not critical")
	}
	if cert.IsCA {
		t.Fatal("the signing certificate says CA")
	}
	if cert.Subject.CommonName != "Wardyn guest agent signing (this Mac)" {
		t.Fatalf("CN %q", cert.Subject.CommonName)
	}
	if life := cert.NotAfter.Sub(cert.NotBefore); life < 19*365*24*time.Hour || life > 21*366*24*time.Hour {
		t.Fatalf("validity %v, want about 20 years", life)
	}
	sum := sha1.Sum(cert.Raw)
	want := `identifier "com.wardyn.guest-agent" and certificate root = H"` + hex.EncodeToString(sum[:]) + `"`
	if got := macvm.AgentRequirement(cert); got != want {
		t.Fatalf("requirement %q, want %q", got, want)
	}
}

// fakeSignExec records each command the host signer runs and answers it; dr is what `codesign -d -r-`
// prints after the designated requirement's arrow, fail fails the argv[0] it names.
type fakeSignExec struct {
	mu   sync.Mutex
	runs [][]string
	dr   string
	fail string
}

func (f *fakeSignExec) exec(_ context.Context, argv []string) ([]byte, error) {
	f.mu.Lock()
	f.runs = append(f.runs, slices.Clone(argv))
	f.mu.Unlock()
	if argv[0] == f.fail {
		return []byte("refused"), errors.New("exit status 1")
	}
	if argv[0] == "/usr/bin/codesign" && slices.Contains(argv, "-d") {
		return []byte("Executable=" + argv[len(argv)-1] + "\ndesignated => " + f.dr + "\n"), nil
	}
	if argv[0] == "/usr/bin/codesign" && slices.Contains(argv, "--sign") {
		p := argv[len(argv)-1]
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		return nil, os.WriteFile(p, append([]byte("signed:"), b...), 0o755)
	}
	return nil, nil
}

// match says whether argv is want, where "*" in want stands for any one argument.
func match(argv, want []string) bool {
	if len(argv) != len(want) {
		return false
	}
	for i := range want {
		if want[i] != "*" && want[i] != argv[i] {
			return false
		}
	}
	return true
}

// The host signer runs exactly the openssl, security and codesign commands the identity needs, in a
// temporary keychain it deletes, signs with the certificate's SHA-1 and the agent's identifier, and reads
// the designated requirement back. A requirement that is not the exact one makes the tools update push
// the ad hoc build and log a WARN; the record says ad_hoc.
func TestAgentSignArgvAndRequirementCheck(t *testing.T) {
	signDir := filepath.Join(t.TempDir(), "agent-signing")
	cert, err := macvm.AgentSigningIdentity(context.Background(), signDir)
	if err != nil {
		t.Fatal(err)
	}
	if cert == nil {
		t.Fatal("no certificate")
	}
	sum := sha1.Sum(cert.Raw)
	sha := strings.ToUpper(hex.EncodeToString(sum[:]))
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("new agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	fx := &fakeSignExec{dr: macvm.AgentRequirement(cert)}
	signer := &macvm.HostAgentSigner{Dir: signDir, Exec: fx.exec}
	sa, err := signer.SignAgent(context.Background(), bin)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(sa.Path); string(b) != "signed:new agent" || sa.Path == bin {
		t.Fatalf("signed copy %q at %s", b, sa.Path)
	}
	if b, _ := os.ReadFile(bin); string(b) != "new agent" {
		t.Fatal("the signer changed the helper's own agent binary")
	}
	if !bytes.Equal(sa.Cert, cert.Raw) {
		t.Fatal("the signed agent does not carry the identity's certificate")
	}
	want := [][]string{
		{"/usr/bin/openssl", "pkcs12", "-export", "-inkey", filepath.Join(signDir, "key.pem"), "-in", filepath.Join(signDir, "cert.pem"), "-out", "*", "-passout", "*"},
		{"/usr/bin/security", "create-keychain", "-p", "*", "*"},
		{"/usr/bin/security", "unlock-keychain", "-p", "*", "*"},
		{"/usr/bin/security", "import", "*", "-k", "*", "-P", "*", "-T", "/usr/bin/codesign"},
		{"/usr/bin/security", "set-key-partition-list", "-S", "apple-tool:,apple:", "-s", "-k", "*", "*"},
		{"/usr/bin/codesign", "--force", "--keychain", "*", "--identifier", "com.wardyn.guest-agent", "--sign", sha, sa.Path},
		{"/usr/bin/codesign", "-d", "-r-", sa.Path},
		{"/usr/bin/security", "delete-keychain", "*"},
	}
	if len(fx.runs) != len(want) {
		t.Fatalf("ran %d commands, want %d:\n%q", len(fx.runs), len(want), fx.runs)
	}
	for i := range want {
		if !match(fx.runs[i], want[i]) {
			t.Fatalf("command %d = %q, want %q", i, fx.runs[i], want[i])
		}
	}
	kc := fx.runs[1][4]
	if fx.runs[3][4] != kc || fx.runs[4][7] != kc || fx.runs[5][3] != kc || fx.runs[7][2] != kc {
		t.Fatalf("the commands name different keychains: %q", fx.runs)
	}
	if fx.runs[1][3] != fx.runs[2][3] || fx.runs[2][3] != fx.runs[4][6] || len(fx.runs[1][3]) < 32 {
		t.Fatalf("keychain password not one random value of 32 or more: %q", fx.runs)
	}
	if !strings.HasPrefix(fx.runs[0][10], "pass:") || fx.runs[0][10][len("pass:"):] != fx.runs[3][6] || len(fx.runs[3][6]) < 32 {
		t.Fatalf("the PKCS#12 password is not one random value of 32 or more: %q", fx.runs)
	}
	sa.Done()
	if _, err := os.Stat(sa.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Done left the signed copy")
	}

	// A requirement read back that is not the exact one is a signing failure, and so is any failing
	// command; the keychain is deleted either way.
	for _, fx := range []*fakeSignExec{{dr: `cdhash H"00"`}, {dr: macvm.AgentRequirement(cert), fail: "/usr/bin/openssl"}} {
		if _, err := (&macvm.HostAgentSigner{Dir: signDir, Exec: fx.exec}).SignAgent(context.Background(), bin); err == nil {
			t.Fatalf("dr %q fail %q signed", fx.dr, fx.fail)
		}
		if fx.fail == "" && fx.runs[len(fx.runs)-1][1] != "delete-keychain" {
			t.Fatalf("the keychain was not deleted: %q", fx.runs)
		}
	}

	// Through a tools update: the ad hoc build is pushed, a WARN is logged, and the result says ad_hoc.
	f, g := newTools(t), newGuest()
	var logs bytes.Buffer
	f.opts.Log = slog.New(slog.NewTextHandler(&logs, nil))
	f.opts.Agent = &macvm.AgentPin{Version: "0.10.1", Path: bin, Dest: guestagent.MacAgentBinaryPath}
	f.opts.Signer = &macvm.HostAgentSigner{Dir: signDir, Exec: (&fakeSignExec{dr: `cdhash H"00"`}).exec}
	var boots, stops atomic.Int64
	res := runStage(t, f.stage(g, &boots, &stops))
	if string(g.files[guestagent.MacAgentBinaryPath]) != "new agent" || string(g.files[guestagent.MacAgentRunPath]) != "new agent" {
		t.Fatal("a failed signing did not push the ad hoc build")
	}
	if res.AgentSignature != macvm.AgentSignedAdHoc || !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("signature %q, log:\n%s", res.AgentSignature, logs.String())
	}
}

// markSigner is a fake AgentSigner: its copy is the agent's bytes behind "signed:".
type markSigner struct {
	cert []byte
	done atomic.Int64
}

func (m *markSigner) SignAgent(_ context.Context, path string) (macvm.SignedAgent, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return macvm.SignedAgent{}, err
	}
	p := filepath.Join(filepath.Dir(path), "signed-copy")
	if err := os.WriteFile(p, append([]byte("signed:"), b...), 0o755); err != nil {
		return macvm.SignedAgent{}, err
	}
	return macvm.SignedAgent{Path: p, Cert: m.cert, Done: func() { m.done.Add(1); _ = os.Remove(p) }}, nil
}

// signGuest is the tools fake guest that also answers the agent's posture (the stage-2 boot's guest
// does), with Full Disk Access fda, and takes a push at any mode (the certificate's is 0644).
type signGuest struct {
	*guest
	fda       string
	certTrust int // verify-cert's exit for the signing certificate
}

func (g *signGuest) Posture(context.Context) (guestagent.ToolsPosture, error) {
	return guestagent.ToolsPosture{FullDiskAccess: g.fda}, nil
}

func (g *signGuest) Push(_ context.Context, dest string, _ fs.FileMode, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	g.mu.Lock()
	g.files[dest] = b
	g.mu.Unlock()
	return err
}

func (g *signGuest) Run(ctx context.Context, argv []string) (string, int, error) {
	if len(argv) > 2 && argv[1] == "verify-cert" && slices.Contains(argv, "codeSign") {
		g.mu.Lock()
		g.runs = append(g.runs, argv)
		g.mu.Unlock()
		return "", g.certTrust, nil
	}
	return g.guest.Run(ctx, argv)
}

func signCert() string {
	return guestagent.ItemDir(guestagent.AgentSigningItem) + "/" + guestagent.ItemCert
}

// A tools update pushes the signed copy to both the agent's path and the run launcher's, puts the signing
// certificate in the base and trusts it for code signing only, and records the signature, the
// certificate's trust and Full Disk Access as the agent read it.
func TestToolsUpdatePushesTheSignedAgent(t *testing.T) {
	f := newTools(t)
	g := &signGuest{guest: newGuest(), fda: "allowed"}
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("new agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	ms := &markSigner{cert: []byte("the cert's DER")}
	f.opts.Agent = &macvm.AgentPin{Version: "0.10.1", Path: bin, Dest: guestagent.MacAgentBinaryPath}
	f.opts.Signer = ms
	stage := &macvm.ToolsStage{ToolsOptions: f.opts, Boot: func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
		return g, func(context.Context) error { return nil }, nil
	}}
	res := runStage(t, stage)
	for _, p := range []string{guestagent.MacAgentBinaryPath, guestagent.MacAgentRunPath} {
		if string(g.files[p]) != "signed:new agent" {
			t.Fatalf("%s holds %q, want the signed copy", p, g.files[p])
		}
	}
	if ms.done.Load() != 1 {
		t.Fatalf("the signed copy was cleaned up %d times, want 1", ms.done.Load())
	}
	if string(g.files[signCert()]) != "the cert's DER" {
		t.Fatalf("signing certificate not in the base: %q", g.files[signCert()])
	}
	trust := []string{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "codeSign", "-k", guestagent.MacSystemKeychain, signCert()}
	verify := []string{"/usr/bin/security", "verify-cert", "-L", "-p", "codeSign", "-c", signCert()}
	if !slices.ContainsFunc(g.runs, func(a []string) bool { return slices.Equal(a, trust) }) ||
		!slices.ContainsFunc(g.runs, func(a []string) bool { return slices.Equal(a, verify) }) {
		t.Fatalf("the certificate was not trusted for code signing and checked: %q", g.runs)
	}
	if res.AgentSignature != macvm.AgentSignedThisMac || res.AgentSigningCert != macvm.ItemTrusted {
		t.Fatalf("signature %q cert %q", res.AgentSignature, res.AgentSigningCert)
	}
	if res.Guest == nil || res.Guest.FullDiskAccess != "allowed" {
		t.Fatalf("Full Disk Access not recorded: %+v", res.Guest)
	}

	// Through a prepare's record: the fields land in tools.json.
	pf, tf, _ := stage2Prep(t)
	tf.opts.Agent = &macvm.AgentPin{Version: "0.10.0", Path: bin, Dest: guestagent.MacAgentBinaryPath}
	tf.opts.Signer = ms
	pf.opts.Stage2 = &macvm.ToolsStage{ToolsOptions: tf.opts, Boot: func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
		return g, func(context.Context) error { return nil }, nil
	}}
	if _, err := macvm.Prepare(context.Background(), pf.opts); err != nil {
		t.Fatal(err)
	}
	rec, err := macvm.ReadToolsRecord(pf.opts.Layout.BaseDir(0))
	if err != nil || rec.AgentSignature != macvm.AgentSignedThisMac || rec.AgentSigningCert != macvm.ItemTrusted || rec.Guest == nil || rec.Guest.FullDiskAccess != "allowed" {
		t.Fatalf("record %+v %v", rec, err)
	}
}

// Where macOS refuses the trust without a person (macOS 26 does from root), the certificate reads not
// trusted, the signed agent is still pushed, and edit-base lists the command that trusts it for code
// signing in the admin Terminal.
func TestAgentSigningCertTrustFallsBackToEditBase(t *testing.T) {
	f := newTools(t)
	g := &signGuest{guest: newGuest(), certTrust: 1}
	bin := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(bin, []byte("new agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.opts.Agent = &macvm.AgentPin{Version: "0.10.1", Path: bin, Dest: guestagent.MacAgentBinaryPath}
	f.opts.Signer = &markSigner{cert: []byte("der")}
	stage := &macvm.ToolsStage{ToolsOptions: f.opts, Boot: func(context.Context, macvm.MachineSpec) (macvm.ToolsGuest, func(context.Context) error, error) {
		return g, func(context.Context) error { return nil }, nil
	}}
	res := runStage(t, stage)
	if res.AgentSignature != macvm.AgentSignedThisMac || res.AgentSigningCert != macvm.ItemNotTrusted || string(g.files[guestagent.MacAgentBinaryPath]) != "signed:new agent" {
		t.Fatalf("signature %q cert %q agent %q", res.AgentSignature, res.AgentSigningCert, g.files[guestagent.MacAgentBinaryPath])
	}
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, macvm.ToolsRecordFile), []byte(`{"agent_signature":"this_mac","agent_signing_cert":"not_trusted"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	(&macvm.EditOptions{Out: &out, Log: slog.New(slog.DiscardHandler)}).PrintUntrusted(base)
	want := fmt.Sprintf("sudo security add-trusted-cert -d -r trustRoot -p codeSign -k %s %s\n", guestagent.MacSystemKeychain, signCert())
	if !strings.Contains(out.String(), want) {
		t.Fatalf("edit-base did not list the signing certificate's trust:\n%s", out.String())
	}
	out.Reset()
	if err := os.WriteFile(filepath.Join(base, macvm.ToolsRecordFile), []byte(`{"agent_signature":"this_mac","agent_signing_cert":"trusted"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	(&macvm.EditOptions{Out: &out, Log: slog.New(slog.DiscardHandler)}).PrintUntrusted(base)
	if strings.Contains(out.String(), "codeSign") {
		t.Fatalf("a trusted signing certificate is listed:\n%s", out.String())
	}
}

// An install list cannot name an item after the signing certificate's stage directory.
func TestBaseListRefusesTheAgentSigningName(t *testing.T) {
	cer := filepath.Join(t.TempDir(), "x.cer")
	raw := fmt.Sprintf(`{"version":1,"items":[{"kind":"file","name":%q,"path":%q,"sha256":"%s","dest":"/usr/local/share/x"}]}`, guestagent.AgentSigningItem, cer, strings.Repeat("a", 64))
	if _, err := macvm.ParseBaseList([]byte(raw)); err == nil || !strings.Contains(err.Error(), guestagent.AgentSigningItem) {
		t.Fatalf("the reserved name was accepted: %v", err)
	}
}
