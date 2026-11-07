// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
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
// enters a guest, so code in a run cannot sign a binary that meets the requirement. Nothing on the host
// trusts the certificate; the base trusts it for code signing only.

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
// them back. They are never made again while the certificate reads: a new one would drop every Full Disk
// Access grant given to the old one in this Mac's bases.
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

// readAgentSigningCert reads dir's certificate: os.ErrNotExist when the identity is not made yet.
func readAgentSigningCert(dir string) (*x509.Certificate, error) {
	b, err := os.ReadFile(filepath.Join(dir, agentSigningCert))
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, agentSigningKey)); err != nil {
		return nil, fmt.Errorf("macvm: the agent signing key beside its certificate: %w", err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("macvm: %s is not a PEM certificate", filepath.Join(dir, agentSigningCert))
	}
	return x509.ParseCertificate(blk.Bytes)
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

// SignedAgent is a signed copy of the guest agent: Path is the copy, Cert the signing certificate (DER)
// the base trusts for code signing, and Done removes the copy.
type SignedAgent struct {
	Path string
	Cert []byte
	Done func()
}

// HostAgentSigner signs the guest agent with this Mac's signing identity in Dir (Layout.AgentSigningDir),
// through openssl, security and codesign. Exec runs one command and returns its combined output (nil: on
// this host, which works on macOS only).
type HostAgentSigner struct {
	Dir  string
	Exec func(ctx context.Context, argv []string) ([]byte, error)
}

// SignAgent copies the agent at path into a new temp directory and signs the copy there. The identity
// goes into a keychain made for this one signing, with random one-use passwords, and the keychain is
// deleted again. The passwords are on the commands' argv while each runs; they open only that keychain
// and that PKCS#12 file, in a 0700 temp directory of this login's. The copy is signed without the
// hardened runtime (the agent needs none in the guest), and its designated requirement is read back: one
// that is not exactly AgentRequirement is a failure.
func (s *HostAgentSigner) SignAgent(ctx context.Context, path string) (sa SignedAgent, err error) {
	cert, err := AgentSigningIdentity(ctx, s.Dir)
	if err != nil {
		return sa, err
	}
	tmp, err := os.MkdirTemp("", "wardyn-agent-sign-")
	if err != nil {
		return sa, err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()
	cp := filepath.Join(tmp, filepath.Base(guestagent.MacAgentBinaryPath))
	if err := copyFile(ctx, path, cp); err != nil {
		return sa, err
	}
	if err := os.Chmod(cp, 0o755); err != nil {
		return sa, err
	}
	kcPW, p12PW := randomHex(32), randomHex(32)
	p12, kc := filepath.Join(tmp, "identity.p12"), filepath.Join(tmp, "signing.keychain-db")
	exec := s.Exec
	if exec == nil {
		exec = hostExec
	}
	run := func(argv ...string) (string, error) {
		out, err := exec(ctx, argv)
		if err != nil {
			return "", fmt.Errorf("macvm: signing the guest agent: %s %s: %w: %s", argv[0], argv[1], err, outputTail(string(out)))
		}
		return string(out), nil
	}
	if _, err := run("/usr/bin/openssl", "pkcs12", "-export", "-inkey", filepath.Join(s.Dir, agentSigningKey),
		"-in", filepath.Join(s.Dir, agentSigningCert), "-out", p12, "-passout", "pass:"+p12PW); err != nil {
		return sa, err
	}
	if _, err := run("/usr/bin/security", "create-keychain", "-p", kcPW, kc); err != nil {
		return sa, err
	}
	defer func() {
		if _, derr := run("/usr/bin/security", "delete-keychain", kc); err == nil && derr != nil {
			err = derr
		}
	}()
	for _, argv := range [][]string{
		{"/usr/bin/security", "unlock-keychain", "-p", kcPW, kc},
		{"/usr/bin/security", "import", p12, "-k", kc, "-P", p12PW, "-T", "/usr/bin/codesign"},
		{"/usr/bin/security", "set-key-partition-list", "-S", "apple-tool:,apple:", "-s", "-k", kcPW, kc},
		{"/usr/bin/codesign", "--force", "--keychain", kc, "--identifier", AgentSigningID, "--sign", agentCertSHA1(cert), cp},
	} {
		if _, err := run(argv...); err != nil {
			return sa, err
		}
	}
	out, err := run("/usr/bin/codesign", "-d", "-r-", cp)
	if err != nil {
		return sa, err
	}
	dr := ""
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "designated => "); ok {
			dr = v
		}
	}
	if want := AgentRequirement(cert); !strings.EqualFold(dr, want) {
		return sa, fmt.Errorf("macvm: signing the guest agent: its designated requirement reads %q, want %q", dr, want)
	}
	return SignedAgent{Path: cp, Cert: cert.Raw, Done: func() { _ = os.RemoveAll(tmp) }}, nil
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

// agentToPush is the guest agent build stage 2 pushes: o.Signer's signed copy, or the build as it is
// (signed ad hoc) when there is no signer or the signing failed. A failure is logged and never stops the
// update: the signature is not a security boundary, and refusing would only leave the base's tools behind.
func agentToPush(ctx context.Context, o ToolsOptions) (path string, signed *SignedAgent) {
	if o.Signer == nil {
		return o.Agent.Path, nil
	}
	sa, err := o.Signer.SignAgent(ctx, o.Agent.Path)
	if err != nil {
		o.Log.Warn("macvm: the guest agent was not signed on this Mac; pushing it signed ad hoc, so macOS drops its Full Disk Access at this update", "err", err.Error())
		return o.Agent.Path, nil
	}
	return sa.Path, &sa
}

// agentSigningCertPath is where the base keeps the guest agent's signing certificate.
func agentSigningCertPath() string {
	return guestagent.ItemDir(guestagent.AgentSigningItem) + "/" + guestagent.ItemCert
}

// trustAgentCert puts the signing certificate in the base, trusts it as a root for code signing only, and
// says whether the guest trusts it (ItemTrusted or ItemNotTrusted). macOS 26 refuses that trust from root
// without a person, and a guest agent from before this build refuses the command; either way the
// certificate reads not trusted and edit-base lists the command that trusts it. Only a failed push is an
// error.
func trustAgentCert(ctx context.Context, g ToolsGuest, der []byte, log *slog.Logger) (string, error) {
	cer := agentSigningCertPath()
	if err := runItem(ctx, g, "/bin/mkdir", "-p", "-m", "0700", filepath.Dir(cer)); err != nil {
		return "", fmt.Errorf("macvm: the agent signing certificate's directory: %w", err)
	}
	if err := g.Push(ctx, cer, 0o644, strings.NewReader(string(der)), int64(len(der))); err != nil {
		return "", fmt.Errorf("macvm: pushing the agent signing certificate: %w", err)
	}
	out, exit, err := g.Run(ctx, []string{"/usr/bin/security", "add-trusted-cert", "-d", "-r", "trustRoot", "-p", "codeSign", "-k", guestagent.MacSystemKeychain, cer})
	if err != nil || exit != 0 {
		log.Warn("macvm: the base did not trust the agent signing certificate; edit-base lists the command that does", "exit", exit, "err", fmt.Sprint(err), "out", outputTail(out))
	}
	return verifyAgentCert(ctx, g), nil
}

// verifyAgentCert is whether the guest trusts the agent's signing certificate for code signing.
func verifyAgentCert(ctx context.Context, g ToolsGuest) string {
	if _, exit, err := g.Run(ctx, []string{"/usr/bin/security", "verify-cert", "-L", "-p", "codeSign", "-c", agentSigningCertPath()}); err == nil && exit == 0 {
		return ItemTrusted
	}
	return ItemNotTrusted
}
