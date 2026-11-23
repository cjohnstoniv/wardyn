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
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

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
		NotAfter:              time.Now().Add(24 * time.Hour),
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
