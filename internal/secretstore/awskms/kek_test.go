// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awskms

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

func testDEK() []byte { return bytes.Repeat([]byte{7}, kek.DEKSize) }

func TestKEK_RoundTripAndBinding(t *testing.T) {
	f := newFakeKMS(t)
	k := newTestKEK(t, f)
	if k.ID() != "awskms:"+testARN || !kek.IsServiceID(k.ID()) {
		t.Fatalf("ID() = %q", k.ID())
	}
	if k.Describe() != "AWS KMS in us-east-1" {
		t.Fatalf("Describe() = %q", k.Describe())
	}
	if _, ok := any(k).(kek.Versioned); ok {
		t.Fatal("the AWS KMS KEK must not be kek.Versioned: KMS rotation is transparent")
	}
	dek := testDEK()
	for _, bind := range []map[string]string{
		kek.Bind("alice@example.com", "pat"),
		kek.PrincipalBind("alice@example.com", "subject", 3, "default"),
	} {
		w, err := k.Wrap(t.Context(), dek, bind)
		if err != nil {
			t.Fatalf("Wrap: %v", err)
		}
		got, err := k.Unwrap(t.Context(), w, bind)
		if err != nil || !bytes.Equal(got, dek) {
			t.Fatalf("Unwrap = (%x, %v), want the data key", got, err)
		}
	}
	for i, alg := range f.algorithms {
		if alg != "SYMMETRIC_DEFAULT" || f.keyIDs[i] != testARN {
			t.Fatalf("call %d: algorithm %q key %q; want SYMMETRIC_DEFAULT under the configured ARN", i, alg, f.keyIDs[i])
		}
	}
}

func TestKEK_RefusesAMovedWrap(t *testing.T) {
	f := newFakeKMS(t)
	k := newTestKEK(t, f)
	w, err := k.Wrap(t.Context(), testDEK(), kek.Bind("alice@example.com", "pat"))
	if err != nil {
		t.Fatal(err)
	}
	for name, bind := range map[string]map[string]string{
		"another owner": kek.Bind("bob@example.com", "pat"),
		"another name":  kek.Bind("alice@example.com", "other"),
		"principal key": kek.PrincipalBind("alice@example.com", "subject", 1, "default"),
	} {
		got, err := k.Unwrap(t.Context(), w, bind)
		if err == nil || got != nil {
			t.Fatalf("%s: Unwrap = (%x, %v); want a refusal", name, got, err)
		}
		if !errors.Is(err, kek.ErrRefused) || errors.Is(err, kek.ErrCorrupt) || errors.Is(err, secretstore.ErrUnavailable) {
			t.Fatalf("%s: err = %v; want kek.ErrRefused (not proof about the row, not transient)", name, err)
		}
	}
	// A wrap made for another KEK id (the same key named another way) is another row's wrap too.
	other := &KEK{client: k.client, arn: k.arn, id: "awskms:other", region: k.region, timeout: k.timeout}
	if _, err := other.Unwrap(t.Context(), w, kek.Bind("alice@example.com", "pat")); !errors.Is(err, kek.ErrRefused) {
		t.Fatalf("Unwrap under another kek id = %v; want kek.ErrRefused", err)
	}
}

func TestKEK_ContextCarriesNoOwnerOrName(t *testing.T) {
	f := newFakeKMS(t)
	k := newTestKEK(t, f)
	const owner, name = "alice@example.com", "github-pat-secret-name"
	bind := kek.Bind(owner, name)
	w, err := k.Wrap(t.Context(), testDEK(), bind)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Unwrap(t.Context(), w, bind); err != nil {
		t.Fatal(err)
	}
	aad, err := kek.WrapAAD(bind, k.ID())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(aad)
	want := hex.EncodeToString(sum[:])
	ctxs := f.allContexts()
	if len(ctxs) < 2 {
		t.Fatalf("saw %d encryption contexts", len(ctxs))
	}
	for _, c := range ctxs[len(ctxs)-2:] {
		if len(c) != 1 || c["wardyn:aad-sha256"] != want {
			t.Fatalf("encryption context = %v; want only wardyn:aad-sha256=%s", c, want)
		}
		for key, v := range c {
			for _, secret := range []string{owner, name, "alice", "github-pat"} {
				if strings.Contains(key, secret) || strings.Contains(v, secret) {
					t.Fatalf("encryption context %q=%q names %q; CloudTrail logs it in clear", key, v, secret)
				}
			}
		}
	}
}

func TestKEK_ErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		is     []error
		not    []error
	}{
		{"access denied", http.StatusBadRequest, "AccessDeniedException", []error{kek.ErrAccess, kek.ErrService}, []error{secretstore.ErrUnavailable}},
		{"403", http.StatusForbidden, "SomethingElse", []error{kek.ErrAccess, kek.ErrService}, []error{secretstore.ErrUnavailable}},
		{"401", http.StatusUnauthorized, "UnrecognizedClientException", []error{kek.ErrAccess, kek.ErrService}, []error{secretstore.ErrUnavailable}},
		{"not found", http.StatusBadRequest, "NotFoundException", []error{kek.ErrKeyMissing, kek.ErrService}, []error{secretstore.ErrUnavailable, kek.ErrAccess}},
		{"disabled", http.StatusBadRequest, "DisabledException", []error{kek.ErrService}, []error{secretstore.ErrUnavailable, kek.ErrRefused, kek.ErrKeyMissing, kek.ErrAccess}},
		{"pending deletion", http.StatusBadRequest, "KMSInvalidStateException", []error{kek.ErrService}, []error{secretstore.ErrUnavailable, kek.ErrRefused, kek.ErrKeyMissing, kek.ErrAccess}},
		{"invalid ciphertext", http.StatusBadRequest, "InvalidCiphertextException", []error{kek.ErrRefused, kek.ErrService}, []error{kek.ErrCorrupt, secretstore.ErrUnavailable}},
		{"incorrect key", http.StatusBadRequest, "IncorrectKeyException", []error{kek.ErrRefused, kek.ErrService}, []error{kek.ErrCorrupt, secretstore.ErrUnavailable}},
		{"throttling", http.StatusBadRequest, "ThrottlingException", []error{secretstore.ErrUnavailable}, []error{kek.ErrService}},
		{"429", http.StatusTooManyRequests, "RequestLimitExceeded", []error{secretstore.ErrUnavailable}, []error{kek.ErrService}},
		{"internal error", http.StatusInternalServerError, "KMSInternalException", []error{secretstore.ErrUnavailable}, []error{kek.ErrService}},
		{"bad gateway", http.StatusBadGateway, "Whatever", []error{secretstore.ErrUnavailable}, []error{kek.ErrService}},
	}
	for _, tc := range tests {
		for _, op := range []string{"Encrypt", "Decrypt"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				f := newFakeKMS(t)
				k := newTestKEK(t, f)
				bind := kek.Bind("alice@example.com", "pat")
				w, err := k.Wrap(t.Context(), testDEK(), bind)
				if err != nil {
					t.Fatal(err)
				}
				f.fail(op, tc.status, tc.code)
				if op == "Encrypt" {
					_, err = k.Wrap(t.Context(), testDEK(), bind)
				} else {
					_, err = k.Unwrap(t.Context(), w, bind)
				}
				if err == nil {
					t.Fatal("want an error")
				}
				for _, want := range tc.is {
					if !errors.Is(err, want) {
						t.Errorf("err = %v; want it to match %v", err, want)
					}
				}
				for _, not := range tc.not {
					if errors.Is(err, not) {
						t.Errorf("err = %v; must not match %v", err, not)
					}
				}
			})
		}
	}
}

func TestKEK_UnreachableAndDeadlineAreTransient(t *testing.T) {
	f := newFakeKMS(t)
	k := newTestKEK(t, f)
	bind := kek.Bind("", "x")
	w, err := k.Wrap(t.Context(), testDEK(), bind)
	if err != nil {
		t.Fatal(err)
	}
	expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := k.Unwrap(expired, w, bind); !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("Unwrap past its deadline = %v; want secretstore.ErrUnavailable", err)
	}
	f.srv.Close()
	if _, err := k.Wrap(t.Context(), testDEK(), bind); !errors.Is(err, secretstore.ErrUnavailable) || errors.Is(err, kek.ErrService) {
		t.Fatalf("Wrap to an unreachable service = %v; want only secretstore.ErrUnavailable", err)
	}
}

func TestKEK_WrapFraming(t *testing.T) {
	f := newFakeKMS(t)
	k := newTestKEK(t, f)
	bind := kek.Bind("", "x")
	w, err := k.Wrap(t.Context(), testDEK(), bind)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(w, kek.Encode(wrapLabel, "")[:4+len(wrapLabel)]) {
		t.Fatalf("a wrap does not start with the %q label", wrapLabel)
	}
	calls := f.callCount("Decrypt")
	for name, tc := range map[string]struct {
		in      []byte
		corrupt bool
	}{
		"another label":         {kek.Encode("wardyn/awskms-wrap/v2", "blob"), false},
		"another key service":   {[]byte("vault:v1:abcdef"), true},
		"empty":                 {nil, false},
		"no ciphertext":         {kek.Encode(wrapLabel), true},
		"empty ciphertext":      {kek.Encode(wrapLabel, ""), true},
		"extra field":           {kek.Encode(wrapLabel, "blob", "more"), true},
		"oversized ciphertext":  {kek.Encode(wrapLabel, strings.Repeat("x", maxBlob+1)), true},
		"truncated after label": {w[:len(w)-1][:4+len(wrapLabel)+2], true},
	} {
		_, err := k.Unwrap(t.Context(), tc.in, bind)
		if err == nil {
			t.Fatalf("%s: want an error", name)
		}
		if errors.Is(err, kek.ErrCorrupt) != tc.corrupt {
			t.Errorf("%s: err = %v; kek.ErrCorrupt = %v, want %v", name, err, !tc.corrupt, tc.corrupt)
		}
		if errors.Is(err, secretstore.ErrUnavailable) {
			t.Errorf("%s: err = %v; a malformed wrap is not transient", name, err)
		}
	}
	if n := f.callCount("Decrypt"); n != calls {
		t.Fatalf("a malformed wrap cost %d Decrypt calls; want 0", n-calls)
	}
}

func TestKEK_RefusesAnAnswerForAnotherKey(t *testing.T) {
	f := newFakeKMS(t)
	k := newTestKEK(t, f)
	bind := kek.Bind("", "x")
	w, err := k.Wrap(t.Context(), testDEK(), bind)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.answerKeyID = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"
	f.mu.Unlock()
	if _, err := k.Wrap(t.Context(), testDEK(), bind); !errors.Is(err, kek.ErrService) {
		t.Fatalf("Wrap answered for another key: %v; want kek.ErrService", err)
	}
	if _, err := k.Unwrap(t.Context(), w, bind); !errors.Is(err, kek.ErrService) {
		t.Fatalf("Unwrap answered for another key: %v; want kek.ErrService", err)
	}
	f.mu.Lock()
	f.answerKeyID, f.shortAnswer = "", true
	f.mu.Unlock()
	if _, err := k.Unwrap(t.Context(), w, bind); !errors.Is(err, kek.ErrService) {
		t.Fatalf("Unwrap answered a short data key: %v; want kek.ErrService", err)
	}
}

func TestKEK_RefusesABadDataKeyOrBind(t *testing.T) {
	k := newTestKEK(t, newFakeKMS(t))
	if _, err := k.Wrap(t.Context(), []byte("short"), kek.Bind("", "x")); err == nil {
		t.Fatal("Wrap of a short data key succeeded")
	}
	if _, err := k.Wrap(t.Context(), testDEK(), map[string]string{kek.BindOwner: "a"}); err == nil {
		t.Fatal("Wrap under an incomplete bind succeeded")
	}
}

func TestNewKEK_RefusesAnAliasOrBareID(t *testing.T) {
	f := newFakeKMS(t)
	useCreds(t, sessionKeyID, "token")
	for name, arn := range map[string]string{
		"empty":             "",
		"bare id":           "00000000-0000-0000-0000-000000000000",
		"alias name":        "alias/wardyn",
		"alias arn":         "arn:aws:kms:us-east-1:111122223333:alias/wardyn",
		"not kms":           "arn:aws:s3:::bucket",
		"no region":         "arn:aws:kms::111122223333:key/00000000-0000-0000-0000-000000000000",
		"no account":        "arn:aws:kms:us-east-1::key/00000000-0000-0000-0000-000000000000",
		"malformed key id":  "arn:aws:kms:us-east-1:111122223333:key/not-a-key-id",
		"key id with slash": "arn:aws:kms:us-east-1:111122223333:key/00000000-0000-0000-0000-000000000000/x",
		"no key prefix":     "arn:aws:kms:us-east-1:111122223333:00000000-0000-0000-0000-000000000000",
	} {
		_, err := NewKEK(t.Context(), Config{KeyARN: arn, Endpoint: f.srv.URL})
		if err == nil {
			t.Errorf("%s: NewKEK(%q) succeeded; want a refusal", name, arn)
			continue
		}
		if !strings.Contains(err.Error(), "WARDYN_AWS_KMS_KEY") {
			t.Errorf("%s: error %q does not name the setting", name, err)
		}
	}
	if n := f.callCount("Encrypt"); n != 0 {
		t.Fatalf("a refused key id cost %d KMS calls; want 0", n)
	}
	if _, err := NewKEK(t.Context(), Config{KeyARN: "alias/x", KeySetting: "WARDYN_OTHER", Endpoint: f.srv.URL}); err == nil || !strings.Contains(err.Error(), "WARDYN_OTHER") {
		t.Fatalf("KeySetting not used in the error: %v", err)
	}
	for _, ok := range []string{
		"arn:aws-us-gov:kms:us-gov-west-1:111122223333:key/00000000-0000-0000-0000-000000000000",
		"arn:aws:kms:eu-west-1:111122223333:key/mrk-0123456789abcdef0123456789abcdef",
	} {
		if _, err := parseKeyARN("WARDYN_AWS_KMS_KEY", ok); err != nil {
			t.Errorf("parseKeyARN(%q) = %v; want it accepted", ok, err)
		}
	}
}

func TestNewKEK_ProbeThatIgnoresContextIsRefused(t *testing.T) {
	f := newFakeKMS(t)
	f.ignoreContext = true
	useCreds(t, sessionKeyID, "token")
	_, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL})
	if err == nil || !strings.Contains(err.Error(), "does not bind a wrap to its row") {
		t.Fatalf("NewKEK over a service that ignores the context = %v; want a boot refusal", err)
	}
}

func TestNewKEK_AFailedProbeFailsBoot(t *testing.T) {
	useCreds(t, sessionKeyID, "token")
	for name, tc := range map[string]struct {
		status int
		code   string
		is     error
	}{
		"access denied": {http.StatusBadRequest, "AccessDeniedException", kek.ErrAccess},
		"missing key":   {http.StatusBadRequest, "NotFoundException", kek.ErrKeyMissing},
		"disabled key":  {http.StatusBadRequest, "DisabledException", kek.ErrService},
		"unreachable":   {http.StatusInternalServerError, "KMSInternalException", secretstore.ErrUnavailable},
	} {
		f := newFakeKMS(t)
		f.fail("Encrypt", tc.status, tc.code)
		_, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL})
		if !errors.Is(err, tc.is) {
			t.Errorf("%s: NewKEK = %v; want a boot failure matching %v", name, err, tc.is)
		}
	}
}

func TestNewKEK_NoCredentialsFailsBoot(t *testing.T) {
	useCreds(t, "", "")
	f := newFakeKMS(t)
	if _, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL}); err == nil {
		t.Fatal("NewKEK without any AWS credentials succeeded")
	}
}

func TestKEK_StaticKeyIsReported(t *testing.T) {
	f := newFakeKMS(t)
	for name, tc := range map[string]struct {
		id, token string
		static    bool
	}{
		"long-lived access key": {longLivedKeyID, "", true},
		"role session":          {sessionKeyID, "token", false},
	} {
		useCreds(t, tc.id, tc.token)
		k, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if k.StaticKey() != tc.static {
			t.Errorf("%s: StaticKey() = %v, want %v", name, k.StaticKey(), tc.static)
		}
	}
}

// The EC2 node's instance role is reported apart from a pod's own role: the default chain's
// credentials from the instance metadata service are shared by every pod on the node.
func TestKEK_NodeRoleIsReported(t *testing.T) {
	f := newFakeKMS(t)
	imds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut:
			_, _ = w.Write([]byte("token"))
		case strings.HasSuffix(r.URL.Path, "/security-credentials/"):
			_, _ = w.Write([]byte("node-role"))
		default:
			_, _ = w.Write([]byte(`{"Code":"Success","AccessKeyId":"` + sessionKeyID + `","SecretAccessKey":"` + strings.Repeat("s", 40) +
				`","Token":"token","Expiration":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
		}
	}))
	t.Cleanup(imds.Close)

	useCreds(t, "", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "false")
	t.Setenv("AWS_EC2_METADATA_SERVICE_ENDPOINT", imds.URL)
	k, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL})
	if err != nil || !k.NodeRole() || k.StaticKey() {
		t.Fatalf("NewKEK under the instance role = (%v, %v); want NodeRole true and StaticKey false", k, err)
	}
	useCreds(t, sessionKeyID, "token")
	if k, err = NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL}); err != nil || k.NodeRole() {
		t.Fatalf("NewKEK under a role session from the shared file = (%v, %v); want NodeRole false", k, err)
	}
}

// A KMS reached over a private CA is reachable only with the CA file the daemon
// is given (WARDYN_TRUSTED_CA_FILE): NewKEK builds its own trust, so a maintenance
// mode that has not installed the boot transport still gets it.
func TestNewKEK_TrustsOnlyTheGivenCA(t *testing.T) {
	f := newFakeKMSTLS(t)
	useCreds(t, sessionKeyID, "token")
	if _, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL}); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("NewKEK over a private CA without the CA file = %v; want an x509 refusal", err)
	}
	if k, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL, CACertFile: f.caFile()}); err != nil || k == nil {
		t.Fatalf("NewKEK over a private CA with the CA file = (%v, %v); want it reachable", k, err)
	}
	empty := t.TempDir() + "/empty.pem"
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL, CACertFile: empty}); err == nil || !strings.Contains(err.Error(), "holds no PEM certificate") {
		t.Fatalf("NewKEK with a CA bundle that holds no certificate = %v; want a refusal", err)
	}
}

// unrelatedCAFile writes a fresh self-signed certificate, which vouches for no
// test server, as a PEM bundle and returns its path.
func unrelatedCAFile(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "unrelated"}, NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "unrelated.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The SDK adds AWS_CA_BUNDLE to its own buildable client and refuses to load
// any other client type, so a corporate-CA install that sets it must still boot,
// and the bundle must still be trusted next to the daemon's CA file.
func TestNewKEK_BootsWithAWSCABundleSet(t *testing.T) {
	f := newFakeKMSTLS(t)
	unrelated := unrelatedCAFile(t)
	useCreds(t, sessionKeyID, "token")

	t.Setenv("AWS_CA_BUNDLE", f.caFile())
	if k, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL}); err != nil || k == nil {
		t.Fatalf("NewKEK with AWS_CA_BUNDLE naming the KMS CA = (%v, %v); want it reachable", k, err)
	}
	t.Setenv("AWS_CA_BUNDLE", unrelated)
	if k, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL, CACertFile: f.caFile()}); err != nil || k == nil {
		t.Fatalf("NewKEK with AWS_CA_BUNDLE and a CA file = (%v, %v); want the CA file still trusted", k, err)
	}
	if _, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL}); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("NewKEK trusting only an unrelated CA = %v; want an x509 refusal", err)
	}
}

// The moved-row probe proves the binding only when KMS itself refuses the
// context. A transient answer fails boot as unavailable, and any other answer
// (here an access error, as a proxy or policy might give) fails it unproven.
func TestNewKEK_MovedProbeNeedsKMSsRefusal(t *testing.T) {
	useCreds(t, sessionKeyID, "token")
	// Decrypt call 1 is the round trip, call 2 the moved probe.
	f := newFakeKMS(t)
	f.failNth("Decrypt", 2, http.StatusServiceUnavailable, "KMSInternalException")
	if _, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL}); !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("NewKEK with a transient moved probe = %v; want secretstore.ErrUnavailable", err)
	}
	f = newFakeKMS(t)
	f.failNth("Decrypt", 2, http.StatusBadRequest, "AccessDeniedException")
	_, err := NewKEK(t.Context(), Config{KeyARN: f.arn, Endpoint: f.srv.URL})
	if err == nil || errors.Is(err, secretstore.ErrUnavailable) || !strings.Contains(err.Error(), "binding is unproven") {
		t.Fatalf("NewKEK with an access error on the moved probe = %v; want the unproven-binding refusal", err)
	}
}
