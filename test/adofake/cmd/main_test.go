// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// TestFront_SignInThenAzureDevOps drives the process the kind walk deploys,
// through its own forward proxy and TLS front: the member signs in by picker,
// the token the Entra fake minted is trusted by the Azure DevOps fake, and
// /_seen attributes the traffic to the member's subject.
func TestFront_SignInThenAzureDevOps(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	certPEM, keyPEM, pool := testCA(t)
	const redirect = "http://127.0.0.1:8580/auth/callback"
	f, err := newFake(config{
		caCertPEM: certPEM, caKeyPEM: keyPEM, clientID: "client-1", clientSecret: "s3cret", redirectURI: redirect,
		identities: []entrafake.Identity{{Username: "admin@x.test", Subject: "sub-a"}, {Username: "member@x.test", Subject: "sub-m"}},
		org:        "contoso", project: "proj", repo: "app", repoDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("newFake: %v", err)
	}
	front := httptest.NewServer(f)
	t.Cleanup(front.Close)
	proxyURL, _ := url.Parse(front.URL)
	c := &http.Client{
		Timeout:       10 * time.Second,
		Transport:     &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	base := "https://login.microsoftonline.com/" + f.tenant

	var doc map[string]any
	getJSON(t, c, base+"/v2.0/.well-known/openid-configuration", "", &doc)
	if doc["issuer"] != base+"/v2.0" {
		t.Fatalf("issuer = %v", doc["issuer"])
	}
	q := url.Values{"client_id": {"client-1"}, "response_type": {"code"}, "redirect_uri": {redirect},
		"scope": {"openid offline_access 499b84ac-1321-427f-aa17-267ca6975798/vso.code 499b84ac-1321-427f-aa17-267ca6975798/vso.project"}, "state": {"st"},
		"code_challenge": {entrafake.Challenge("verifier")}, "code_challenge_method": {"S256"}, "login_hint": {"member@x.test"}}
	resp, err := c.Get(base + "/oauth2/v2.0/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Query().Get("code") == "" {
		t.Fatalf("authorize: %d %s", resp.StatusCode, loc)
	}
	resp, err = c.PostForm(base+"/oauth2/v2.0/token", url.Values{"grant_type": {"authorization_code"},
		"code": {loc.Query().Get("code")}, "code_verifier": {"verifier"}, "redirect_uri": {redirect},
		"client_id": {"client-1"}, "client_secret": {"s3cret"}})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	var tok map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	_ = resp.Body.Close()
	bearer, _ := tok["access_token"].(string)
	if bearer == "" {
		t.Fatalf("token: %d %v", resp.StatusCode, tok)
	}

	getJSON(t, c, "https://dev.azure.com/contoso/_apis/projects?api-version=7.1", bearer, &doc)
	req, _ := http.NewRequest(http.MethodGet, "https://dev.azure.com/contoso/proj/_git/app/info/refs?service=git-upload-pack", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	if resp, err = c.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("git advertise: %v %v", err, resp)
	}
	_ = resp.Body.Close()

	// Anything but the two names is refused at the CONNECT.
	if _, err := c.Get("https://example.com/"); err == nil {
		t.Error("CONNECT to example.com was served, want a refusal")
	}

	var seen struct {
		Callers map[string]seenCaller `json:"callers"`
	}
	getJSON(t, http.DefaultClient, front.URL+"/_seen", "", &seen)
	m := seen.Callers["sub-m"]
	if m.Authorized != 2 || m.Endpoints["projects.get"] != 1 || m.Endpoints["git.advertise"] != 1 {
		t.Errorf("/_seen member = %+v, want the REST read and the advertisement", m)
	}
	if _, ok := seen.Callers["sub-a"]; ok || len(seen.Callers) != 1 {
		t.Errorf("/_seen callers = %+v, want the member only", seen.Callers)
	}
}

func getJSON(t *testing.T, c *http.Client, target, bearer string, into any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, target, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", target, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("GET %s: decode: %v", target, err)
	}
}

func testCA(t *testing.T) (certPEM, keyPEM []byte, pool *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "adofake test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CA: %v", err)
	}
	kder, _ := x509.MarshalPKCS8PrivateKey(key)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pool = x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), pool
}
