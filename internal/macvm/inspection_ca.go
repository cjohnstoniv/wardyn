// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package macvm

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	inspectionCAKey  = "key.pem"
	inspectionCACert = "ca.pem"
	inspectionCACN   = "Wardyn inspection CA (this Mac)"
	// inspectionCAYears is the CA's validity. On macOS 26 a new CA takes a new base (prepare --new-image).
	inspectionCAYears = 5
	// crossSignMax is the longest a re-issued run CA is valid from the moment it is signed, whatever window the
	// run CA carries. A macOS VM run has no fixed maximum lifetime, so one that outlives it fails TLS to its
	// Inspected hosts in apps that use only the system trust store; a run with a lifetime cap is held to that.
	crossSignMax = 7 * 24 * time.Hour
)

// InspectionCADir is where this Mac's inspection CA is kept (OpenInspectionCA).
func (l Layout) InspectionCADir() string { return filepath.Join(l.Root, "inspection-ca") }

// InspectionCA is this Mac's inspection CA (the runner setting macos_vm_inspection_ca): a root the Mac's bases
// trust once, by hand in edit-base, that re-issues a macOS VM run's proxy CA (CrossSign) so an app that uses
// only the system trust store accepts the proxy's certificates. Its key stays in the Mac's VM directory: no
// proxy, guest or control plane is given it.
type InspectionCA struct {
	cert *x509.Certificate
	key  crypto.Signer
	dir  string
}

// OpenInspectionCA reads this Mac's inspection CA from dir, and makes it when dir holds no certificate. dir,
// the key and the certificate must be this login's and not symlinks, with modes 0700, 0600 and 0644. Anything
// else is refused and kept: a key another user could read may already have signed. To start over, remove dir,
// then trust the new CA in edit-base. Only the runner calls it; the macos-vm commands use ReadInspectionCA.
func OpenInspectionCA(ctx context.Context, dir string) (*InspectionCA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("macvm: the inspection CA directory: %w", err)
	}
	if err := inspectionCAOwned(dir, 0o700); err != nil {
		return nil, fmt.Errorf("macvm: the inspection CA directory: %w; it is kept and not used. To start over, remove it, then trust the new CA in macos-vm edit-base", err)
	}
	unlock, err := lockFile(ctx, filepath.Join(dir, ".lock"), true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	ca, err := readInspectionCA(dir)
	if !errors.Is(err, os.ErrNotExist) {
		return ca, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: inspectionCACN, Organization: []string{"Wardyn"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(inspectionCAYears, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLen:            1, // the run CA it re-issues signs only leaves
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	// The key first: the certificate is what says the CA is made.
	if err := writeFileAtomic(filepath.Join(dir, inspectionCAKey), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(dir, inspectionCACert), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	return readInspectionCA(dir)
}

// ReadInspectionCA reads this Mac's inspection CA from dir with OpenInspectionCA's checks, and never makes or
// changes one. It is for the macos-vm commands: only the runner makes and removes the CA, since a command may
// resolve another config than the runner's. An error that is os.ErrNotExist means there is no CA yet.
func ReadInspectionCA(ctx context.Context, dir string) (*InspectionCA, error) {
	if err := inspectionCAOwned(dir, 0o700); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("macvm: the inspection CA directory: %w; it is kept and not used", err)
	}
	unlock, err := lockFile(ctx, filepath.Join(dir, ".lock"), false)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return readInspectionCA(dir)
}

// RemoveInspectionCA deletes this Mac's inspection CA in dir, the key first, and says whether there was one.
// It holds OpenInspectionCA's lock, so a remove and a make never interleave and leave a certificate without its
// key. The runner calls it when it starts with the setting off, or on while one of its drive roots holds dir: a
// key left on disk is one a run's mount could read. A base that trusted the CA keeps the trust of a root nothing
// can sign for any more.
func RemoveInspectionCA(ctx context.Context, dir string) (bool, error) {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	release, err := lockFile(ctx, filepath.Join(dir, ".lock"), true)
	if err != nil {
		return false, err
	}
	defer release()
	removed := false
	for _, name := range []string{inspectionCAKey, inspectionCACert} {
		switch err := os.Remove(filepath.Join(dir, name)); {
		case err == nil:
			removed = true
		case !errors.Is(err, os.ErrNotExist):
			return removed, fmt.Errorf("macvm: remove this Mac's inspection CA: %w", err)
		}
	}
	return removed, nil
}

// readInspectionCA reads dir's certificate and key and checks they are one pair, owned as OpenInspectionCA
// says. It returns os.ErrNotExist only when the certificate is missing (the CA is not made yet); any other
// failure keeps both files and says how to start over.
func readInspectionCA(dir string) (*InspectionCA, error) {
	if _, err := os.Lstat(filepath.Join(dir, inspectionCACert)); errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ca, err := parseInspectionCA(dir)
	if err != nil {
		// %v, not %w: a missing key must not read as a CA not made yet.
		return nil, fmt.Errorf("macvm: this Mac's inspection CA in %s does not read (%v); it is kept and not used. To start over, remove %s, then trust the new CA in macos-vm edit-base", dir, err, dir)
	}
	return ca, nil
}

// parseInspectionCA is readInspectionCA's check of the two files.
func parseInspectionCA(dir string) (*InspectionCA, error) {
	certPath, keyPath := filepath.Join(dir, inspectionCACert), filepath.Join(dir, inspectionCAKey)
	if err := inspectionCAOwned(certPath, 0o644); err != nil {
		return nil, err
	}
	if err := inspectionCAOwned(keyPath, 0o600); err != nil {
		return nil, err
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !cert.IsCA || !ok {
		return nil, errors.New("the certificate is not a CA with a signing key")
	}
	return &InspectionCA{cert: cert, key: signer, dir: dir}, nil
}

// inspectionCAOwned refuses path unless it is this login's, not a symlink, and has exactly perm.
func inspectionCAOwned(path string, perm os.FileMode) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink", path)
	case !ok || int(st.Uid) != os.Getuid():
		return fmt.Errorf("%s is not this login's", path)
	case fi.Mode().Perm() != perm:
		return fmt.Errorf("%s has mode %#o, not %#o", path, fi.Mode().Perm(), perm)
	}
	return nil
}

// CertPath is the CA certificate's file, which the install list's InspectionCAItem names.
func (c *InspectionCA) CertPath() string { return filepath.Join(c.dir, inspectionCACert) }

// PEM is the CA certificate.
func (c *InspectionCA) PEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw}))
}

// Fingerprint is the CA's SHA-256 fingerprint, of its DER, as tools.json records a ca item's.
func (c *InspectionCA) Fingerprint() string {
	sum := sha256.Sum256(c.cert.Raw)
	return hex.EncodeToString(sum[:])
}

// Expired says whether this Mac's CA is past its validity at now. It then re-issues nothing (CrossSign), and
// a run takes today's path.
func (c *InspectionCA) Expired(now time.Time) bool { return !now.Before(c.cert.NotAfter) }

// TrustedIn says whether the base (or kept machine) in dir records this CA as trusted: its tools.json holds
// the ca item InspectionCAItem with this CA's fingerprint, read back trusted. On macOS 26 that is after the
// person trusted it in edit-base. It is the guest's answer at its last tools update or edit, not a check now.
func (c *InspectionCA) TrustedIn(dir string) bool {
	rec, err := ReadToolsRecord(dir)
	if err != nil {
		return false
	}
	for _, it := range rec.Items {
		if it.Kind == ItemKindCA && it.Name == InspectionCAItem {
			return it.Fingerprint == c.Fingerprint() && it.State == ItemTrusted
		}
	}
	return false
}

// InspectedNames is the name constraint a re-issued run CA carries (CrossSign). permitted is each host the run's
// proxy terminates (proxy.Config.TerminatedHosts, without its port). excluded is the subdomains of each such host
// that no other terminated host sits under, since a DNS name constraint names a host and every name below it. A
// literal IP is left out, since CrossSign excludes every address. So is a name x509 cannot encode as a DNS
// constraint (dnsConstraintName): that host is not covered, and the run still starts.
func InspectedNames(terminated []string) (permitted, excluded []string) {
	for _, t := range terminated {
		h := t
		if host, _, err := net.SplitHostPort(t); err == nil {
			h = host
		}
		h = strings.ToLower(strings.TrimSuffix(h, "."))
		if !dnsConstraintName(h) || net.ParseIP(h) != nil || slices.Contains(permitted, h) {
			continue
		}
		permitted = append(permitted, h)
	}
	for _, h := range permitted {
		if !slices.ContainsFunc(permitted, func(p string) bool { return strings.HasSuffix(p, "."+h) }) {
			excluded = append(excluded, "."+h)
		}
	}
	return permitted, excluded
}

// dnsConstraintName says whether h is dot-separated, non-empty labels of a-z, 0-9, '-' and '_': a name
// x509.CreateCertificate encodes as a DNS name constraint. It refuses an internationalised name as written, an
// empty label, a wildcard and a port.
func dnsConstraintName(h string) bool {
	for _, l := range strings.Split(h, ".") {
		if l == "" || strings.Trim(l, "abcdefghijklmnopqrstuvwxyz0123456789-_") != "" {
			return false
		}
	}
	return true
}

// CrossSign re-issues runCAPEM, a run proxy's CA, under this Mac's CA, for that run's proxy to serve after each
// leaf it mints. The copy keeps the run CA's subject, key and key id, so a leaf chains to the run CA in the
// guest's CA bundle and, through the copy, to the root the base trusts. The copy is limited three ways:
//   - by name: the hosts in terminated (InspectedNames) and no IP address, so a leaked run key cannot pass for a
//     host the guest reaches through a tunnel and checks itself;
//   - in time: no longer than validFor from now (crossSignMax when validFor is 0 or longer), nor past either CA;
//   - in depth: it signs only leaves.
//
// It refuses a run CA that may sign other CAs, a run whose proxy terminates no host (a name constraint naming
// nothing limits nothing), an expired run CA, and an expired Mac CA.
func (c *InspectionCA) CrossSign(runCAPEM string, terminated []string, validFor time.Duration) (string, error) {
	blk, _ := pem.Decode([]byte(runCAPEM))
	if blk == nil || blk.Type != "CERTIFICATE" {
		return "", errors.New("macvm: the run's proxy CA is not a PEM certificate")
	}
	run, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return "", fmt.Errorf("macvm: the run's proxy CA: %w", err)
	}
	if !run.IsCA || run.MaxPathLen != 0 || !run.MaxPathLenZero || len(run.SubjectKeyId) == 0 {
		return "", errors.New("macvm: the run's proxy CA is not a CA that signs only leaves")
	}
	permitted, excluded := InspectedNames(terminated)
	if len(permitted) == 0 {
		return "", errors.New("macvm: the run's proxy terminates no host a name constraint can name, so its CA is not re-issued")
	}
	now := time.Now()
	if c.Expired(now) {
		return "", fmt.Errorf("macvm: this Mac's inspection CA expired at %s; to rotate it, remove %s, restart the runner and run macos-vm prepare --new-image", c.cert.NotAfter.UTC().Format(time.RFC3339), c.dir)
	}
	if validFor <= 0 || validFor > crossSignMax {
		validFor = crossSignMax
	}
	notAfter := now.Add(validFor)
	if run.NotAfter.Before(notAfter) {
		notAfter = run.NotAfter
	}
	if c.cert.NotAfter.Before(notAfter) {
		notAfter = c.cert.NotAfter
	}
	if !notAfter.After(now) {
		return "", errors.New("macvm: the run's proxy CA has expired")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial,
		RawSubject:                  run.RawSubject,
		SubjectKeyId:                run.SubjectKeyId,
		NotBefore:                   run.NotBefore,
		NotAfter:                    notAfter,
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    run.KeyUsage,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         permitted,
		ExcludedDNSDomains:          excluded,
		ExcludedIPRanges: []*net.IPNet{
			{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)},
			{IP: net.IPv6zero, Mask: net.CIDRMask(0, 128)},
		},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, run.PublicKey, c.key)
	if err != nil {
		return "", fmt.Errorf("macvm: re-issue the run's proxy CA: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), nil
}
