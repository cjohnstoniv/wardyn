// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// fakeTransit is the fake Vault's Transit engine, mounted at transit/: an
// aes256-gcm96 key with versions, encrypt and decrypt honouring
// associated_data, and min_decryption_version. A call to rewrap/ or keys/
// fails the test: the documented policy grants neither.
type fakeTransit struct {
	name       string         // the one key; "" = none
	versions   map[int][]byte // version -> 32-byte key
	minDecrypt int
	ignoreAD   bool // a key type that does not bind associated_data
	encrypts   int
	decrypts   int
	ads        []string // the associated_data of each encrypt, as sent
}

// transitKey creates the fake's key named name, at version 1.
func (f *fakeVault) transitKey(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.transit = fakeTransit{name: name, versions: map[int][]byte{}}
	f.rotateLocked()
}

// rotateTransit adds a key version, as `vault write transit/keys/<k>/rotate`.
func (f *fakeVault) rotateTransit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rotateLocked()
}

func (f *fakeVault) rotateLocked() {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	f.transit.versions[len(f.transit.versions)+1] = k
}

func (f *fakeVault) transitRoute(w http.ResponseWriter, p string, body map[string]any) {
	op, key, _ := strings.Cut(p, "/")
	tr := &f.transit
	switch {
	case op != "encrypt" && op != "decrypt":
		f.t.Errorf("wardynd called transit/%s: the documented policy grants only encrypt/ and decrypt/", p)
		fail(w, http.StatusForbidden, "permission denied")
		return
	case f.revoked:
		fail(w, http.StatusForbidden, "permission denied")
		return
	case key != tr.name || tr.name == "":
		fail(w, http.StatusBadRequest, "encryption key not found")
		return
	}
	var aad []byte
	adStr, _ := body["associated_data"].(string)
	if op == "encrypt" {
		tr.ads = append(tr.ads, adStr)
	}
	if s := adStr; s != "" && !tr.ignoreAD {
		var err error
		if aad, err = base64.StdEncoding.DecodeString(s); err != nil {
			fail(w, http.StatusBadRequest, "failed to base64-decode associated data")
			return
		}
	}
	if op == "encrypt" {
		tr.encrypts++
		pt, err := base64.StdEncoding.DecodeString(body["plaintext"].(string))
		if err != nil {
			fail(w, http.StatusBadRequest, "failed to base64-decode plaintext")
			return
		}
		v := len(tr.versions)
		ct, _ := kek.Seal(tr.versions[v], pt, aad)
		reply(w, 200, map[string]any{"data": map[string]any{
			"ciphertext": "vault:v" + strconv.Itoa(v) + ":" + base64.StdEncoding.EncodeToString(ct), "key_version": v}})
		return
	}
	tr.decrypts++
	ctStr, _ := body["ciphertext"].(string)
	rest, _ := strings.CutPrefix(ctStr, "vault:v")
	num, b64, _ := strings.Cut(rest, ":")
	v, _ := strconv.Atoi(num)
	if v < tr.minDecrypt {
		fail(w, http.StatusBadRequest, "ciphertext or signature version is disallowed by policy (too old)")
		return
	}
	k, ok := tr.versions[v]
	raw, err := base64.StdEncoding.DecodeString(b64)
	if !ok || err != nil {
		fail(w, http.StatusBadRequest, "invalid ciphertext")
		return
	}
	pt, err := kek.Open(k, raw, aad)
	if err != nil {
		fail(w, http.StatusBadRequest, "cipher: message authentication failed")
		return
	}
	reply(w, 200, map[string]any{"data": map[string]any{"plaintext": base64.StdEncoding.EncodeToString(pt)}})
}

// openFakeTransit builds a Transit KEK logged in to f with Kubernetes auth,
// over the fake's key "wardyn".
func openFakeTransit(t *testing.T, f *fakeVault) (*Transit, error) {
	t.Helper()
	f.mu.Lock()
	f.jwts["sa-jwt"] = "wardyn"
	f.mu.Unlock()
	tr, err := NewTransit(t.Context(), Config{
		Addr: f.srv.URL, Namespace: f.namespace, Auth: AuthKubernetes, AuthMount: "kubernetes", Role: "wardyn",
		K8sTokenFile: writeFile(t, "sa-jwt\n"),
	}, "transit", "wardyn")
	if tr != nil {
		tr.c.backoff = 0
	}
	return tr, err
}

// newFakeTransit is openFakeTransit over a fresh key, failing the test on error.
func newFakeTransit(t *testing.T, f *fakeVault) *Transit {
	t.Helper()
	f.transitKey("wardyn")
	tr, err := openFakeTransit(t, f)
	if err != nil {
		t.Fatalf("NewTransit: %v", err)
	}
	return tr
}
