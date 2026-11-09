// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// The guest agent keeps Full Disk Access across tools updates only if every build it gets has the same
// designated requirement. An ad hoc signature's requirement is the build's own hash, so macOS drops the
// grant at each update. The helper therefore signs each build, before it pushes it, with a code signing
// identity made once on this Mac: the requirement names the identifier and this Mac's certificate, and
// stays the same for every build. The key stays on the host, in the login's VM directory, and never
// enters a guest, so code in a run cannot sign a binary that meets the requirement. Nothing trusts the
// certificate, on the host or in the base: the requirement is a hash match and needs no trust.

// AgentSigningID is the identifier every guest agent build is signed with.
const AgentSigningID = "com.wardyn.guest-agent"

// How the guest agent a stage 2 pushed is signed (ToolsRecord.AgentSignature).
const (
	// AgentSignedThisMac: with this Mac's signing identity, so the Full Disk Access grant can last.
	AgentSignedThisMac = "this_mac"
	// AgentSignedAdHoc: as built, ad hoc, so macOS drops the grant at each tools update.
	AgentSignedAdHoc = "ad_hoc"
)

const (
	agentSigningKey  = "key.pem"
	agentSigningCert = "cert.pem"
	agentSigningCN   = "Wardyn guest agent signing (this Mac)"
)

// AgentSigningIdentity is this Mac's guest agent signing certificate, kept in dir (0700) with its key
// (0600). The first caller makes them, under a lock on dir so two helpers make one; every later call reads
// them back. Only a missing certificate makes a new identity: a new one would drop every Full Disk Access
// grant given to the old one in this Mac's bases, so a certificate whose key is missing or unreadable is
// refused and kept, and the person decides to start over.
func AgentSigningIdentity(ctx context.Context, dir string) (*x509.Certificate, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("macvm: the agent signing directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("macvm: the agent signing directory: %w", err)
	}
	unlock, err := lockFile(ctx, filepath.Join(dir, ".lock"), true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cert, err := readAgentSigningCert(dir)
	if !errors.Is(err, os.ErrNotExist) {
		return cert, err
	}
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: agentSigningCN},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	// The key first: the certificate is what says the identity is made.
	if err := writeFileAtomic(filepath.Join(dir, agentSigningKey), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, agentSigningCert), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// readAgentSigningCert reads dir's certificate and checks its key is there and is the certificate's. It
// returns os.ErrNotExist only when the certificate is missing (the identity is not made yet); any other
// failure keeps both files and says how to start over.
func readAgentSigningCert(dir string) (*x509.Certificate, error) {
	b, err := os.ReadFile(filepath.Join(dir, agentSigningCert))
	if err != nil {
		return nil, err
	}
	var cert *x509.Certificate
	if blk, _ := pem.Decode(b); blk == nil || blk.Type != "CERTIFICATE" {
		err = fmt.Errorf("%s is not a PEM certificate", filepath.Join(dir, agentSigningCert))
	} else if cert, err = x509.ParseCertificate(blk.Bytes); err == nil {
		err = checkAgentSigningKey(filepath.Join(dir, agentSigningKey), cert)
	}
	if err != nil {
		// %v, not %w: a missing key must not read as an identity not made yet.
		return nil, fmt.Errorf("macvm: this Mac's guest agent signing identity in %s does not read (%v); it is kept, and the agent is signed ad hoc. To start over, remove %s; then grant Full Disk Access again in edit-base", dir, err, dir)
	}
	return cert, nil
}

// checkAgentSigningKey says whether the key at path reads and is cert's.
func checkAgentSigningKey(path string, cert *x509.Certificate) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return fmt.Errorf("%s is not a PEM key", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	if rk, ok := key.(*rsa.PrivateKey); !ok || !rk.PublicKey.Equal(cert.PublicKey) {
		return fmt.Errorf("%s is not the certificate's key", path)
	}
	return nil
}

// writeFileAtomic writes b to path, at mode, through a temp file and a rename.
func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// AgentRequirement is the designated requirement of a guest agent signed with cert: the identifier, and
// the certificate's SHA-1 as the root of its chain.
func AgentRequirement(cert *x509.Certificate) string {
	sum := sha1.Sum(cert.Raw)
	return `identifier "` + AgentSigningID + `" and certificate root = H"` + hex.EncodeToString(sum[:]) + `"`
}

// AgentSigner signs a copy of the guest agent build at path.
type AgentSigner interface {
	SignAgent(ctx context.Context, path string) (SignedAgent, error)
}

// SignedAgent is a signed copy of the guest agent: Path is the copy, Cert the signing certificate (DER),
// and Done removes the copy. Signer is the signer that made it
// (AgentSignerWardyn or AgentSignerCodesign), and Errors why each signer tried before it failed, path-free.
type SignedAgent struct {
	Path   string
	Cert   []byte
	Done   func()
	Signer string
	Errors []string
}

// HostAgentSigner signs the guest agent with this Mac's signing identity in Dir (Layout.AgentSigningDir):
// in this process first (machosign), then through openssl, security and codesign. Exec runs one command and
// returns its combined output (nil: on this host, which works on macOS only).
type HostAgentSigner struct {
	Dir  string
	Exec func(ctx context.Context, argv []string) ([]byte, error)
	// Log gets a failed keychain cleanup (nil: slog's default).
	Log *slog.Logger
}

// SignAgent copies the agent at path into a new temp directory and signs the copy there: with Wardyn's own
// signer first, then with codesign. Each signed copy must pass this Mac's codesign --verify --strict and read
// back exactly AgentRequirement as its designated requirement before it is used; a copy that does not moves on
// to the next signer. When none signs, the error is an *AgentSignError listing why each failed, path-free.
// The copy is signed without the hardened runtime: the agent needs none in the guest.
func (s *HostAgentSigner) SignAgent(ctx context.Context, path string) (sa SignedAgent, err error) {
	var se AgentSignError
	tmp, err := os.MkdirTemp("", "wardyn-agent-sign-")
	if err != nil {
		se.add("", err)
		return sa, &se
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()
	cert, err := AgentSigningIdentity(ctx, s.Dir)
	if err != nil {
		se.add("", err)
		return sa, &se
	}
	exec := s.Exec
	if exec == nil {
		exec = hostExec
	}
	run := func(argv ...string) (string, error) {
		out, err := exec(ctx, argv)
		if err != nil {
			return "", fmt.Errorf("macvm: signing the guest agent: %s %s: %w: %s", filepath.Base(argv[0]), argv[1], err, outputTail(string(out)))
		}
		return string(out), nil
	}
	cp := filepath.Join(tmp, filepath.Base(guestagent.MacAgentBinaryPath))
	for _, sg := range []struct {
		name string
		sign func() error
	}{
		{AgentSignerWardyn, func() error { return s.signInProcess(cert, path, cp) }},
		{AgentSignerCodesign, func() error { return s.signCodesign(ctx, exec, run, cert, path, cp, tmp) }},
	} {
		err := sg.sign()
		if err == nil {
			err = proveSigned(run, cp, cert)
		}
		if err == nil {
			return SignedAgent{Path: cp, Cert: cert.Raw, Done: func() { _ = os.RemoveAll(tmp) }, Signer: sg.name, Errors: se.Errors}, nil
		}
		se.add(sg.name, err)
		if ctx.Err() != nil {
			break
		}
	}
	return sa, &se
}

// signCodesign signs a fresh copy of path at cp with codesign, through a keychain made for this one signing,
// with random one-use passwords, and deleted again. The passwords are on the commands' argv while each runs;
// they open only that keychain and that PKCS#12 file, in a 0700 temp directory of this login's. codesign looks
// the identity up on the login's search list, so the keychain goes first on that list for the call, and the
// list read before is put back after it, under its own bounded context, before the keychain is deleted. The
// designated requirement is passed as text: without it codesign derives one naming the leaf certificate.
func (s *HostAgentSigner) signCodesign(ctx context.Context, exec func(context.Context, []string) ([]byte, error), run func(...string) (string, error), cert *x509.Certificate, path, cp, tmp string) error {
	_ = os.Remove(cp)
	if err := copyFile(ctx, path, cp); err != nil {
		return err
	}
	if err := os.Chmod(cp, 0o755); err != nil {
		return err
	}
	kcPW, p12PW := randomHex(32), randomHex(32)
	p12, kc := filepath.Join(tmp, "identity.p12"), filepath.Join(tmp, "signing.keychain-db")
	if _, err := run("/usr/bin/openssl", "pkcs12", "-export", "-inkey", filepath.Join(s.Dir, agentSigningKey),
		"-in", filepath.Join(s.Dir, agentSigningCert), "-out", p12, "-passout", "pass:"+p12PW); err != nil {
		return err
	}
	_, cerr := run("/usr/bin/security", "create-keychain", "-p", kcPW, kc)
	// A create the update's cancel cut short may have made the keychain all the same.
	if cerr == nil || ctx.Err() != nil {
		defer s.deleteKeychain(ctx, exec, kc)
	}
	if cerr != nil {
		return cerr
	}
	out, err := run("/usr/bin/security", "list-keychains", "-d", "user")
	if err != nil {
		return err
	}
	list := keychainList(out)
	_, err = run(append([]string{"/usr/bin/security", "list-keychains", "-d", "user", "-s", kc}, list...)...)
	// Like the create, a set the cancel cut short may have taken effect.
	if err == nil || ctx.Err() != nil {
		defer s.restoreKeychains(ctx, exec, list)
	}
	if err != nil {
		return err
	}
	for _, argv := range [][]string{
		{"/usr/bin/security", "unlock-keychain", "-p", kcPW, kc},
		{"/usr/bin/security", "import", p12, "-k", kc, "-P", p12PW, "-T", "/usr/bin/codesign"},
		{"/usr/bin/security", "set-key-partition-list", "-S", "apple-tool:,apple:", "-s", "-k", kcPW, kc},
		// One "=" only: "-r=" takes the rest as requirement text.
		{"/usr/bin/codesign", "--force", "--keychain", kc, "--identifier", AgentSigningID, "--sign", agentCertSHA1(cert), "-r=designated => " + AgentRequirement(cert), cp},
	} {
		if _, err := run(argv...); err != nil {
			return err
		}
	}
	return nil
}

// sameRequirement is whether requirement text got is want: everything up to the certificate hash exactly, as
// macOS compares the identifier, and the hash's hex in either case.
func sameRequirement(got, want string) bool {
	gp, gh, ok := strings.Cut(got, `H"`)
	wp, wh, _ := strings.Cut(want, `H"`)
	return ok && gp == wp && strings.EqualFold(gh, wh)
}

// proveSigned is this Mac's own check of a signed copy: codesign --verify --strict passes, and the designated
// requirement reads back as exactly AgentRequirement. Neither needs the certificate trusted.
func proveSigned(run func(...string) (string, error), cp string, cert *x509.Certificate) error {
	if _, err := run("/usr/bin/codesign", "--verify", "--strict", cp); err != nil {
		return err
	}
	out, err := run("/usr/bin/codesign", "-d", "-r-", cp)
	if err != nil {
		return err
	}
	dr := ""
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "designated => "); ok {
			dr = v
		}
	}
	if want := AgentRequirement(cert); !sameRequirement(dr, want) {
		return fmt.Errorf("macvm: signing the guest agent: its designated requirement reads %q, want %q", dr, want)
	}
	return nil
}

// deleteKeychain deletes the signing's keychain, which also takes it off the login's search list. It runs
// under its own bounded context, so a cancelled update still cleans up, and a failure is logged whatever
// the signing's own outcome: the signed copy does not need the keychain.
func (s *HostAgentSigner) deleteKeychain(ctx context.Context, exec func(context.Context, []string) ([]byte, error), kc string) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if out, err := exec(dctx, []string{"/usr/bin/security", "delete-keychain", kc}); err != nil {
		cmp.Or(s.Log, slog.Default()).Warn("macvm: the guest agent signing keychain was not deleted", "keychain", kc, "err", err.Error(), "out", outputTail(string(out)))
	}
}

// agentCertSHA1 is cert's SHA-1 as codesign --sign names an identity: upper-case hex.
func agentCertSHA1(cert *x509.Certificate) string {
	sum := sha1.Sum(cert.Raw)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return hex.EncodeToString(b)
}
