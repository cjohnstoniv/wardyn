// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The Key Vault keys half of fakeKV: GET /keys/{name}[/{version}] and POST
// /keys/{name}/{version}/{wrapkey|unwrapkey|sign}, with real RSA-OAEP-SHA256
// and ECDSA P-256, as the REST reference documents them (api 2025-07-01): a
// crypto operation needs the version, is refused 403 KeyDisabled on a
// disabled one and outside nbf/exp (unwrap excepted), and 403 when the key's
// key_ops do not permit it; an alg other than RSA-OAEP-256 / ES256, or a
// ciphertext that does not decrypt, is 400 BadParameter. A GET of a disabled
// version answers 200 with enabled false. Every crypto call is logged as
// "<op> <alg>" in keyOps.

type fakeKey struct {
	versions []*fakeKeyVersion // creation order; the last is the latest
}

type fakeKeyVersion struct {
	id         string
	kind       string // rsa3072 | rsa4096 | rsa2048 | p256 | p384
	rsa        *rsa.PrivateKey
	ec         *ecdsa.PrivateKey
	ops        []string
	exportable bool
	enabled    bool
	nbf, exp   int64 // unix seconds; 0 = unset
}

// RSA key generation dominates a test binary's time, so each size comes from
// a small pool made once; versions cycle through it.
var (
	rsaPools = map[string]func() []*rsa.PrivateKey{
		"rsa3072": sync.OnceValue(func() []*rsa.PrivateKey { return genRSA(3072, 3) }),
		"rsa4096": sync.OnceValue(func() []*rsa.PrivateKey { return genRSA(4096, 1) }),
		"rsa2048": sync.OnceValue(func() []*rsa.PrivateKey { return genRSA(2048, 1) }),
	}
	rsaNext atomic.Int64
)

func genRSA(bits, n int) []*rsa.PrivateKey {
	out := make([]*rsa.PrivateKey, n)
	for i := range out {
		k, err := rsa.GenerateKey(rand.Reader, bits)
		if err != nil {
			panic(err)
		}
		out[i] = k
	}
	return out
}

func newFakeKeyVersion(kind string, ops []string) *fakeKeyVersion {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	v := &fakeKeyVersion{id: hex.EncodeToString(b), kind: kind, ops: slices.Clone(ops), enabled: true}
	switch kind {
	case "p256", "p384":
		curve := elliptic.P256()
		if kind == "p384" {
			curve = elliptic.P384()
		}
		k, err := ecdsa.GenerateKey(curve, rand.Reader)
		if err != nil {
			panic(err)
		}
		v.ec = k
	default:
		pool := rsaPools[kind]()
		v.rsa = pool[int(rsaNext.Add(1))%len(pool)]
	}
	return v
}

// addKey creates name with one version of kind. ops nil takes the kind's
// fitting set: wrapKey/unwrapKey for RSA, sign/verify for EC.
func (f *fakeKV) addKey(name, kind string, ops []string) {
	if ops == nil {
		ops = []string{"wrapKey", "unwrapKey"}
		if strings.HasPrefix(kind, "p") {
			ops = []string{"sign", "verify"}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys == nil {
		f.keys = map[string]*fakeKey{}
	}
	f.keys[strings.ToLower(name)] = &fakeKey{versions: []*fakeKeyVersion{newFakeKeyVersion(kind, ops)}}
}

// rotateKey adds a version like the latest (az keyvault key rotate) and
// returns its id.
func (f *fakeKV) rotateKey(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.keys[strings.ToLower(name)]
	last := k.versions[len(k.versions)-1]
	v := newFakeKeyVersion(last.kind, last.ops)
	k.versions = append(k.versions, v)
	return v.id
}

// editLatest changes the latest version of name.
func (f *fakeKV) editLatest(name string, fn func(v *fakeKeyVersion)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.keys[strings.ToLower(name)]
	fn(k.versions[len(k.versions)-1])
}

// disableAllBut disables every version of name except keep.
func (f *fakeKV) disableAllBut(name, keep string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.keys[strings.ToLower(name)].versions {
		v.enabled = v.id == keep
	}
}

// enableAll enables every version of name.
func (f *fakeKV) enableAll(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.keys[strings.ToLower(name)].versions {
		v.enabled = true
	}
}

// latestKey returns the latest version of name.
func (f *fakeKV) latestKey(name string) *fakeKeyVersion {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.keys[strings.ToLower(name)]
	return k.versions[len(k.versions)-1]
}

// cryptoCalls counts the logged crypto calls whose op is op; resetCalls
// clears the log.
func (f *fakeKV) cryptoCalls(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.keyOps {
		if strings.HasPrefix(c, op+" ") {
			n++
		}
	}
	return n
}

func (f *fakeKV) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyOps, f.calls = nil, nil
}

// key answers /keys/<name>[/<version>[/<op>]]. Callers hold f.mu.
func (f *fakeKV) key(w http.ResponseWriter, r *http.Request, seg []string) {
	if !kvNameRE.MatchString(seg[0]) {
		kvFail(w, http.StatusBadRequest, "BadParameter", "", "invalid key name")
		return
	}
	name := strings.ToLower(seg[0])
	k := f.keys[name]
	if k == nil {
		kvFail(w, http.StatusNotFound, "KeyNotFound", "", "A key with (name/id) "+name+" was not found in this key vault.")
		return
	}
	v := k.versions[len(k.versions)-1]
	if len(seg) >= 2 && seg[1] != "" {
		if v = k.version(seg[1]); v == nil {
			kvFail(w, http.StatusNotFound, "KeyNotFound", "", "no such version")
			return
		}
	}
	switch {
	case r.Method == http.MethodGet && len(seg) <= 2:
		kvReply(w, http.StatusOK, f.keyBundle(name, v))
	case r.Method == http.MethodPost && len(seg) == 3:
		f.keyOp(w, r, name, v, seg[2])
	default:
		kvFail(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "", r.Method)
	}
}

func (k *fakeKey) version(id string) *fakeKeyVersion {
	for _, v := range k.versions {
		if v.id == id {
			return v
		}
	}
	return nil
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func (f *fakeKV) keyBundle(name string, v *fakeKeyVersion) map[string]any {
	jwk := map[string]any{"kid": f.id("keys", name, v.id), "key_ops": v.ops}
	if v.rsa != nil {
		jwk["kty"] = "RSA"
		jwk["n"] = b64u(v.rsa.N.Bytes())
		jwk["e"] = b64u(big.NewInt(int64(v.rsa.E)).Bytes())
	} else {
		pub, err := v.ec.PublicKey.Bytes() // 0x04 ‖ X ‖ Y
		if err != nil {
			panic(err)
		}
		size := (len(pub) - 1) / 2
		jwk["kty"], jwk["crv"] = "EC", v.ec.Curve.Params().Name
		jwk["x"], jwk["y"] = b64u(pub[1:1+size]), b64u(pub[1+size:])
	}
	attrs := map[string]any{"enabled": v.enabled, "created": 1727712345, "updated": 1727712345, "recoveryLevel": "Recoverable+Purgeable", "exportable": v.exportable}
	if v.nbf != 0 {
		attrs["nbf"] = v.nbf
	}
	if v.exp != 0 {
		attrs["exp"] = v.exp
	}
	return map[string]any{"key": jwk, "attributes": attrs}
}

func (f *fakeKV) keyOp(w http.ResponseWriter, r *http.Request, name string, v *fakeKeyVersion, op string) {
	var body struct {
		Alg   string `json:"alg"`
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		kvFail(w, http.StatusBadRequest, "BadParameter", "", "bad body")
		return
	}
	f.keyOps = append(f.keyOps, op+" "+body.Alg)
	perm := map[string]string{"wrapkey": "wrapKey", "unwrapkey": "unwrapKey", "sign": "sign"}[op]
	now := time.Now().Unix()
	switch {
	case perm == "":
		kvFail(w, http.StatusNotFound, "NotFound", "", "no route")
		return
	case !v.enabled:
		kvFail(w, http.StatusForbidden, "Forbidden", "KeyDisabled", "Operation "+op+" is not allowed on a disabled key.")
		return
	case op != "unwrapkey" && ((v.nbf != 0 && now < v.nbf) || (v.exp != 0 && now >= v.exp)):
		kvFail(w, http.StatusForbidden, "Forbidden", "KeyNotYetValidOrExpired", "Operation "+op+" is not allowed outside the key's validity period.")
		return
	case !slices.Contains(v.ops, perm):
		kvFail(w, http.StatusForbidden, "Forbidden", "", "Operation "+op+" is not permitted on this key.")
		return
	}
	in, err := base64.RawURLEncoding.DecodeString(body.Value)
	if err != nil {
		kvFail(w, http.StatusBadRequest, "BadParameter", "", "value is not base64url")
		return
	}
	var out []byte
	enc := base64.RawURLEncoding
	switch {
	case op == "wrapkey" && body.Alg == "RSA-OAEP-256" && v.rsa != nil:
		out, err = rsa.EncryptOAEP(sha256.New(), rand.Reader, &v.rsa.PublicKey, in, nil)
		enc = base64.StdEncoding // the wrap-key doc's own sample answers standard base64
	case op == "unwrapkey" && body.Alg == "RSA-OAEP-256" && v.rsa != nil:
		out, err = v.rsa.Decrypt(nil, in, &rsa.OAEPOptions{Hash: crypto.SHA256})
		if err == nil && f.shortUnwrap {
			out = out[:len(out)-1]
		}
	case op == "sign" && body.Alg == "ES256" && v.ec != nil && v.ec.Curve == elliptic.P256() && len(in) == sha256.Size:
		var rr, ss *big.Int
		if rr, ss, err = ecdsa.Sign(rand.Reader, v.ec, in); err == nil {
			out = append(rr.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
		}
	default:
		kvFail(w, http.StatusBadRequest, "BadParameter", "", "Invalid algorithm "+body.Alg+" for this key")
		return
	}
	if err != nil {
		kvFail(w, http.StatusBadRequest, "BadParameter", "", "The parameter is incorrect.")
		return
	}
	kvReply(w, http.StatusOK, map[string]any{"kid": f.id("keys", name, v.id), "value": enc.EncodeToString(out)})
}

// The two keys newFakeKEK configures.
const (
	fakeWrapKey = "wardyn-kek"
	fakeSignKey = "wardyn-kek-sig"
)

func fakeKEKConfig(f *fakeKV, tokenFile string) KEKConfig {
	return KEKConfig{
		Key: f.srv.URL + "/keys/" + fakeWrapKey, SigningKey: f.srv.URL + "/keys/" + fakeSignKey,
		Auth: AuthWorkloadIdentity, TenantID: f.tenant, ClientID: f.client,
		FederatedTokenFile: tokenFile, AuthorityHost: f.srv.URL,
	}
}

// openFakeKEK builds and boots a KEK over f's two keys, created (RSA-3072,
// P-256) when f holds no key yet; edit the config with opts first.
func openFakeKEK(t *testing.T, f *fakeKV, opts ...func(*KEKConfig)) (*KEK, error) {
	t.Helper()
	f.mu.Lock()
	missing := f.keys == nil
	f.mu.Unlock()
	if missing {
		f.addKey(fakeWrapKey, "rsa3072", nil)
		f.addKey(fakeSignKey, "p256", nil)
	}
	cfg := fakeKEKConfig(f, writeFile(t, "projected-sa-1\n"))
	for _, o := range opts {
		o(&cfg)
	}
	k, err := newKEK(cfg)
	if err != nil {
		return nil, err
	}
	k.c.backoff = 0
	return k, k.start(t.Context())
}

// newFakeKEK is openFakeKEK, failing the test on error.
func newFakeKEK(t *testing.T, f *fakeKV) *KEK {
	t.Helper()
	k, err := openFakeKEK(t, f)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	return k
}
