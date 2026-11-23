// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// deviceNotRequired is the verdict of an organisation with no MDM CA: the gate as it was before the check.
var deviceNotRequired = mdmDeviceVerdict{notRequired: true}

// mdmTestCA is a throwaway CA generated for one test: no fixture key files.
type mdmTestCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

// newMDMTestCA makes a CA named cn; a second call with the same cn is a different key under the same name.
func newMDMTestCA(t *testing.T, cn string) mdmTestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(30 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return mdmTestCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

// mdmTestLeaf is a non-CA certificate (and its key) the test CA issued.
func mdmTestLeaf(t *testing.T, ca mdmTestCA) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "device"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestValidateMDMDeviceSettings(t *testing.T) {
	ca := newMDMTestCA(t, "MDM CA")
	keyBlock := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}))
	var seventeen strings.Builder
	for i := 0; i < 17; i++ {
		seventeen.WriteString(ca.pem)
	}
	const crl = "https://pki.example.com/ca.crl"
	cases := []struct {
		name string
		st   types.RunnerSettings
		want string // "" valid, else a phrase of the refusal
	}{
		{"both empty", types.RunnerSettings{}, ""},
		{"valid", types.RunnerSettings{MDMDeviceCAPEM: ca.pem, MDMDeviceCRLURL: crl}, ""},
		{"valid http", types.RunnerSettings{MDMDeviceCAPEM: ca.pem, MDMDeviceCRLURL: "http://pki.internal/ca.crl"}, ""},
		{"CA without URL", types.RunnerSettings{MDMDeviceCAPEM: ca.pem}, "together"},
		{"URL without CA", types.RunnerSettings{MDMDeviceCRLURL: crl}, "together"},
		{"leaf is not a CA", types.RunnerSettings{MDMDeviceCAPEM: mdmTestLeaf(t, ca), MDMDeviceCRLURL: crl}, "CA certificates only"},
		{"key block", types.RunnerSettings{MDMDeviceCAPEM: ca.pem + keyBlock, MDMDeviceCRLURL: crl}, "CERTIFICATE blocks only"},
		{"trailing bytes", types.RunnerSettings{MDMDeviceCAPEM: ca.pem + "junk", MDMDeviceCRLURL: crl}, "not a PEM certificate"},
		{"no certificate", types.RunnerSettings{MDMDeviceCAPEM: "not pem", MDMDeviceCRLURL: crl}, "not a PEM certificate"},
		{"17 certificates", types.RunnerSettings{MDMDeviceCAPEM: seventeen.String(), MDMDeviceCRLURL: crl}, "more than 16"},
		{"over 64 KiB", types.RunnerSettings{MDMDeviceCAPEM: strings.Repeat(" ", 65<<10) + ca.pem, MDMDeviceCRLURL: crl}, "64 KiB"},
		{"file URL", types.RunnerSettings{MDMDeviceCAPEM: ca.pem, MDMDeviceCRLURL: "file:///etc/ca.crl"}, "http or https"},
		{"relative URL", types.RunnerSettings{MDMDeviceCAPEM: ca.pem, MDMDeviceCRLURL: "/ca.crl"}, "http or https"},
		{"user info", types.RunnerSettings{MDMDeviceCAPEM: ca.pem, MDMDeviceCRLURL: "https://u:p@pki.example.com/ca.crl"}, "no user info"},
		{"URL over 2048 bytes", types.RunnerSettings{MDMDeviceCAPEM: ca.pem, MDMDeviceCRLURL: "https://pki.example.com/" + strings.Repeat("a", 2048)}, "2048"},
	}
	for _, c := range cases {
		got := validateMDMDeviceSettings(c.st)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want it to contain %q", c.name, got, c.want)
		}
	}
}

// The MDM fields live in the stored runner settings and survive an enabled toggle; the audit row carries the
// CA's certificate fingerprints, never the PEM.
func TestRunnerSettings_MDMFieldsSurviveToggle(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	srv.cfg.ControlPlaneURL = "https://wardynd.internal:8443"
	admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
	put := func(body string) *httptest.ResponseRecorder {
		return doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", admin, body)
	}
	ca := newMDMTestCA(t, "MDM CA")
	body, _ := json.Marshal(map[string]string{"mdm_device_ca_pem": ca.pem, "mdm_device_crl_url": "https://pki.example.com/ca.crl"})
	if w := put(string(body)); w.Code != http.StatusOK {
		t.Fatalf("set the MDM fields: %d %s", w.Code, w.Body.String())
	}
	for _, enabled := range []string{"false", "true"} {
		if w := put(`{"enabled":` + enabled + `}`); w.Code != http.StatusOK {
			t.Fatalf("toggle enabled=%s: %d %s", enabled, w.Code, w.Body.String())
		}
		if rs := f.siteCfg.Runners; rs == nil || rs.MDMDeviceCAPEM != ca.pem || rs.MDMDeviceCRLURL != "https://pki.example.com/ca.crl" {
			t.Fatalf("after enabled=%s the stored settings are %+v, want the CA and URL kept", enabled, rs)
		}
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/runners/settings", admin, ""); !strings.Contains(w.Body.String(), `"mdm_device_crl_url":"https://pki.example.com/ca.crl"`) {
		t.Fatalf("settings read: %s", w.Body.String())
	}
	ev := lastAuditEvent(t, h.audit.snapshot(), "runners.mdm_device.set")
	if strings.Contains(string(ev.Data), "BEGIN CERTIFICATE") || strings.Contains(string(ev.Data), "MIIB") {
		t.Fatalf("the audit row carries the PEM: %s", ev.Data)
	}
	if w := put(`{"mdm_device_ca_pem":""}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "runner_mdm_device_invalid") {
		t.Fatalf("clearing only the CA: %d %s, want 400 runner_mdm_device_invalid", w.Code, w.Body.String())
	}
	if w := put(`{"mdm_device_ca_pem":"","mdm_device_crl_url":""}`); w.Code != http.StatusOK || f.siteCfg.Runners.MDMDeviceCAPEM != "" || f.siteCfg.Runners.MDMDeviceCRLURL != "" {
		t.Fatalf("clearing both: %d %+v", w.Code, f.siteCfg.Runners)
	}
}

func carryWith(t *testing.T, body, existing *types.RunnerSettings) (*httptest.ResponseRecorder, bool, types.SiteConfig) {
	t.Helper()
	w := httptest.NewRecorder()
	cfg := types.SiteConfig{Runners: body}
	ok := carryRunners(w, &cfg, types.SiteConfig{Runners: existing})
	return w, ok, cfg
}

func TestCarryRunners_RefusesMDMFields(t *testing.T) {
	stored := &types.RunnerSettings{Enabled: true, MDMDeviceCAPEM: "stored-pem", MDMDeviceCRLURL: "https://pki.example.com/a.crl"}
	for name, body := range map[string]*types.RunnerSettings{
		"another CA":  {Enabled: true, MDMDeviceCAPEM: "other-pem", MDMDeviceCRLURL: stored.MDMDeviceCRLURL},
		"another URL": {Enabled: true, MDMDeviceCAPEM: stored.MDMDeviceCAPEM, MDMDeviceCRLURL: "https://pki.example.com/b.crl"},
	} {
		w, ok, _ := carryWith(t, body, stored)
		if ok || w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "PUT /runners/settings") {
			t.Errorf("%s: ok=%v %d %s, want a 400 naming PUT /runners/settings", name, ok, w.Code, w.Body.String())
		}
	}
	if _, ok, cfg := carryWith(t, &types.RunnerSettings{Enabled: true, MDMDeviceCAPEM: "x", MDMDeviceCRLURL: "https://pki.example.com/a.crl"}, &types.RunnerSettings{Enabled: true}); ok || cfg.Runners == nil {
		t.Errorf("setting a CA where none is stored: ok=%v, want refused", ok)
	}
}

func TestCarryRunners_EmptyMDMFieldsCarry(t *testing.T) {
	stored := &types.RunnerSettings{Enabled: true, MDMDeviceCAPEM: "stored-pem", MDMDeviceCRLURL: "https://pki.example.com/a.crl"}
	for name, body := range map[string]*types.RunnerSettings{
		"fields absent": {Enabled: true},
		"an echo":       {Enabled: true, MDMDeviceCAPEM: stored.MDMDeviceCAPEM, MDMDeviceCRLURL: stored.MDMDeviceCRLURL},
		"no runners":    nil,
	} {
		w, ok, cfg := carryWith(t, body, stored)
		if !ok || w.Code != http.StatusOK || cfg.Runners == nil || cfg.Runners.MDMDeviceCAPEM != "stored-pem" || cfg.Runners.MDMDeviceCRLURL != stored.MDMDeviceCRLURL {
			t.Errorf("%s: ok=%v %+v, want the stored CA and URL carried", name, ok, cfg.Runners)
		}
	}
}

// lockWatcher is a locker whose holders are counted, so a test can see that the site-config work runs
// under the lock and one writer at a time.
type lockWatcher struct {
	db.Locker
	mu       sync.Mutex
	holding  int
	maxHeld  int
	acquired int
}

func (l *lockWatcher) Lock(ctx context.Context, k db.LockKey, wait time.Duration) (context.Context, func(), error) {
	lctx, unlock, err := l.Locker.Lock(ctx, k, wait)
	if err != nil {
		return lctx, unlock, err
	}
	l.mu.Lock()
	l.holding++
	l.acquired++
	l.maxHeld = max(l.maxHeld, l.holding)
	l.mu.Unlock()
	return lctx, func() {
		l.mu.Lock()
		l.holding--
		l.mu.Unlock()
		unlock()
	}, nil
}

func (l *lockWatcher) held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holding
}

// putRecorder sits over the store and records each PutSiteConfig, and whether the locker was held for it.
type putRecorder struct {
	*runnerRegistrationFake
	locks *lockWatcher
	mu    sync.Mutex
	puts  []types.RunnerSettings
	bare  int // puts made while no lock was held
}

func (p *putRecorder) PutSiteConfig(ctx context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	p.mu.Lock()
	if p.locks.held() == 0 {
		p.bare++
	}
	if cfg.Runners != nil {
		p.puts = append(p.puts, *cfg.Runners)
	}
	p.mu.Unlock()
	return p.runnerRegistrationFake.PutSiteConfig(ctx, cfg)
}

// "Both or neither" holds under concurrent writes: validation runs on the settings as they will be stored,
// under the site-config lock, so a PUT clearing both racing a PUT setting only the CA never stores a CA
// without a URL, and a refused PUT writes nothing.
func TestRunnerSettings_MDMValidatedUnderLock(t *testing.T) {
	ca := newMDMTestCA(t, "MDM CA")
	other := newMDMTestCA(t, "Other CA")
	const crl = "https://pki.example.com/ca.crl"
	for i := 0; i < 20; i++ {
		srv, f, _ := newRunnerRegistrationServer(t)
		f.siteCfg.Runners = &types.RunnerSettings{Enabled: true, MDMDeviceCAPEM: ca.pem, MDMDeviceCRLURL: crl}
		w := &lockWatcher{Locker: db.NewLocalLocker()}
		srv.locks.override = w
		rec := &putRecorder{runnerRegistrationFake: f, locks: w}
		srv.cfg.Store = rec
		admin := memberModeSSOSession(t, "carol", "carol@example.com", oidc.RoleAdmin, false)
		clearBody, _ := json.Marshal(map[string]string{"mdm_device_ca_pem": "", "mdm_device_crl_url": ""})
		caBody, _ := json.Marshal(map[string]string{"mdm_device_ca_pem": other.pem})
		codes := make([]*httptest.ResponseRecorder, 2)
		var wg sync.WaitGroup
		for j, body := range []string{string(clearBody), string(caBody)} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				codes[j] = doSSO(t, srv, http.MethodPut, "/api/v1/runners/settings", admin, body)
			}()
		}
		wg.Wait()
		ok := 0
		for _, c := range codes {
			switch c.Code {
			case http.StatusOK:
				ok++
			case http.StatusBadRequest:
				if !strings.Contains(c.Body.String(), "runner_mdm_device_invalid") {
					t.Fatalf("iteration %d: a refusal without the reason: %s", i, c.Body.String())
				}
			default:
				t.Fatalf("iteration %d: %d %s", i, c.Code, c.Body.String())
			}
		}
		if len(rec.puts) != ok || rec.bare != 0 || w.maxHeld != 1 {
			t.Fatalf("iteration %d: %d puts for %d accepted PUTs, %d outside the lock, %d holders at once", i, len(rec.puts), ok, rec.bare, w.maxHeld)
		}
		for _, st := range rec.puts {
			if msg := validateMDMDeviceSettings(st); msg != "" {
				t.Fatalf("iteration %d: stored settings that are not valid (%s)", i, msg)
			}
		}
		if got := f.siteCfg.Runners; validateMDMDeviceSettings(*got) != "" {
			t.Fatalf("iteration %d: final settings %+v are not valid", i, got)
		}
	}
}

// mdmClock is the fake clock the verdicts run on.
type mdmClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *mdmClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *mdmClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// mdmStore answers the site config a verdict reads.
type mdmStore struct {
	store.Store
	mu  sync.Mutex
	cfg types.SiteConfig
	err error
}

func (m *mdmStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg, m.err
}

// mdmEnv is a server whose MDM CA is one test CA and whose CRL URL is a test server that counts requests.
type mdmEnv struct {
	t     *testing.T
	ca    mdmTestCA
	clock *mdmClock
	store *mdmStore
	srv   *Server
	web   *httptest.Server
	hits  atomic.Int32
	mu    sync.Mutex
	serve http.HandlerFunc
}

func newMDMEnv(t *testing.T) *mdmEnv {
	t.Helper()
	e := &mdmEnv{t: t, ca: newMDMTestCA(t, "MDM CA"), clock: &mdmClock{t: time.Now().Truncate(time.Second)}}
	e.web = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		e.mu.Lock()
		h := e.serve
		e.mu.Unlock()
		if h == nil {
			http.Error(w, "no crl", http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	t.Cleanup(e.web.Close)
	e.store = &mdmStore{cfg: types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true, MDMDeviceCAPEM: e.ca.pem, MDMDeviceCRLURL: e.web.URL + "/ca.crl"}}}
	e.srv = &Server{cfg: Config{Store: e.store, Now: e.clock.now}}
	return e
}

func (e *mdmEnv) handle(h http.HandlerFunc) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.serve = h
}

// serveCRL answers every request with der.
func (e *mdmEnv) serveCRL(der []byte) {
	e.handle(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(der) })
}

// crl builds a CRL the test CA signed, naming the serials revoked.
func (e *mdmEnv) crl(number int64, next time.Duration, revoked ...int64) []byte {
	return mdmTestCRL(e.t, e.ca, number, e.clock.now().Add(-time.Minute), e.clock.now().Add(next), revoked, nil, nil)
}

// leaf issues a device certificate and returns its DER.
func (e *mdmEnv) leaf(serial int64, from, to time.Duration) []byte {
	return mdmTestDeviceCert(e.t, e.ca, serial, e.clock.now().Add(from), e.clock.now().Add(to))
}

func (e *mdmEnv) runner(chains ...[]byte) types.Runner {
	rn := types.Runner{ID: uuid.New(), Name: "studio"}
	rep := e.clock.now().Add(-time.Minute)
	rn.DeviceCert = &types.RunnerDeviceCert{}
	for _, c := range chains {
		rn.DeviceCert.Chains = append(rn.DeviceCert.Chains, [][]byte{c})
	}
	rn.DeviceCertReportedAt = &rep
	return rn
}

func (e *mdmEnv) verdict(rn types.Runner) mdmDeviceVerdict {
	return e.srv.mdmDeviceVerdictFor(context.Background(), rn)
}

// mustRefuse fails unless v refuses with a sentence naming the runner and containing want.
func mustRefuse(t *testing.T, label string, v mdmDeviceVerdict, want string) {
	t.Helper()
	if v.ok || v.notRequired || !strings.HasPrefix(v.refusal, "studio") || !strings.Contains(v.refusal, want) {
		t.Fatalf("%s: %+v, want a refusal starting with the runner's name and containing %q", label, v, want)
	}
}

func mustPass(t *testing.T, label string, v mdmDeviceVerdict) {
	t.Helper()
	if !v.ok || v.refusal != "" || !strings.Contains(v.evidence, "MDM device certificate") {
		t.Fatalf("%s: %+v, want a pass with evidence", label, v)
	}
}

// mdmTestDeviceCert is a device certificate the CA issued, with the given serial and validity.
func mdmTestDeviceCert(t *testing.T, ca mdmTestCA, serial int64, from, to time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "device-" + uuid.NewString()},
		NotBefore: from, NotAfter: to}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// mdmTestCRL is a CRL the CA signed. extra are CRL-level extensions, entryExtra extensions on each entry.
func mdmTestCRL(t *testing.T, ca mdmTestCA, number int64, this, next time.Time, revoked []int64, extra, entryExtra []pkix.Extension) []byte {
	t.Helper()
	rl := &x509.RevocationList{Number: big.NewInt(number), ThisUpdate: this, NextUpdate: next, ExtraExtensions: extra}
	for _, n := range revoked {
		rl.RevokedCertificateEntries = append(rl.RevokedCertificateEntries, x509.RevocationListEntry{
			SerialNumber: big.NewInt(n), RevocationTime: this, ExtraExtensions: entryExtra})
	}
	der, err := x509.CreateRevocationList(rand.Reader, rl, ca.cert, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// Revocation that cannot be read fails closed, each for its own cause, and a CRL that is not a full list
// from a CA in the bundle is never relied on.
func TestMDMDeviceVerdict_RevocationUnknownRefuses(t *testing.T) {
	delta := pkix.Extension{Id: oidDeltaCRLIndicator, Value: []byte{0x02, 0x01, 0x01}}
	idp := pkix.Extension{Id: oidIssuingDistPoint, Value: []byte{0x30, 0x00}}
	unknown := pkix.Extension{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Critical: true, Value: []byte{0x05, 0x00}}
	certIssuer := pkix.Extension{Id: asn1.ObjectIdentifier{2, 5, 29, 29}, Critical: true, Value: []byte{0x30, 0x00}}
	crlWith := func(e *mdmEnv, extra, entry []pkix.Extension) []byte {
		now := e.clock.now()
		return mdmTestCRL(t, e.ca, 1, now.Add(-time.Minute), now.Add(48*time.Hour), []int64{99}, extra, entry)
	}
	cases := []struct {
		name  string
		cause string
		setup func(e *mdmEnv)
	}{
		{"unreachable", "network", func(e *mdmEnv) { e.store.cfg.Runners.MDMDeviceCRLURL = "http://127.0.0.1:1/ca.crl" }},
		{"redirect to another scheme", "redirect", func(e *mdmEnv) {
			e.handle(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "ftp://pki.example.com/ca.crl", http.StatusFound)
			})
		}},
		{"redirect loop", "redirect", func(e *mdmEnv) {
			e.handle(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/ca.crl", http.StatusFound) })
		}},
		{"status", "status", func(e *mdmEnv) {}},
		{"past next update", "expired", func(e *mdmEnv) {
			now := e.clock.now()
			e.serveCRL(mdmTestCRL(t, e.ca, 1, now.Add(-2*time.Hour), now.Add(-time.Hour), nil, nil, nil))
		}},
		{"signed by a key outside the bundle", "signature", func(e *mdmEnv) {
			other := newMDMTestCA(t, "Other CA")
			now := e.clock.now()
			e.serveCRL(mdmTestCRL(t, other, 1, now.Add(-time.Minute), now.Add(time.Hour), nil, nil, nil))
		}},
		{"same name, another key", "signature", func(e *mdmEnv) {
			other := newMDMTestCA(t, "MDM CA")
			now := e.clock.now()
			e.serveCRL(mdmTestCRL(t, other, 1, now.Add(-time.Minute), now.Add(time.Hour), nil, nil, nil))
		}},
		{"not a CRL", "parse", func(e *mdmEnv) { e.serveCRL([]byte("not a crl")) }},
		{"critical delta indicator", "unsupported", func(e *mdmEnv) {
			d := delta
			d.Critical = true
			e.serveCRL(crlWith(e, []pkix.Extension{d}, nil))
		}},
		{"non-critical delta indicator", "unsupported", func(e *mdmEnv) { e.serveCRL(crlWith(e, []pkix.Extension{delta}, nil)) }},
		{"issuing distribution point", "unsupported", func(e *mdmEnv) { e.serveCRL(crlWith(e, []pkix.Extension{idp}, nil)) }},
		{"unknown critical extension", "unsupported", func(e *mdmEnv) { e.serveCRL(crlWith(e, []pkix.Extension{unknown}, nil)) }},
		{"critical entry extension", "unsupported", func(e *mdmEnv) { e.serveCRL(crlWith(e, nil, []pkix.Extension{certIssuer})) }},
		{"9 MiB body", "size", func(e *mdmEnv) { e.serveCRL(make([]byte, 9<<20)) }},
	}
	for _, c := range cases {
		e := newMDMEnv(t)
		if c.name == "status" {
			e.handle(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) })
		}
		c.setup(e)
		v := e.verdict(e.runner(e.leaf(5, -time.Hour, 24*time.Hour)))
		mustRefuse(t, c.name, v, "revocation of its MDM device certificate could not be checked: "+c.cause)
	}
	// The control: the same leaf passes against a complete CRL that does not list it.
	e := newMDMEnv(t)
	e.serveCRL(e.crl(1, 48*time.Hour, 99))
	mustPass(t, "a complete CRL", e.verdict(e.runner(e.leaf(5, -time.Hour, 24*time.Hour))))
}

// A CRL from a CA of the same name but another key says nothing about a leaf of the new key.
func TestMDMDeviceVerdict_CRLFromSameNameOtherKeyRefuses(t *testing.T) {
	e := newMDMEnv(t)
	oldCA := newMDMTestCA(t, "MDM CA")
	e.store.cfg.Runners.MDMDeviceCAPEM = oldCA.pem + e.ca.pem
	now := e.clock.now()
	// The old key's CRL lists nothing; the new key's would list the leaf, but the URL serves the old one.
	e.serveCRL(mdmTestCRL(t, oldCA, 1, now.Add(-time.Minute), now.Add(48*time.Hour), nil, nil, nil))
	v := e.verdict(e.runner(e.leaf(5, -time.Hour, 24*time.Hour)))
	mustRefuse(t, "old key's CRL", v, "could not be checked for this certificate's issuer")
	// The new key's own CRL, listing the leaf, refuses it as revoked.
	e2 := newMDMEnv(t)
	e2.store.cfg.Runners.MDMDeviceCAPEM = oldCA.pem + e2.ca.pem
	e2.serveCRL(e2.crl(1, 48*time.Hour, 5))
	mustRefuse(t, "new key's CRL", e2.verdict(e2.runner(e2.leaf(5, -time.Hour, 24*time.Hour))), "was revoked on")
}

func TestMDMDeviceVerdict_RevokedRefuses(t *testing.T) {
	e := newMDMEnv(t)
	e.serveCRL(e.crl(1, 48*time.Hour, 5, 6))
	mustRefuse(t, "revoked", e.verdict(e.runner(e.leaf(5, -time.Hour, 24*time.Hour))), "MDM device certificate was revoked on "+e.clock.now().UTC().Format("2006-01-02"))
	mustPass(t, "not listed", e.verdict(e.runner(e.leaf(7, -time.Hour, 24*time.Hour))))
	v := e.verdict(e.runner(e.leaf(7, -time.Hour, 24*time.Hour)))
	if !strings.Contains(v.evidence, "expires ") || !strings.Contains(v.evidence, "presented ") || !strings.Contains(v.evidence, "CRL checked ") {
		t.Fatalf("evidence %q, want the expiry, the report time and the CRL time", v.evidence)
	}
}

// A revocation published after the cached CRL is seen at the next refetch.
func TestMDMCRLCache_RefetchSeesNewRevocation(t *testing.T) {
	e := newMDMEnv(t)
	rn := e.runner(e.leaf(5, -time.Hour, 24*time.Hour))
	e.serveCRL(e.crl(1, 48*time.Hour))
	mustPass(t, "before", e.verdict(rn))
	e.serveCRL(e.crl(2, 48*time.Hour, 5))
	mustPass(t, "inside the hour the cached CRL answers", e.verdict(rn))
	e.clock.add(mdmCRLRefetch + time.Minute)
	mustRefuse(t, "after the refetch", e.verdict(rn), "was revoked on")
}

// After a failed fetch the cached CRL is served only until its next-update time and for 24 hours after
// the last good fetch.
func TestMDMCRLCache_FetchFailureServesCacheOnlyUntilBound(t *testing.T) {
	fail := func(e *mdmEnv) {
		e.handle(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusServiceUnavailable) })
	}
	t.Run("within both bounds", func(t *testing.T) {
		e := newMDMEnv(t)
		rn := e.runner(e.leaf(5, -time.Hour, 72*time.Hour))
		e.serveCRL(e.crl(1, 30*time.Hour))
		mustPass(t, "fetched", e.verdict(rn))
		fail(e)
		e.clock.add(2 * time.Hour)
		mustPass(t, "served from the cache after a failed fetch", e.verdict(rn))
	})
	t.Run("past next update", func(t *testing.T) {
		e := newMDMEnv(t)
		rn := e.runner(e.leaf(5, -time.Hour, 72*time.Hour))
		e.serveCRL(e.crl(1, 3*time.Hour))
		mustPass(t, "fetched", e.verdict(rn))
		fail(e)
		e.clock.add(2 * time.Hour)
		mustPass(t, "still before next update", e.verdict(rn))
		e.clock.add(time.Hour + time.Minute)
		mustRefuse(t, "past next update", e.verdict(rn), "could not be checked")
	})
	t.Run("past 24 hours after the last good fetch", func(t *testing.T) {
		e := newMDMEnv(t)
		rn := e.runner(e.leaf(5, -time.Hour, 72*time.Hour))
		e.serveCRL(e.crl(1, 30*time.Hour))
		mustPass(t, "fetched", e.verdict(rn))
		fail(e)
		e.clock.add(mdmCRLMaxStale - time.Minute)
		mustPass(t, "just inside 24 hours", e.verdict(rn))
		e.clock.add(2 * time.Minute)
		mustRefuse(t, "past 24 hours, before next update", e.verdict(rn), "could not be checked")
	})
}

// An older CRL never replaces a newer one, so a revocation stays visible after a replay.
func TestMDMCRLCache_OlderCRLNeverReplacesNewer(t *testing.T) {
	e := newMDMEnv(t)
	rn := e.runner(e.leaf(5, -time.Hour, 72*time.Hour))
	e.serveCRL(e.crl(5, 48*time.Hour, 5))
	mustRefuse(t, "revoked at number 5", e.verdict(rn), "was revoked on")
	e.serveCRL(e.crl(3, 48*time.Hour)) // a replayed older list that does not name the leaf
	e.clock.add(mdmCRLRefetch + time.Minute)
	mustRefuse(t, "after the replay", e.verdict(rn), "was revoked on")
	// Equal keeps the list and moves lastOK, so it is served without a refetch for another hour.
	e.serveCRL(e.crl(5, 48*time.Hour, 5))
	e.clock.add(mdmCRLRefetch + time.Minute)
	mustRefuse(t, "an equal CRL", e.verdict(rn), "was revoked on")

	// Without numbers the later ThisUpdate is the newer.
	now := time.Now()
	a := &x509.RevocationList{ThisUpdate: now}
	b := &x509.RevocationList{ThisUpdate: now.Add(-time.Hour)}
	if !mdmCRLNewer(a, b) || mdmCRLNewer(b, a) || mdmCRLNewer(a, a) {
		t.Errorf("ThisUpdate order: a newer than b %v, b newer than a %v, a newer than a %v", mdmCRLNewer(a, b), mdmCRLNewer(b, a), mdmCRLNewer(a, a))
	}
	n1, n2 := &x509.RevocationList{Number: big.NewInt(1), ThisUpdate: now}, &x509.RevocationList{Number: big.NewInt(2), ThisUpdate: now.Add(-time.Hour)}
	if !mdmCRLNewer(n2, n1) || mdmCRLNewer(n1, n2) {
		t.Errorf("Number outranks ThisUpdate: n2 newer %v, n1 newer %v", mdmCRLNewer(n2, n1), mdmCRLNewer(n1, n2))
	}
}

// After a failed fetch the cache makes no request until its back-off ends: 1, 2, 4, 8 minutes, then 10;
// a good fetch resets it.
func TestMDMCRLCache_BackoffAfterFailure(t *testing.T) {
	e := newMDMEnv(t)
	rn := e.runner(e.leaf(5, -time.Hour, 72*time.Hour))
	e.handle(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusInternalServerError) })
	mustRefuse(t, "first", e.verdict(rn), "status")
	for i := 0; i < 20; i++ {
		e.verdict(rn)
	}
	if got := e.hits.Load(); got != 1 {
		t.Fatalf("21 verdicts after one failure made %d requests, want 1", got)
	}
	want := int32(1)
	for _, wait := range []time.Duration{1, 2, 4, 8, 10, 10} {
		wait *= time.Minute
		e.clock.add(wait - time.Second)
		e.verdict(rn)
		if got := e.hits.Load(); got != want {
			t.Fatalf("1 s before the %v back-off ended: %d requests, want %d", wait, got, want)
		}
		e.clock.add(2 * time.Second)
		e.verdict(rn)
		want++
		if got := e.hits.Load(); got != want {
			t.Fatalf("after the %v back-off: %d requests, want %d", wait, got, want)
		}
	}
	// A good fetch resets the back-off: the next failure waits 1 minute again.
	e.clock.add(10 * time.Minute)
	e.serveCRL(e.crl(1, 48*time.Hour))
	mustPass(t, "recovered", e.verdict(rn))
	e.handle(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "down", http.StatusInternalServerError) })
	e.clock.add(mdmCRLRefetch + time.Minute)
	e.verdict(rn)
	hits := e.hits.Load()
	e.clock.add(mdmCRLMinBackoff + time.Second)
	e.verdict(rn)
	if got := e.hits.Load(); got != hits+1 {
		t.Fatalf("after a success then a failure: %d requests, want %d (a 1 minute back-off)", got, hits+1)
	}
}

// Concurrent verdicts share one fetch.
func TestMDMCRLCache_SharesOneInFlightFetch(t *testing.T) {
	e := newMDMEnv(t)
	rn := e.runner(e.leaf(5, -time.Hour, 72*time.Hour))
	body := e.crl(1, 48*time.Hour)
	release := make(chan struct{})
	e.handle(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		_, _ = w.Write(body)
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	var wg sync.WaitGroup
	verdicts := make([]mdmDeviceVerdict, 20)
	for i := range verdicts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			verdicts[i] = e.verdict(rn)
		}()
	}
	for e.hits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := e.hits.Load(); got != 1 {
		t.Fatalf("20 concurrent verdicts made %d requests, want 1", got)
	}
	for i, v := range verdicts {
		if !v.ok {
			t.Fatalf("verdict %d: %+v, want a pass", i, v)
		}
	}
}

// A hung CRL host does not stall a verdict: the caller's own context, or the fetch's timeout, ends the wait
// and the verdict refuses.
func TestMDMCRLCache_WaitIsBoundedAndFailsClosed(t *testing.T) {
	hang := func(e *mdmEnv) {
		block := make(chan struct{})
		t.Cleanup(func() { close(block) })
		e.handle(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-block:
			case <-r.Context().Done():
			}
		})
	}
	t.Run("the caller's context ends first", func(t *testing.T) {
		e := newMDMEnv(t)
		hang(e)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		start := time.Now()
		v := e.srv.mdmDeviceVerdictFor(ctx, e.runner(e.leaf(5, -time.Hour, 72*time.Hour)))
		mustRefuse(t, "context ended", v, "timeout")
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("the verdict took %v, want it to end with its context", d)
		}
	})
	t.Run("the fetch times out", func(t *testing.T) {
		e := newMDMEnv(t)
		hang(e)
		e.srv.mdmCRLs().timeout = 300 * time.Millisecond
		start := time.Now()
		v := e.verdict(e.runner(e.leaf(5, -time.Hour, 72*time.Hour)))
		mustRefuse(t, "fetch timed out", v, "timeout")
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("the verdict took %v, want it bounded by the fetch timeout", d)
		}
	})
}

// Each way a certificate can be wrong has its own sentence, and a verdict nobody asked for refuses.
func TestMDMDeviceVerdict_RefusalSentences(t *testing.T) {
	e := newMDMEnv(t)
	e.serveCRL(e.crl(1, 48*time.Hour))
	other := newMDMTestCA(t, "Other CA")
	notBefore := e.clock.now().Add(48 * time.Hour)
	v := e.verdict(types.Runner{ID: uuid.New(), Name: "studio"})
	mustRefuse(t, "missing", v, "has not presented an MDM device certificate")
	v = e.verdict(e.runner(e.leaf(5, -72*time.Hour, -time.Hour)))
	mustRefuse(t, "expired", v, "expired on "+e.clock.now().Add(-time.Hour).UTC().Format("2006-01-02"))
	v = e.verdict(e.runner(e.leaf(5, 48*time.Hour, 72*time.Hour)))
	mustRefuse(t, "not yet valid", v, "is not valid until "+notBefore.UTC().Format("2006-01-02"))
	foreign := mdmTestDeviceCert(t, other, 5, e.clock.now().Add(-time.Hour), e.clock.now().Add(time.Hour))
	mustRefuse(t, "foreign CA", e.verdict(e.runner(foreign)), "is not issued by the configured MDM CA")
	e.store.err = errors.New("db down")
	mustRefuse(t, "config unreadable", e.verdict(e.runner(e.leaf(5, -time.Hour, time.Hour))), "try again")
	e.store.err = nil
	e.store.cfg.Runners.MDMDeviceCAPEM = "not a pem"
	mustRefuse(t, "bundle unreadable", e.verdict(e.runner(e.leaf(5, -time.Hour, time.Hour))), "the configured MDM CA could not be read")
	e.store.cfg.Runners = nil
	if v := e.verdict(types.Runner{}); !v.notRequired || v.refusal != "" {
		t.Fatalf("no CA configured: %+v, want notRequired", v)
	}
	// The zero verdict refuses with its fixed sentence at the gate.
	rn := managedMac("studio")
	rn.Name = "studio"
	_, ref := macOSHostGate(rn, false, mdmDeviceVerdict{})
	if ref == nil || ref.Reason != placement.ReasonRunnerPostureUnmet || ref.Detail != "studio was not checked for an MDM device certificate." {
		t.Fatalf("the zero verdict at the gate: %+v", ref)
	}
}

// A renewed certificate passes beside a revoked old one; two revoked ones refuse with the last sentence.
func TestMDMDeviceVerdict_JudgesEveryChain(t *testing.T) {
	e := newMDMEnv(t)
	e.serveCRL(e.crl(1, 48*time.Hour, 5, 6))
	oldC, oldC2, renewed := e.leaf(5, -time.Hour, 24*time.Hour), e.leaf(6, -time.Hour, 24*time.Hour), e.leaf(7, -time.Hour, 24*time.Hour)
	mustPass(t, "revoked then renewed", e.verdict(e.runner(oldC, renewed)))
	mustPass(t, "renewed then revoked", e.verdict(e.runner(renewed, oldC)))
	mustRefuse(t, "both revoked", e.verdict(e.runner(oldC, oldC2)), "was revoked on")
}

// The gate with no CA is the gate as it was; with a verdict it names the verdict's evidence or refusal.
func TestMacOSHostGate_NotRequiredIsUnchanged(t *testing.T) {
	rn := managedMac("studio")
	ev, ref := macOSHostGate(rn, false, deviceNotRequired)
	if ref != nil || len(ev) != 3 || ev[2] != "no MDM device-certificate check configured" {
		t.Fatalf("not required: %v %+v", ev, ref)
	}
	ev, ref = macOSHostGate(rn, false, mdmDeviceVerdict{ok: true, evidence: "the evidence"})
	if ref != nil || len(ev) != 3 || ev[2] != "the evidence" {
		t.Fatalf("a pass: %v %+v", ev, ref)
	}
	_, ref = macOSHostGate(rn, false, mdmRefused("studio has not presented an MDM device certificate."))
	if ref == nil || ref.Reason != placement.ReasonRunnerPostureUnmet || ref.Field != "machine_kind" || ref.Detail != "studio has not presented an MDM device certificate." {
		t.Fatalf("a refusal: %+v", ref)
	}
	// The trust and posture checks still come first.
	rn.TrustClass = types.RunnerTrustPersonal
	if _, ref := macOSHostGate(rn, false, mdmDeviceVerdict{ok: true}); ref == nil || !strings.Contains(ref.Detail, "not marked as company-managed") {
		t.Fatalf("a personal runner with a passing verdict: %+v", ref)
	}
}

func TestMacOSHostGate_ZeroVerdictRefuses(t *testing.T) {
	rn := managedMac("studio")
	if _, ref := macOSHostGate(rn, false, mdmDeviceVerdict{}); ref == nil {
		t.Fatal("the zero verdict passed the gate")
	}
	mc := readyMac()
	if o := macOSVMOffer(rn, runnerwire.Caps{MacOSVM: mc}, true, 0, false, mdmDeviceVerdict{}); o.Offered {
		t.Fatalf("the zero verdict was offered: %+v", o)
	}
	if got := poolMacOSVM([]macOSCandidate{{rn: rn, caps: runnerwire.Caps{MacOSVM: mc}, haveCaps: true}}, false); got.Offered || len(got.Refusals) != 1 {
		t.Fatalf("a candidate with no verdict: %+v", got)
	}
	refused := macCandidate(rn, mc, 0)
	refused.device = mdmRefused("studio has not presented an MDM device certificate.")
	got := poolMacOSVM([]macOSCandidate{refused}, false)
	if got.Offered || len(got.Refusals) != 1 || got.Refusals[0].Why != client.MacOSVMWhyNoPosture || !strings.Contains(got.Refusals[0].Detail, "has not presented") {
		t.Fatalf("a candidate refused by its verdict: %+v", got)
	}
}

// Every non-test macOSVMOffer call passes a verdict that mdmDeviceVerdictFor made: an identifier assigned from
// it, or the candidate's device inside poolMacOSVM, which only poolMacOSVMOf sets.
func TestMacOSVMOfferCallersComputeTheVerdict(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	calls := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			calls += checkVerdictShapes(t, fset, fn)
		}
	}
	if calls != 2 {
		t.Fatalf("found %d macOSVMOffer calls, want the two callers (macOSVMRefusal, poolMacOSVM)", calls)
	}
}

func isVerdictCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "mdmDeviceVerdictFor"
}

// checkVerdictShapes fails on a macOSVMOffer call or a macOSCandidate device assignment in fn that is not
// one of the two allowed shapes, and returns how many macOSVMOffer calls fn holds.
func checkVerdictShapes(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl) (calls int) {
	t.Helper()
	assigned := map[string]bool{} // identifiers assigned from mdmDeviceVerdictFor in fn
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if len(x.Rhs) != len(x.Lhs) {
					continue
				}
				if id, ok := lhs.(*ast.Ident); ok && isVerdictCall(x.Rhs[i]) {
					assigned[id.Name] = true
				}
				if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "device" && (!isVerdictCall(x.Rhs[i]) || fn.Name.Name != "poolMacOSVMOf") {
					t.Errorf("%s: a device field is set outside poolMacOSVMOf, or not from mdmDeviceVerdictFor", fset.Position(x.Pos()))
				}
			}
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok && id.Name == "device" {
				t.Errorf("%s: a macOSCandidate literal names device; poolMacOSVMOf must assign it from mdmDeviceVerdictFor", fset.Position(x.Pos()))
			}
		}
		return true
	})
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "macOSVMOffer" {
			return true
		}
		calls++
		if len(call.Args) < 6 {
			t.Errorf("%s: a macOSVMOffer call without a verdict", fset.Position(call.Pos()))
			return true
		}
		switch a := call.Args[5].(type) {
		case *ast.Ident:
			if !assigned[a.Name] {
				t.Errorf("%s: the verdict %s was not assigned from mdmDeviceVerdictFor", fset.Position(call.Pos()), a.Name)
			}
		case *ast.SelectorExpr:
			if x, ok := a.X.(*ast.Ident); !ok || x.Name != "c" || a.Sel.Name != "device" || fn.Name.Name != "poolMacOSVM" {
				t.Errorf("%s: an unexpected verdict argument", fset.Position(call.Pos()))
			}
		default:
			t.Errorf("%s: a macOSVMOffer verdict that is neither an assigned identifier nor c.device", fset.Position(call.Pos()))
		}
		return true
	})
	return calls
}
