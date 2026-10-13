// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swapDefaultTransport gives the test its own http.DefaultTransport and puts
// the real one back (the returned func), so a maintenance mode's install does not leak.
func swapDefaultTransport() (restore func()) {
	orig := http.DefaultTransport
	http.DefaultTransport = orig.(*http.Transport).Clone()
	return func() { http.DefaultTransport = orig }
}

// The EC2, ECS and EKS Pod Identity credential endpoints are link-local: the
// daemon proxy can never reach them, so a proxied credential call would fail
// the AWS KMS boot with a misleading "no credentials".
func TestBootDaemonProxy_LinkLocalCredentialEndpointsBypass(t *testing.T) {
	tr := freshTransport()
	if err := bootDaemonProxy(tr, testBootFlags("http://proxy.corp.example:3128", "", "", "")); err != nil {
		t.Fatal(err)
	}
	if got, err := tr.Proxy(&http.Request{URL: mustParseURL(t, "https://kms.us-east-1.amazonaws.com/")}); err != nil || got == nil {
		t.Fatalf("Proxy(kms) = %v, %v; want the configured proxy", got, err)
	}
	for _, host := range []string{"169.254.169.254", "169.254.170.2", "169.254.170.23", "[fd00:ec2::23]", "[fd00:ec2::254]"} {
		if got, err := tr.Proxy(&http.Request{URL: mustParseURL(t, "http://"+host+"/v1/credentials")}); err != nil || got != nil {
			t.Errorf("Proxy(%s) = %v, %v; want a direct call", host, got, err)
		}
	}
}

// A maintenance mode that opens the key service (-rewrap and the other modes
// behind withBootTransport) reaches AWS KMS through the same trusted CA and
// daemon proxy as a serving boot: a KMS behind a private CA is reachable only
// with WARDYN_TRUSTED_CA_FILE, and the mode sees the proxy.
func TestWithBootTransport_AWSKMSBehindPrivateCAAndProxy(t *testing.T) {
	noAWSIdentity(t)
	creds := filepath.Join(t.TempDir(), "credentials")
	body := "[default]\naws_access_key_id = " + "AS" + "IA" + strings.Repeat("T", 16) + "\n" + "aws_secret_" + "access_key = " + strings.Repeat("s", 40) + "\n" + "aws_session_" + "token = t\n"
	if err := os.WriteFile(creds, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"__type":"AccessDeniedException","message":"denied"}`)
	}))
	defer srv.Close()
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	t.Setenv("AWS_ENDPOINT_URL_KMS", srv.URL)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func(ca string) (proxied bool, err error) {
		defer swapDefaultTransport()() // each run starts from an untouched shared transport
		f := testBootFlags("http://proxy.corp.example:3128", "", "", "")
		f.trustedCAFile = &ca
		f.vault, f.azure = testExternalFlags("", "")
		f.vault.kek, f.vault.transitMount, f.vault.transitKey, f.vault.transitKeyPlatform, f.vault.awsKMSKey = strp("awskms"), strp("transit"), strp(""), strp(""), strp(testAWSKey)
		err = withBootTransport(f, func(f *bootFlags) error {
			req := &http.Request{URL: mustParseURL(t, "https://kms.us-east-1.amazonaws.com/")}
			if p, perr := http.DefaultTransport.(*http.Transport).Proxy(req); perr == nil && p != nil {
				proxied = true
			}
			_, _, err := buildKEK(t.Context(), f.vault, f.azure, *f.trustedCAFile)
			return err
		})
		return proxied, err
	}
	proxied, err := run(caFile)
	if !proxied {
		t.Error("the mode did not see WARDYN_DAEMON_PROXY_URL on the shared transport")
	}
	if err == nil || !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Fatalf("with the CA file: %v; want KMS reached and its refusal returned", err)
	}
	if _, err = run(""); err == nil || strings.Contains(err.Error(), "AccessDeniedException") || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("without the CA file: %v; want an x509 refusal before KMS answers", err)
	}
}

// buildKEK hands the CA file to the AWS KMS client itself, so the key service
// is reachable behind a private CA even when nothing has installed the boot
// transport (the order a maintenance mode would otherwise depend on).
func TestBuildKEK_AWSKMSTrustsTheCAFileWithoutTheBootTransport(t *testing.T) {
	noAWSIdentity(t)
	defer swapDefaultTransport()()
	creds := filepath.Join(t.TempDir(), "credentials")
	body := "[default]\naws_access_key_id = " + "AS" + "IA" + strings.Repeat("T", 16) + "\n" + "aws_secret_" + "access_key = " + strings.Repeat("s", 40) + "\n" + "aws_session_" + "token = t\n"
	if err := os.WriteFile(creds, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"__type":"AccessDeniedException","message":"denied"}`)
	}))
	defer srv.Close()
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)
	t.Setenv("AWS_ENDPOINT_URL_KMS", srv.URL)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	v, az := testExternalFlags("", "")
	v.kek, v.transitMount, v.transitKey, v.transitKeyPlatform, v.awsKMSKey = strp("awskms"), strp("transit"), strp(""), strp(""), strp(testAWSKey)
	if _, _, err := buildKEK(t.Context(), v, az, caFile); err == nil || !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Fatalf("buildKEK with the CA file = %v; want KMS reached and its refusal returned", err)
	}
}
