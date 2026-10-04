// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek/kektest"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/secretstoretest"
)

// The Key Vault KEK held to the KEK contract against the fake, with every
// hook: a rotation of both keys, disabling every other version, an
// unreachable vault and a disabled key.
func TestKEK_Conformance(t *testing.T) {
	f := newFakeKV(t)
	kektest.Run(t, func(t *testing.T) kek.KEK { return newFakeKEK(t, f) }, kektest.Hooks{
		Rotate: func(*testing.T) { f.rotateKey(fakeWrapKey); f.rotateKey(fakeSignKey) },
		Retire: func(t *testing.T, keep string) {
			if keep == "" {
				f.enableAll(fakeWrapKey)
				f.enableAll(fakeSignKey)
				return
			}
			wv, sv, ok := strings.Cut(keep, "/")
			if !ok {
				t.Fatalf("Retire(%q): not a Key Vault version pair", keep)
			}
			f.disableAllBut(fakeWrapKey, wv)
			f.disableAllBut(fakeSignKey, sv)
		},
		Unreachable: func(*testing.T) func() {
			f.mu.Lock()
			f.force = slices.Repeat([]int{http.StatusServiceUnavailable}, 100)
			f.mu.Unlock()
			return func() { f.mu.Lock(); f.force = nil; f.mu.Unlock() }
		},
		Disable: func(*testing.T) { f.disableAllBut(fakeWrapKey, ""); f.disableAllBut(fakeSignKey, "") },
	})
}

// wrapFields splits a wrap into its five fields, failing the test otherwise.
func wrapFields(t *testing.T, w []byte) []string {
	t.Helper()
	fs, err := decodeFields(w)
	if err != nil || len(fs) != 5 {
		t.Fatalf("decode the wrap = (%d fields, %v)", len(fs), err)
	}
	return fs
}

// A wrap made from the RSA public key alone — what anyone with Key Vault
// Reader and the database could make — never opens, whatever signature it
// carries, and wardynd refuses it before calling unwrapkey.
func TestKEK_ForgedWrapIsRefused(t *testing.T) {
	f := newFakeKV(t)
	k := newFakeKEK(t, f)
	ctx := t.Context()
	bind := kek.Bind("", "wardyn-signing-key")
	honest := wrapFields(t, kektest.Wrap(t, k, kektest.DEK(t), kek.Bind("alice", "pat")))
	wv, sv := honest[1], honest[2]
	pub := &f.latestKey(fakeWrapKey).rsa.PublicKey
	c, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, kektest.DEK(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 64)
	_, _ = rand.Read(random)
	f.resetCalls()
	for what, sig := range map[string]string{"another wrap's signature": honest[4], "64 random bytes": string(random)} {
		_, err := k.Unwrap(ctx, kek.Encode(blobLabel, wv, sv, string(c), sig), bind)
		kektest.Definitive(t, "Unwrap of a public-key wrap with "+what, err)
	}
	if n := f.cryptoCalls("unwrapkey"); n != 0 {
		t.Fatalf("a forged wrap cost %d unwrapkey calls; want 0 (refused on the local verify)", n)
	}
}

// A wrap moved to another row fails the local verify: definitive, and no
// unwrapkey call is made.
func TestKEK_MovedWrapMakesNoUnwrapCall(t *testing.T) {
	f := newFakeKV(t)
	k := newFakeKEK(t, f)
	w := kektest.Wrap(t, k, kektest.DEK(t), kek.Bind("alice", "pat"))
	f.resetCalls()
	_, err := k.Unwrap(t.Context(), w, kek.Bind("bob", "pat"))
	kektest.Definitive(t, "Unwrap under another row", err)
	if !strings.Contains(err.Error(), "not signed for this row") {
		t.Fatalf("Unwrap under another row = %v; want the signature refusal", err)
	}
	if n := f.cryptoCalls("unwrapkey"); n != 0 || len(f.calls) != 0 {
		t.Fatalf("a moved wrap cost %d unwrapkey calls and %v; want no Key Vault call", n, f.calls)
	}
}

// Boot refuses, by name, every key and setting that could not keep the
// contract, and fails closed on an unreachable vault.
func TestKEK_BootRefusesUnfitKeys(t *testing.T) {
	hour := time.Hour
	for _, tc := range []struct {
		name string
		prep func(f *fakeKV)
		cfg  func(c *KEKConfig)
		want string
	}{
		{name: "RSA-2048", prep: func(f *fakeKV) { f.addKey(fakeWrapKey, "rsa2048", nil) }, want: "2048-bit"},
		{name: "wrapping key may decrypt", prep: func(f *fakeKV) { f.addKey(fakeWrapKey, "rsa3072", []string{"wrapKey", "unwrapKey", "decrypt"}) }, want: "key_ops"},
		{name: "wrapping key may sign", prep: func(f *fakeKV) { f.addKey(fakeWrapKey, "rsa3072", []string{"wrapKey", "unwrapKey", "sign"}) }, want: "key_ops"},
		{name: "wrapping key is EC", prep: func(f *fakeKV) { f.addKey(fakeWrapKey, "p256", []string{"wrapKey", "unwrapKey"}) }, want: `kty "EC"`},
		{name: "signing key on P-384", prep: func(f *fakeKV) { f.addKey(fakeSignKey, "p384", nil) }, want: "P-384"},
		{name: "signing key is RSA", prep: func(f *fakeKV) { f.addKey(fakeSignKey, "rsa3072", []string{"sign", "verify"}) }, want: `kty "RSA"`},
		{name: "signing key may wrap", prep: func(f *fakeKV) { f.addKey(fakeSignKey, "p256", []string{"sign", "verify", "wrapKey"}) }, want: "key_ops"},
		{name: "exportable", prep: func(f *fakeKV) { f.editLatest(fakeWrapKey, func(v *fakeKeyVersion) { v.exportable = true }) }, want: "exportable"},
		{name: "latest version disabled", prep: func(f *fakeKV) { f.editLatest(fakeSignKey, func(v *fakeKeyVersion) { v.enabled = false }) }, want: "disabled"},
		{name: "not yet valid", prep: func(f *fakeKV) {
			f.editLatest(fakeWrapKey, func(v *fakeKeyVersion) { v.nbf = time.Now().Add(hour).Unix() })
		}, want: "not valid until"},
		{name: "expired", prep: func(f *fakeKV) {
			f.editLatest(fakeSignKey, func(v *fakeKeyVersion) { v.exp = time.Now().Add(-hour).Unix() })
		}, want: "expired"},
		{name: "key missing", prep: func(f *fakeKV) { f.mu.Lock(); delete(f.keys, fakeWrapKey); f.mu.Unlock() }, want: "404"},
		{name: "403 on GET", prep: func(f *fakeKV) { f.denyPath = "/keys/" + fakeSignKey }, want: "403"},
		{name: "signing key in another vault", cfg: func(c *KEKConfig) { c.SigningKey = "https://other.vault.azure.net/keys/sig" }, want: "same vault"},
		{name: "versioned key id", cfg: func(c *KEKConfig) { c.Key += "/0123456789abcdef0123456789abcdef" }, want: "versionless"},
		{name: "not a key id", cfg: func(c *KEKConfig) { c.Key = strings.Replace(c.Key, "/keys/", "/secrets/", 1) }, want: "not a key identifier"},
		{name: "managed HSM", cfg: func(c *KEKConfig) { c.Key = "https://pool.managedhsm.azure.net/keys/k" }, want: "Managed HSM"},
		{name: "sovereign cloud", cfg: func(c *KEKConfig) { c.Key = "https://kv.vault.azure.cn/keys/k" }, want: "*.vault.azure.net"},
		{name: "plain http", cfg: func(c *KEKConfig) { c.Key = "http://kv.vault.azure.net/keys/k" }, want: "plain http://"},
		{name: "unreachable", prep: func(f *fakeKV) { f.force = slices.Repeat([]int{http.StatusServiceUnavailable}, 100) }, want: "503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeKV(t)
			f.addKey(fakeWrapKey, "rsa3072", nil)
			f.addKey(fakeSignKey, "p256", nil)
			if tc.prep != nil {
				tc.prep(f) // before any call: the fake is idle
			}
			var opts []func(*KEKConfig)
			if tc.cfg != nil {
				opts = append(opts, tc.cfg)
			}
			k, err := openFakeKEK(t, f, opts...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewKEK = (%v, %v); want a refusal containing %q", k, err, tc.want)
			}
		})
	}
}

// Every wrapkey and unwrapkey sent is RSA-OAEP-256, every sign ES256:
// RSA1_5 and RSA-OAEP (SHA-1) are never sent.
func TestKEK_OnlyOAEP256AndES256(t *testing.T) {
	f := newFakeKV(t)
	k := newFakeKEK(t, f)
	dek, bind := kektest.DEK(t), kek.Bind("alice", "pat")
	w := kektest.Wrap(t, k, dek, bind)
	f.rotateKey(fakeWrapKey)
	kektest.MustUnwrap(t, k, w, dek, bind)
	kektest.MustUnwrap(t, k, kektest.Wrap(t, k, dek, bind), dek, bind)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.keyOps) == 0 {
		t.Fatal("no crypto call was logged")
	}
	for _, c := range f.keyOps {
		switch c {
		case "wrapkey RSA-OAEP-256", "unwrapkey RSA-OAEP-256", "sign ES256":
		default:
			t.Errorf("crypto call %q; want only RSA-OAEP-256 wrap/unwrap and ES256 sign", c)
		}
	}
}

// The wrap is five fields: the format label, both versions (the fake's kids),
// the RSA ciphertext and the r‖s signature. Another label or trailing bytes
// are refused without a Key Vault call.
func TestKEK_BlobFormat(t *testing.T) {
	f := newFakeKV(t)
	k := newFakeKEK(t, f)
	bind := kek.Bind("", "k")
	w := kektest.Wrap(t, k, kektest.DEK(t), bind)
	fs := wrapFields(t, w)
	wv, sv := f.latestKey(fakeWrapKey).id, f.latestKey(fakeSignKey).id
	if fs[0] != blobLabel || fs[1] != wv || fs[2] != sv || len(fs[3]) != 384 || len(fs[4]) != 64 {
		t.Fatalf("wrap fields = %q, %q, %q, %d-byte c, %d-byte sig; want %q, %q, %q, 384, 64", fs[0], fs[1], fs[2], len(fs[3]), len(fs[4]), blobLabel, wv, sv)
	}
	if got, err := k.WrapVersion(w); err != nil || got != wv+"/"+sv {
		t.Fatalf("WrapVersion = (%q, %v); want %q", got, err, wv+"/"+sv)
	}
	f.resetCalls()
	_, err := k.Unwrap(t.Context(), kek.Encode("wardyn/kek/azurekv/v2", fs[1], fs[2], fs[3], fs[4]), bind)
	kektest.Definitive(t, "Unwrap of a v2 wrap", err)
	if !strings.Contains(err.Error(), "newer wardynd") {
		t.Fatalf("Unwrap of a v2 wrap = %v; want the newer-wardynd refusal", err)
	}
	_, err = k.Unwrap(t.Context(), append(w, 0, 0, 0, 0), bind)
	kektest.Definitive(t, "Unwrap of a wrap with trailing bytes", err)
	if len(f.calls) != 0 {
		t.Fatalf("malformed wraps made Key Vault calls: %v", f.calls)
	}
}

// Rotating the wrapping key applies at the next Wrap with no restart, and a
// row under the old version still opens.
func TestKEK_RotationAppliesWithoutRestart(t *testing.T) {
	f := newFakeKV(t)
	k := newFakeKEK(t, f)
	dek, bind := kektest.DEK(t), kek.Bind("", "k")
	old := kektest.Wrap(t, k, dek, bind)
	wv2 := f.rotateKey(fakeWrapKey)
	w := kektest.Wrap(t, k, dek, bind)
	if fs := wrapFields(t, w); fs[1] != wv2 {
		t.Fatalf("a wrap after the rotation names %q; want the new version %q", fs[1], wv2)
	}
	kektest.MustUnwrap(t, k, old, dek, bind)
	kektest.MustUnwrap(t, k, w, dek, bind)
}

// Only an unreachable vault is transient; every answer Key Vault gives is
// kek.ErrService, and a missing version is never not-found.
func TestKEK_ErrorClassification(t *testing.T) {
	f := newFakeKV(t)
	k := newFakeKEK(t, f)
	bind := kek.Bind("", "k")
	w := kektest.Wrap(t, k, kektest.DEK(t), bind)
	unwrap := func(force ...int) error {
		f.mu.Lock()
		f.force = force
		f.mu.Unlock()
		_, err := k.Unwrap(t.Context(), w, bind)
		f.mu.Lock()
		f.force = nil
		f.mu.Unlock()
		return err
	}
	if err := unwrap(503, 503, 503, 503); !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("four 503s = %v; want ErrUnavailable", err)
	}
	k.c.lastFetched = time.Time{} // let the 401 fetch a new token, which the vault refuses too
	for name, force := range map[string][]int{"401 after a refresh": {401, 401}, "403": {403}, "404": {404}} {
		err := unwrap(force...)
		kektest.Definitive(t, name, err)
		if !errors.Is(err, kek.ErrService) {
			t.Fatalf("%s = %v; want kek.ErrService", name, err)
		}
		if refused := name != "404"; errors.Is(err, kek.ErrAccess) != refused {
			t.Fatalf("%s = %v; kek.ErrAccess want %v", name, err, refused)
		}
	}
	f.mu.Lock()
	f.shortUnwrap = true
	f.mu.Unlock()
	if err := unwrap(); !errors.Is(err, kek.ErrService) {
		t.Fatalf("a 31-byte unwrap answer = %v; want kek.ErrService", err)
	}
}

// A row naming a signing-key version the vault does not have costs one GET,
// then none until the miss expires: a database writer cannot make every read
// a Key Vault call.
func TestKEK_UnknownSigningVersionIsNegativeCached(t *testing.T) {
	f := newFakeKV(t)
	k := newFakeKEK(t, f)
	bind := kek.Bind("", "k")
	fs := wrapFields(t, kektest.Wrap(t, k, kektest.DEK(t), bind))
	forged := kek.Encode(blobLabel, fs[1], strings.Repeat("ab", 16), fs[3], fs[4])
	f.resetCalls()
	for range 3 {
		_, err := k.Unwrap(t.Context(), forged, bind)
		kektest.Definitive(t, "Unwrap under an unknown signing version", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("three reads of an unknown signing version made %v; want one GET", f.calls)
	}
	k.now = func() time.Time { return time.Now().Add(missTTL + time.Second) }
	_, _ = k.Unwrap(t.Context(), forged, bind)
	if len(f.calls) != 2 {
		t.Fatalf("after the miss expired: %v; want a second GET", f.calls)
	}
}

// NewKEK and a round trip run under GODEBUG=fips140=only: RSA-OAEP-SHA256 at
// the fake, ECDSA P-256 verify here.
func TestKEK_UnderFIPSOnly(t *testing.T) {
	if !secretstoretest.UnderFIPSOnly(t) {
		return
	}
	k := newFakeKEK(t, newFakeKV(t))
	dek, bind := kektest.DEK(t), kek.Bind("alice", "pat")
	kektest.MustUnwrap(t, k, kektest.Wrap(t, k, dek, bind), dek, bind)
}

// The id names the vault host and both keys, lower-cased (Key Vault names are
// case-insensitive); Describe names the vault as the store does.
func TestKEK_IDAndDescribe(t *testing.T) {
	k, err := newKEK(KEKConfig{
		Key: "https://MyVault.vault.azure.net/keys/Wardyn-KEK", SigningKey: "https://myvault.VAULT.azure.net/keys/Wardyn-KEK-Sig",
		Auth: AuthManagedIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "azurekv-key:myvault.vault.azure.net/wardyn-kek/wardyn-kek-sig"; k.ID() != want || !kek.IsServiceID(k.ID()) {
		t.Fatalf("ID = %q; want %q", k.ID(), want)
	}
	if d := k.Describe(); d != "Key Vault myvault" {
		t.Fatalf("Describe = %q", d)
	}
	f := newFakeKV(t)
	k = newFakeKEK(t, f)
	if !regexp.MustCompile(`^azurekv-key:127\.0\.0\.1:\d+/wardyn-kek/wardyn-kek-sig$`).MatchString(k.ID()) {
		t.Fatalf("ID over the fake = %q", k.ID())
	}
}
