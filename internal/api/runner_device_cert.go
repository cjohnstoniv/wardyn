// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/singleflight"

	"github.com/cjohnstoniv/wardyn/internal/devicecert"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The bounds on the MDM device CA settings.
const (
	mdmCAPEMMaxBytes = 64 << 10
	mdmCAMaxCerts    = 16
	mdmCRLURLMaxLen  = 2048
)

// parseMDMBundle reads the configured MDM CA bundle: one to mdmCAMaxCerts CERTIFICATE blocks and nothing
// else, each a CA certificate with valid basic constraints.
func parseMDMBundle(bundle string) ([]*x509.Certificate, string) {
	if len(bundle) > mdmCAPEMMaxBytes {
		return nil, "mdm_device_ca_pem is larger than 64 KiB"
	}
	var certs []*x509.Certificate
	rest := []byte(bundle)
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			return nil, "mdm_device_ca_pem may hold CERTIFICATE blocks only"
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, "mdm_device_ca_pem holds a certificate that cannot be read"
		}
		if !c.BasicConstraintsValid || !c.IsCA {
			return nil, "mdm_device_ca_pem may hold CA certificates only"
		}
		certs = append(certs, c)
	}
	if strings.TrimSpace(string(rest)) != "" {
		return nil, "mdm_device_ca_pem holds data that is not a PEM certificate"
	}
	if len(certs) == 0 {
		return nil, "mdm_device_ca_pem holds no certificate"
	}
	if len(certs) > mdmCAMaxCerts {
		return nil, "mdm_device_ca_pem holds more than 16 certificates"
	}
	return certs, ""
}

// validateMDMDeviceSettings is "" when the MDM device CA and CRL URL in st are usable: both empty, or
// both set to a bundle of CA certificates and an absolute http(s) URL.
func validateMDMDeviceSettings(st types.RunnerSettings) string {
	if st.MDMDeviceCAPEM == "" && st.MDMDeviceCRLURL == "" {
		return ""
	}
	if st.MDMDeviceCAPEM == "" || st.MDMDeviceCRLURL == "" {
		return "set mdm_device_ca_pem and mdm_device_crl_url together, or clear both"
	}
	if _, msg := parseMDMBundle(st.MDMDeviceCAPEM); msg != "" {
		return msg
	}
	if len(st.MDMDeviceCRLURL) > mdmCRLURLMaxLen {
		return "mdm_device_crl_url is longer than 2048 bytes"
	}
	u, err := url.Parse(st.MDMDeviceCRLURL)
	if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return "mdm_device_crl_url must be an absolute http or https URL with a host and no user info"
	}
	return ""
}

// mdmBundleFingerprints is the sorted SHA-256 hex of each certificate in a bundle, what the audit row
// records instead of the PEM. An unreadable bundle gives none.
func mdmBundleFingerprints(bundle string) []string {
	out := []string{}
	rest := []byte(bundle)
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		sum := sha256.Sum256(blk.Bytes)
		out = append(out, hex.EncodeToString(sum[:]))
	}
	sort.Strings(out)
	return out
}

// mdmDeviceVerdict is the device-certificate check's answer for one runner. Its zero value refuses, so a
// caller that never asked cannot pass the gate.
type mdmDeviceVerdict struct {
	notRequired bool   // no CA configured: the v0.10.2 gate
	ok          bool   // a presented certificate chains to the CA, is current and is not revoked
	evidence    string // what the pass rested on, for the explain card
	refusal     string // the sentence the runner's refusal names, when !ok and !notRequired
}

func mdmRefused(sentence string) mdmDeviceVerdict { return mdmDeviceVerdict{refusal: sentence} }

// gateOutcome is the managed-Mac gate's answer once the trust and posture checks have passed: the evidence
// the pass rested on, or the refusal. The zero verdict refuses with a fixed sentence.
func (v mdmDeviceVerdict) gateOutcome(rn types.Runner) ([]string, *placement.Refusal) {
	evidence := []string{"managed_device (set by an administrator)", "MDM posture file"}
	switch {
	case v.notRequired:
		return append(evidence, "no MDM device-certificate check configured"), nil
	case !v.ok:
		detail := v.refusal
		if detail == "" {
			detail = macRunnerName(rn) + " was not checked for an MDM device certificate."
		}
		return nil, &placement.Refusal{Reason: placement.ReasonRunnerPostureUnmet, Field: "machine_kind", Detail: detail}
	}
	return append(evidence, v.evidence), nil
}

// mdmNow is the control plane's clock, time.Now when none is set.
func (s *Server) mdmNow() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// mdmDeviceVerdictFor judges rn's presented chains against the runner settings and the CRL as they are now.
// With no CA configured it answers notRequired. Any other failure to reach a clear pass refuses.
func (s *Server) mdmDeviceVerdictFor(ctx context.Context, rn types.Runner) mdmDeviceVerdict {
	name := macRunnerName(rn)
	cfg, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return mdmRefused(name + " could not be checked for an MDM device certificate; try again.")
	}
	st := runnerSettingsOf(cfg)
	if st.MDMDeviceCAPEM == "" {
		return mdmDeviceVerdict{notRequired: true}
	}
	roots, msg := parseMDMBundle(st.MDMDeviceCAPEM)
	if msg != "" {
		return mdmRefused(name + " could not be checked: the configured MDM CA could not be read.")
	}
	if rn.DeviceCert == nil || len(rn.DeviceCert.Chains) == 0 {
		return mdmRefused(name + " has not presented an MDM device certificate.")
	}
	pool := x509.NewCertPool()
	for _, c := range roots {
		pool.AddCert(c)
	}
	j := &mdmJudge{s: s, ctx: ctx, st: st, roots: roots, name: name, now: s.mdmNow()}
	last := name + " has not presented an MDM device certificate."
	for _, chain := range rn.DeviceCert.Chains {
		leaf, inter, presented := parseMDMChain(chain)
		if leaf == nil || leaf.IsCA {
			continue
		}
		paths, err := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, CurrentTime: j.now,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		if err != nil {
			last = name + mdmChainRefusal(leaf, presented, roots, j.now)
			continue
		}
		for _, vc := range paths {
			sentence := j.revocationRefusal(vc)
			if sentence == "" {
				return mdmDeviceVerdict{ok: true, evidence: j.evidence(leaf, rn.DeviceCertReportedAt)}
			}
			last = sentence
		}
	}
	return mdmRefused(last)
}

// parseMDMChain parses a presented chain, leaf first. A chain with a certificate that does not parse is
// no chain.
func parseMDMChain(chain [][]byte) (leaf *x509.Certificate, inter *x509.CertPool, presented []*x509.Certificate) {
	inter = x509.NewCertPool()
	for i, der := range chain {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, nil, nil
		}
		if i == 0 {
			leaf = c
		} else {
			inter.AddCert(c)
			presented = append(presented, c)
		}
	}
	return leaf, inter, presented
}

// mdmChainRefusal is the rest of the sentence for a chain that did not verify. Go reports a CA outside its window
// only as an unknown authority, so the windows are read here, at the verify time: the first certificate outside its
// window is named, the device's own or a CA's, with its date. Otherwise the chain is not from the configured CA.
func mdmChainRefusal(leaf *x509.Certificate, presented, roots []*x509.Certificate, now time.Time) string {
	bad, notYet := devicecert.OutsideWindow(now, leaf, presented, roots)
	if bad == nil {
		return "'s MDM device certificate is not issued by the configured MDM CA."
	}
	who := "'s MDM device certificate"
	if bad != leaf {
		who = "'s MDM device certificate chain holds a CA certificate that"
	}
	if notYet {
		return who + " is not valid until " + bad.NotBefore.UTC().Format("2006-01-02") + "."
	}
	return who + " expired on " + bad.NotAfter.UTC().Format("2006-01-02") + "."
}

// mdmJudge is one verdict's state: the CRL is fetched at most once for it.
type mdmJudge struct {
	s       *Server
	ctx     context.Context
	st      types.RunnerSettings
	roots   []*x509.Certificate
	name    string
	now     time.Time
	crl     *mdmCRL
	crlErr  error
	fetched bool
}

func (j *mdmJudge) crlOnce() (*mdmCRL, error) {
	if !j.fetched {
		j.crl, j.crlErr = j.s.mdmCRLs().current(j.ctx, j.st, j.roots)
		j.fetched = true
	}
	return j.crl, j.crlErr
}

// revocationRefusal is "" when the verified path vc (leaf first, then its issuer) is not revoked by a CRL
// that the leaf's issuer signed, else the refusal sentence.
func (j *mdmJudge) revocationRefusal(vc []*x509.Certificate) string {
	crl, err := j.crlOnce()
	if err != nil {
		cause := "unavailable"
		var ce *mdmCRLError
		if errors.As(err, &ce) {
			cause = ce.cause
		}
		return j.name + ": revocation of its MDM device certificate could not be checked: " + cause + "."
	}
	if len(vc) < 2 || !crl.signers[sha256.Sum256(vc[1].Raw)] {
		return j.name + ": revocation could not be checked for this certificate's issuer."
	}
	for _, e := range crl.list.RevokedCertificateEntries {
		if e.SerialNumber != nil && e.SerialNumber.Cmp(vc[0].SerialNumber) == 0 {
			return j.name + "'s MDM device certificate was revoked on " + e.RevocationTime.UTC().Format("2006-01-02") + "."
		}
	}
	return ""
}

// evidence is the sentence the explain card shows for a pass.
func (j *mdmJudge) evidence(leaf *x509.Certificate, reported *time.Time) string {
	out := "MDM device certificate (expires " + leaf.NotAfter.UTC().Format("2006-01-02")
	if reported != nil {
		out += ", presented " + reported.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	return out + "), CRL checked " + j.crl.lastOK.UTC().Format("2006-01-02 15:04") + " UTC"
}

// The CRL cache's bounds.
const (
	mdmCRLRefetch    = time.Hour
	mdmCRLMaxStale   = 24 * time.Hour
	mdmCRLMaxBytes   = 8 << 20
	mdmCRLMaxRedirs  = 3
	mdmCRLMinBackoff = time.Minute
	mdmCRLMaxBackoff = 10 * time.Minute
	mdmCRLFetchLimit = 10 * time.Second
	mdmCRLWarnEvery  = 10 * time.Minute
)

// mdmCRL is one verified CRL: the list, the bundle certificates it verified under (SHA-256 of Raw), and the
// last fetch that returned it, or one no older.
type mdmCRL struct {
	list    *x509.RevocationList
	signers map[[32]byte]bool
	lastOK  time.Time
}

// mdmCRLError is why no CRL can be used: a cause class only, never a body or a URL.
type mdmCRLError struct{ cause string }

func (e *mdmCRLError) Error() string { return "MDM CRL: " + e.cause }

// mdmCRLEntry is the state for one (CRL URL, CA bundle).
type mdmCRLEntry struct {
	crl      *mdmCRL
	failures int
	nextTry  time.Time
	lastErr  *mdmCRLError
	lastWarn time.Time
}

// mdmCRLCache fetches the configured CRL lazily, once per key at a time, and never takes an older CRL
// over a newer one. The floor is per process: a restart accepts as its first any CA-signed CRL that has
// not passed its next-update time.
type mdmCRLCache struct {
	mu      sync.Mutex
	entries map[string]*mdmCRLEntry
	flight  singleflight.Group
	now     func() time.Time
	timeout time.Duration
}

// mdmCRLs is the server's cache, made on first use.
func (s *Server) mdmCRLs() *mdmCRLCache {
	if c := s.mdmCRL.Load(); c != nil {
		return c
	}
	s.mdmCRL.CompareAndSwap(nil, &mdmCRLCache{entries: map[string]*mdmCRLEntry{}, now: s.mdmNow, timeout: mdmCRLFetchLimit})
	return s.mdmCRL.Load()
}

func mdmCRLKey(st types.RunnerSettings) string {
	sum := sha256.Sum256([]byte(st.MDMDeviceCAPEM))
	return st.MDMDeviceCRLURL + "\x00" + hex.EncodeToString(sum[:])
}

// entry returns the key's entry. The caller holds c.mu.
func (c *mdmCRLCache) entry(key string) *mdmCRLEntry {
	e := c.entries[key]
	if e == nil {
		e = &mdmCRLEntry{}
		c.entries[key] = e
	}
	return e
}

// fresh is whether the cached CRL needs no fetch now.
func (e *mdmCRLEntry) fresh(now time.Time) bool {
	return e.crl != nil && now.Sub(e.crl.lastOK) < mdmCRLRefetch && now.Before(e.crl.list.NextUpdate)
}

// serve is the cached CRL while it is within both its next-update time and the stale bound, else the
// last error. The caller holds the cache's mu.
func (e *mdmCRLEntry) serve(now time.Time) (*mdmCRL, error) {
	if c := e.crl; c != nil && now.Before(c.list.NextUpdate) && now.Before(c.lastOK.Add(mdmCRLMaxStale)) {
		return c, nil
	}
	if e.lastErr != nil {
		return nil, e.lastErr
	}
	return nil, &mdmCRLError{cause: "expired"}
}

// current is the CRL the verdict may rely on for st, fetching it when the cached one is old and the
// back-off allows. A caller waits for the shared fetch no longer than its own context and the fetch's
// timeout; failing that, or finding no usable CRL, it gets an error and the gate refuses.
func (c *mdmCRLCache) current(ctx context.Context, st types.RunnerSettings, roots []*x509.Certificate) (*mdmCRL, error) {
	key := mdmCRLKey(st)
	c.mu.Lock()
	e := c.entry(key)
	now := c.now()
	if e.fresh(now) || now.Before(e.nextTry) {
		defer c.mu.Unlock()
		if e.fresh(now) {
			return e.crl, nil
		}
		return e.serve(now)
	}
	c.mu.Unlock()
	done := c.flight.DoChan(key, func() (any, error) {
		c.fetch(context.WithoutCancel(ctx), key, st, roots)
		return nil, nil
	})
	select {
	case <-done:
	case <-ctx.Done():
		return nil, &mdmCRLError{cause: "timeout"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return e.serve(c.now())
}

// fetch downloads and applies the key's CRL, unless a fetch that finished since the caller looked made
// it fresh or started a back-off.
func (c *mdmCRLCache) fetch(ctx context.Context, key string, st types.RunnerSettings, roots []*x509.Certificate) {
	c.mu.Lock()
	e := c.entry(key)
	if now := c.now(); e.fresh(now) || now.Before(e.nextTry) {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	crl, cause := c.download(ctx, st, roots)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if cause == "" {
		cause = e.apply(crl, now)
	}
	if cause == "" {
		e.failures, e.nextTry, e.lastErr = 0, time.Time{}, nil
		return
	}
	e.failures++
	e.nextTry = now.Add(min(mdmCRLMinBackoff<<min(e.failures-1, 8), mdmCRLMaxBackoff))
	e.lastErr = &mdmCRLError{cause: cause}
	if now.Sub(e.lastWarn) >= mdmCRLWarnEvery {
		e.lastWarn = now
		host := ""
		if u, err := url.Parse(st.MDMDeviceCRLURL); err == nil {
			host = u.Host
		}
		slog.Warn("api: the MDM CRL could not be used", slog.String("host", host), slog.String("cause", cause))
	}
}

// apply takes a fetched CRL into the entry: a newer one replaces the cached one, an equal one only moves
// lastOK, an older one is a failed fetch ("older"). The caller holds the cache's mu.
func (e *mdmCRLEntry) apply(crl *mdmCRL, now time.Time) string {
	cur := e.crl
	switch {
	case cur == nil:
		e.crl = crl
	case mdmCRLNewer(crl.list, cur.list):
		e.crl = crl
	case mdmCRLNewer(cur.list, crl.list):
		return "older"
	default:
		kept := *cur
		kept.lastOK = now
		e.crl = &kept
	}
	e.crl.lastOK = now
	return ""
}

// mdmCRLNewer is whether a is a later CRL than b: a greater Number when both carry one, else a later
// ThisUpdate.
func mdmCRLNewer(a, b *x509.RevocationList) bool {
	if a.Number != nil && b.Number != nil {
		return a.Number.Cmp(b.Number) > 0
	}
	return a.ThisUpdate.After(b.ThisUpdate)
}

var errMDMRedirect = errors.New("redirect refused")

// mdmCRLRedirect allows at most three redirects, each to an http or https URL.
func mdmCRLRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > mdmCRLMaxRedirs || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return errMDMRedirect
	}
	return nil
}

// mdmFetchCause names a transport failure by class.
func mdmFetchCause(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, errMDMRedirect):
		return "redirect"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	}
	return "network"
}

// download fetches and checks the CRL: status 200, at most 8 MiB, parsed, signed by a bundle certificate
// and complete. The cause is a class only.
func (c *mdmCRLCache) download(ctx context.Context, st types.RunnerSettings, roots []*x509.Certificate) (*mdmCRL, string) {
	client := &http.Client{Timeout: c.timeout, CheckRedirect: mdmCRLRedirect}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, st.MDMDeviceCRLURL, nil)
	if err != nil {
		return nil, "network"
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, mdmFetchCause(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "status"
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, mdmCRLMaxBytes+1))
	if err != nil {
		return nil, mdmFetchCause(err)
	}
	if len(body) > mdmCRLMaxBytes {
		return nil, "size"
	}
	return parseMDMCRL(body, roots, c.now())
}

var (
	oidDeltaCRLIndicator = asn1.ObjectIdentifier{2, 5, 29, 27}
	oidIssuingDistPoint  = asn1.ObjectIdentifier{2, 5, 29, 28}
	oidAuthorityKeyID    = asn1.ObjectIdentifier{2, 5, 29, 35}
	oidCRLNumber         = asn1.ObjectIdentifier{2, 5, 29, 20}
)

// mdmCRLComplete is whether the CRL lists every revocation of its issuer: no delta, no partition, and no
// critical extension this code does not process. Go's parser leaves those to the caller.
func mdmCRLComplete(rl *x509.RevocationList) bool {
	for _, ex := range rl.Extensions {
		if ex.Id.Equal(oidDeltaCRLIndicator) || ex.Id.Equal(oidIssuingDistPoint) {
			return false
		}
		if ex.Critical && !ex.Id.Equal(oidAuthorityKeyID) && !ex.Id.Equal(oidCRLNumber) {
			return false
		}
	}
	for _, ent := range rl.RevokedCertificateEntries {
		for _, ex := range ent.Extensions {
			if ex.Critical {
				return false
			}
		}
	}
	return true
}

// parseMDMCRL reads a PEM or DER CRL and keeps it only when a bundle certificate signed it, it is complete
// and its next-update time has not passed.
func parseMDMCRL(body []byte, roots []*x509.Certificate, now time.Time) (*mdmCRL, string) {
	der := body
	if blk, _ := pem.Decode(body); blk != nil {
		if blk.Type != "X509 CRL" {
			return nil, "parse"
		}
		der = blk.Bytes
	}
	rl, err := x509.ParseRevocationList(der)
	if err != nil {
		return nil, "parse"
	}
	signers := map[[32]byte]bool{}
	for _, c := range roots {
		if bytes.Equal(c.RawSubject, rl.RawIssuer) && rl.CheckSignatureFrom(c) == nil {
			signers[sha256.Sum256(c.Raw)] = true
		}
	}
	switch {
	case len(signers) == 0:
		return nil, "signature"
	case !mdmCRLComplete(rl):
		return nil, "unsupported"
	case rl.NextUpdate.IsZero() || !now.Before(rl.NextUpdate):
		return nil, "expired"
	}
	return &mdmCRL{list: rl, signers: signers, lastOK: now}, ""
}

// maxDeviceCertEventBytes bounds one device_cert event's data.
const maxDeviceCertEventBytes = devicecert.MaxEventBytes

// runnerDeviceCertStore is the PG capability behind the device_cert event. The api reaches it through this
// interface, not by widening store.RunnerStore.
type runnerDeviceCertStore interface {
	GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error)
	RecordRunnerDeviceCertFrom(ctx context.Context, id uuid.UUID, session string, chains [][][]byte, now time.Time) (bool, error)
}

var _ runnerDeviceCertStore = store.PG{}

// deviceProofLeaf is the proof's leaf certificate when the proof binds to this runner and organisation: the
// signature is by the leaf's key over the message that names the runner's key fingerprint and the
// organisation, made within the allowed skew. Any other key type, or a signature that does not verify, is no
// proof.
func deviceProofLeaf(p runnerwire.DeviceCertProof, fingerprint, org string, now time.Time) *x509.Certificate {
	if d := now.Sub(p.SignedAt); d > devicecert.MaxSkew || -d > devicecert.MaxSkew {
		return nil
	}
	leaf, err := x509.ParseCertificate(p.Chain[0])
	if err != nil {
		return nil
	}
	msg, err := devicecert.Message(fingerprint, org, p.SignedAt)
	if err != nil {
		return nil
	}
	digest := sha256.Sum256(msg)
	switch pub := leaf.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if (pub.Curve == elliptic.P256() || pub.Curve == elliptic.P384()) && ecdsa.VerifyASN1(pub, digest[:], p.Signature) {
			return leaf
		}
	case *rsa.PublicKey:
		if pub.N.BitLen() >= 2048 && rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], p.Signature) == nil {
			return leaf
		}
	}
	return nil
}

// decodeDeviceCertReport strictly decodes one device_cert body: no unknown field and nothing after the
// object. decodeRunnerEventData is not used because it refuses a body over maxActionResultBytes, which a
// certificate chain exceeds; the caller has already bounded the body.
func decodeDeviceCertReport(data []byte) (runnerwire.DeviceCertReport, error) {
	var r runnerwire.DeviceCertReport
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, errReportShape
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return r, errReportShape
	}
	return r, nil
}

// deviceCertShapeOK is whether the report is within the bounds the helper keeps to.
func deviceCertShapeOK(r runnerwire.DeviceCertReport) bool {
	if len(r.Proofs) > devicecert.MaxProofs {
		return false
	}
	for _, p := range r.Proofs {
		if len(p.Chain) == 0 || len(p.Chain) > devicecert.MaxChainLen {
			return false
		}
		for _, der := range p.Chain {
			if len(der) > devicecert.MaxDER {
				return false
			}
		}
	}
	return true
}

// DeviceCertFromRunner stores the MDM device certificate chains a claimed runner presented, each whose
// device-key signature binds this runner's key and organisation, and, only when they changed, writes
// runner.device_cert.record. session is the runner session the event came in on: a superseded session
// writes nothing. A proof that does not verify is dropped and counted. Nothing here touches trust_class,
// the posture or any run: whether a chain is trusted, current and unrevoked is judged when a macOS sandbox
// is placed (mdmDeviceVerdictFor).
func (s *Server) DeviceCertFromRunner(ctx context.Context, runnerID uuid.UUID, session string, ev runnerwire.Event) error {
	st, ok := s.cfg.Store.(runnerDeviceCertStore)
	if !ok {
		return errRunnerIngestUnavailable
	}
	if ev.RunID != nil || len(ev.Data) > maxDeviceCertEventBytes {
		return errReportShape
	}
	report, err := decodeDeviceCertReport(ev.Data)
	if err != nil || !deviceCertShapeOK(report) {
		return errReportShape
	}
	rn, err := st.GetRunner(ctx, runnerID)
	if err != nil {
		return err
	}
	org := federation.OrgURLSHA256(s.cfg.RunnerOrgURL)
	now := s.mdmNow()
	chains := [][][]byte{}
	var leaves []map[string]any
	for _, p := range report.Proofs {
		leaf := deviceProofLeaf(p, rn.KeyFingerprint, org, now)
		if leaf == nil {
			continue
		}
		chains = append(chains, p.Chain)
		sum, issuer := sha256.Sum256(leaf.Raw), sha256.Sum256(leaf.RawIssuer)
		leaves = append(leaves, map[string]any{"sha256": hex.EncodeToString(sum[:]), "serial": leaf.SerialNumber.Text(16),
			"not_after": leaf.NotAfter.UTC().Format(time.RFC3339), "issuer_sha256": hex.EncodeToString(issuer[:])})
	}
	changed, err := st.RecordRunnerDeviceCertFrom(ctx, runnerID, session, chains, now)
	if err != nil || !changed {
		return err
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "runner:"+runnerID.String(),
		placement.ActionRunnerDeviceCertRecord, runnerID.String(), "success",
		mustJSON(map[string]any{"leaves": leaves, "dropped": len(report.Proofs) - len(chains)})))
	return nil
}
