// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"

	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// testGatewayCA wires a real client CA into f's server.
func testGatewayCA(t *testing.T, f *gatewayFixture) *gatewayidentity.Authority {
	t.Helper()
	raw, err := gatewayidentity.RotateAuthority(nil, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ca, err := gatewayidentity.NewAuthority(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.cfg.GatewayClientCA = func(_ context.Context, create bool) (*gatewayidentity.Authority, error) {
		if create {
			t.Error("a certificate mint asked for a CA to be made")
		}
		return ca, nil
	}
	return ca
}

func testGatewayCertify(t *testing.T, f *gatewayFixture, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, http.MethodPost, gatewayMintPath, f.token, body)
}

func testGatewayPublicKeyBody(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return `{"public_key":"` + base64.StdEncoding.EncodeToString(der) + `"}`
}

func (f *gatewayFixture) certEvents() []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range f.h.audit.snapshot() {
		if ev.Action == "gateway_identity.certificate.mint" {
			out = append(out, ev)
		}
	}
	return out
}

// A certificate answers {"certificate","expires_at"}; its one URI SAN is the run's SPIFFE ID, it
// verifies at the served bundle as an X509-SVID, and it lives no longer than the provider's token.
// One success row per run on a replica, not one per renewal.
func TestGatewayCertificateNamesTheRun(t *testing.T) {
	f := newGatewayFixture(t)
	testGatewayCA(t, f)
	w := testGatewayCertify(t, f, testGatewayPublicKeyBody(t))
	if w.Code != http.StatusOK {
		t.Fatalf("certify = %d %s", w.Code, w.Body)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body) != 2 {
		t.Fatalf("answer %q: %v, want certificate and expires_at alone", w.Body, err)
	}
	block, _ := pem.Decode([]byte(body["certificate"]))
	if block == nil {
		t.Fatalf("certificate %q is not PEM", body["certificate"])
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	served := testGatewayGet(t, f.srv, http.MethodGet, gatewayClientCARoute)
	if served.Code != http.StatusOK || served.Header().Get("Content-Type") != "application/x-pem-file" || served.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("bundle: %d %v", served.Code, served.Header())
	}
	bundle, err := x509bundle.Parse(spiffeid.RequireTrustDomainFromString("wardyn.local"), served.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := x509svid.Verify([]*x509.Certificate{leaf}, bundle)
	if err != nil {
		t.Fatalf("the served bundle does not verify the certificate: %v", err)
	}
	if want := "spiffe://wardyn.local/agent-run/" + f.run.ID.String(); id.String() != want {
		t.Errorf("SPIFFE ID %s, want %s", id, want)
	}
	if limit := time.Now().Add(10*time.Minute + time.Minute); leaf.NotAfter.After(limit) {
		t.Errorf("NotAfter %v is past the token cap", leaf.NotAfter)
	}
	if w := testGatewayCertify(t, f, testGatewayPublicKeyBody(t)); w.Code != http.StatusOK {
		t.Fatalf("renewal = %d %s", w.Code, w.Body)
	}
	evs := f.certEvents()
	if len(evs) != 1 || evs[0].Outcome != "success" || !strings.Contains(string(evs[0].Data), `"first":true`) {
		t.Fatalf("rows = %+v, want one first success", evs)
	}
	if strings.Contains(string(evs[0].Data), "CERTIFICATE") {
		t.Errorf("the audit row holds the certificate: %s", evs[0].Data)
	}
}

// The certificate follows the token's admission: a provider changed since dispatch is refused
// and recorded, a bad body is a 400, and a missing or unreadable CA is a 503 that names no cause.
func TestGatewayCertificateRefusals(t *testing.T) {
	t.Run("provider changed", func(t *testing.T) {
		f := newGatewayFixture(t)
		testGatewayCA(t, f)
		testGatewayIdentityOn(f.srv)
		p := testGatewayProvider()
		p.GatewayIdentity.Audience = "new-aud"
		f.st.set(func(s *gatewayStore) { s.site = testGatewaySite(p) })
		w := testGatewayCertify(t, f, testGatewayPublicKeyBody(t))
		if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonGatewayIdentityChanged {
			t.Fatalf("certify = %d %s", w.Code, w.Body)
		}
		if evs := f.certEvents(); len(evs) != 1 || evs[0].Outcome != "denied" {
			t.Fatalf("rows = %+v, want the refusal recorded", evs)
		}
	})
	for name, body := range map[string]string{
		"not base64":      `{"public_key":"%%%"}`,
		"not a key":       `{"public_key":"` + base64.StdEncoding.EncodeToString([]byte("key")) + `"}`,
		"too long":        `{"public_key":"` + strings.Repeat("A", maxGatewayPublicKey+4) + `"}`,
		"beside provider": `{"public_key":"QQ==","provider_id":"corp-gw"}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newGatewayFixture(t)
			testGatewayCA(t, f)
			if w := testGatewayCertify(t, f, body); w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonInvalidRequestBody {
				t.Fatalf("certify = %d %s", w.Code, w.Body)
			}
		})
	}
	for name, ca := range map[string]func(context.Context, bool) (*gatewayidentity.Authority, error){
		"not wired": nil,
		"no row":    func(context.Context, bool) (*gatewayidentity.Authority, error) { return nil, nil },
		"unreadable": func(context.Context, bool) (*gatewayidentity.Authority, error) {
			return nil, errors.New("decrypt failed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newGatewayFixture(t)
			f.srv.cfg.GatewayClientCA = ca
			w := testGatewayCertify(t, f, testGatewayPublicKeyBody(t))
			if w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonGatewayIdentityUnavailable {
				t.Fatalf("certify = %d %s", w.Code, w.Body)
			}
			if strings.Contains(w.Body.String(), "decrypt") {
				t.Errorf("the answer names the cause: %s", w.Body)
			}
		})
	}
}

// Off, or on with no CA row, the bundle answers exactly as the key set's sibling path no route
// holds, with and without a console.
func TestGatewayClientCAOffEqualsAnUnregisteredPath(t *testing.T) {
	noCA := func(context.Context, bool) (*gatewayidentity.Authority, error) { return nil, nil }
	for _, ui := range []string{"", testGatewayUIDir(t)} {
		srv := testGatewayServer(t, ui, "", nil)
		srv.cfg.GatewayClientCA = noCA
		for _, on := range []bool{false, true} {
			if on {
				testGatewayIdentityOn(srv)
			}
			got := testGatewayGet(t, srv, http.MethodGet, gatewayClientCARoute)
			want := testGatewayGet(t, srv, http.MethodGet, testGatewaySibling)
			if got.Code != http.StatusNotFound || got.Code != want.Code || got.Body.String() != want.Body.String() {
				t.Errorf("ui=%v on=%v: %d %q, want %d %q", ui != "", on, got.Code, got.Body, want.Code, want.Body)
			}
		}
	}
}
