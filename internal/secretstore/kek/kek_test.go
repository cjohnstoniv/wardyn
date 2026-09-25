// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package kek

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/secretstoretest"
)

// The golden vectors were computed OUTSIDE Go (Python `cryptography`: HKDF
// with salt=None, AESGCM), so a change to any pinned parameter of design §2.2 —
// hash, salt, info, key length, id fingerprint, field encoding, nonce framing —
// fails here rather than silently re-keying every stored credential. The IKM is
// deliberately not an age key: NewLocal feeds identity.String() to the same
// newLocal these pin (TestNewLocal_DerivesFromTheCanonicalIdentityString).
const (
	goldenIKM       = "wardyn test vector ikm, not an age key"
	goldenRecipient = "wardyn test vector recipient"
	goldenKEK       = "e3c4bea619498fb9c9979a8fa74e54054c6a869ae15b46b047ecf20f2a838199"
	goldenKEKID     = "local:929a37ad7ab6608f"
	goldenOwner     = "alice@corp.example"
	goldenName      = "anthropic-api-key"
	goldenAADKEK    = "0000000d77617264796e2f6b656b2f763100000012616c69636540636f72702e6578616d706c6500000011616e7468726f7069632d6170692d6b6579000000166c6f63616c3a39323961333761643761623636303866"
	goldenAADSecret = "0000001077617264796e2f7365637265742f763100000012616c69636540636f72702e6578616d706c6500000011616e7468726f7069632d6170692d6b6579"
	goldenWrapped   = "101112131415161718191a1bb65118ea7f786ae4089b52166078469a8e9561d5f0417e5bc3b39d14be331fd51de3d4df0cc80da6a8c1b7de36c932e7"
	goldenCT        = "202122232425262728292a2ba1518b1102ec377a7f0f36e3b77d978dbf3bc1eae6ec168bbacc783f86d2ed44ea98fa7184516832"
	goldenValue     = "sk-ant-test-vector-value"
)

// nonceSize is GCM's standard 96-bit nonce, the prefix Seal writes.
const nonceSize = 12

// sealWithNonce is Seal with the nonce fixed, for the golden vectors only:
// production never supplies a nonce (Seal draws it inside the module).
func sealWithNonce(key, nonce, plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead.Seal(append([]byte(nil), nonce...), nonce, plaintext, aad), nil
}

func seq(from byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = from + byte(i)
	}
	return b
}

func TestLocalKEK_GoldenVectors(t *testing.T) {
	l, err := newLocal(goldenIKM, goldenRecipient, "local:", localInfo)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(l.key); got != goldenKEK {
		t.Errorf("KEK = %s, want %s — the HKDF-SHA256(IKM, salt=empty, info=%q) derivation changed", got, goldenKEK, localInfo)
	}
	if l.ID() != goldenKEKID {
		t.Errorf("kek_id = %s, want %s", l.ID(), goldenKEKID)
	}
	aad, err := l.aad(Bind(goldenOwner, goldenName))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(aad); got != goldenAADKEK {
		t.Errorf("AAD_kek = %s, want %s", got, goldenAADKEK)
	}

	dek := seq(0x00, DEKSize)
	wrapped, err := sealWithNonce(l.key, seq(0x10, nonceSize), dek, aad)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(wrapped); got != goldenWrapped {
		t.Errorf("wrapped_dek = %s, want %s", got, goldenWrapped)
	}
	gw, _ := hex.DecodeString(goldenWrapped)
	if got, err := l.Unwrap(context.Background(), gw, Bind(goldenOwner, goldenName)); err != nil || !bytes.Equal(got, dek) {
		t.Errorf("Unwrap(golden) = (%x, %v), want the golden DEK", got, err)
	}

	aadSecret := Encode("wardyn/secret/v1", goldenOwner, goldenName)
	if got := hex.EncodeToString(aadSecret); got != goldenAADSecret {
		t.Errorf("AAD_secret = %s, want %s", got, goldenAADSecret)
	}
	ct, err := sealWithNonce(dek, seq(0x20, nonceSize), []byte(goldenValue), aadSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(ct); got != goldenCT {
		t.Errorf("ciphertext = %s, want %s", got, goldenCT)
	}
	if got, err := Open(dek, ct, aadSecret); err != nil || string(got) != goldenValue {
		t.Errorf("Open(golden) = (%q, %v)", got, err)
	}
}

// TestLocalPurposeKEK_GoldenVectors pins the purpose split (design §2.13 c),
// computed outside Go like the vectors above: each purpose is its own HKDF
// info and its own kek_id, AAD_kek carries that id under the unchanged label,
// and AAD_secret is untouched.
func TestLocalPurposeKEK_GoldenVectors(t *testing.T) {
	cases := map[string]struct{ key, id string }{
		PurposePlatform: {"9cd38afd47776338a67066f214d0e1c207e14827d1fbcc9193b55e5dacaa1080", "local/platform:929a37ad7ab6608f"},
		PurposeCred:     {"5c2159f189fcced495ce3a9b8fb6ebd0e0e89b2c6cc66c1d106ab0fc59cb0c06", "local/cred:929a37ad7ab6608f"},
	}
	for purpose, want := range cases {
		l, err := newLocal(goldenIKM, goldenRecipient, "local/"+purpose+":", localInfo+"/"+purpose)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(l.key); got != want.key {
			t.Errorf("%s KEK = %s, want %s", purpose, got, want.key)
		}
		if l.ID() != want.id {
			t.Errorf("%s kek_id = %s, want %s", purpose, l.ID(), want.id)
		}
	}
	cred, _ := newLocal(goldenIKM, goldenRecipient, "local/cred:", localInfo+"/cred")
	aad, err := cred.aad(Bind(goldenOwner, goldenName))
	if err != nil {
		t.Fatal(err)
	}
	const wantAAD = "0000000d77617264796e2f6b656b2f763100000012616c69636540636f72702e6578616d706c6500000011616e7468726f7069632d6170692d6b65790000001b6c6f63616c2f637265643a39323961333761643761623636303866"
	if got := hex.EncodeToString(aad); got != wantAAD {
		t.Errorf("AAD_kek (cred) = %s, want %s", got, wantAAD)
	}
}

// TestLocalPurposeKEK_OneIdentityTwoUnrelatedKeys: the platform and credential
// KEKs of one age key, and the pre-split KEK, are three keys, and a wrap made
// under any one of them opens under neither other — so a credential KEK can
// never unwrap (or forge) a platform row.
func TestLocalPurposeKEK_OneIdentityTwoUnrelatedKeys(t *testing.T) {
	ctx := context.Background()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	platform, err := NewLocalPurpose(id, PurposePlatform)
	if err != nil {
		t.Fatal(err)
	}
	cred, _ := NewLocalPurpose(id, PurposeCred)
	legacy, _ := NewLocal(id)
	keks := []*Local{platform, cred, legacy}
	for i, a := range keks {
		wrapped, err := a.Wrap(ctx, seq(0x40, DEKSize), Bind("", "wardyn-signing-key"))
		if err != nil {
			t.Fatal(err)
		}
		for j, b := range keks {
			_, err := b.Unwrap(ctx, wrapped, Bind("", "wardyn-signing-key"))
			if (i == j) != (err == nil) {
				t.Errorf("wrapped by %s, unwrapped by %s: err = %v", a.ID(), b.ID(), err)
			}
		}
	}
	if _, err := NewLocalPurpose(id, "people"); err == nil {
		t.Error("NewLocalPurpose accepted a purpose that is not platform or cred")
	}
}

// TestNewLocal_DerivesFromTheCanonicalIdentityString pins the one step the
// golden vectors leave to code: the IKM is identity.String() and the id is
// taken over identity.Recipient().String(), and both are stable for the same
// key across a restart (a fresh parse).
func TestNewLocal_DerivesFromTheCanonicalIdentityString(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewLocal(id)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := newLocal(id.String(), id.Recipient().String(), "local:", localInfo)
	reparsed, err := age.ParseX25519Identity(id.String())
	if err != nil {
		t.Fatal(err)
	}
	again, _ := NewLocal(reparsed)
	if !bytes.Equal(got.key, want.key) || got.id != want.id || !bytes.Equal(again.key, got.key) || again.id != got.id {
		t.Fatal("NewLocal does not derive from the canonical identity string, or is not stable across a re-parse")
	}
	if !strings.HasPrefix(got.ID(), "local:") || len(got.ID()) != len("local:")+16 {
		t.Errorf("kek_id %q is not local:<16 hex>", got.ID())
	}
}

func TestEncode_IsInjective(t *testing.T) {
	pairs := [][2][]string{
		{{"ab", "c"}, {"a", "bc"}},
		{{"", "x"}, {"x", ""}},
		{{"a"}, {"a", ""}},
	}
	for _, p := range pairs {
		if bytes.Equal(Encode(p[0]...), Encode(p[1]...)) {
			t.Errorf("Encode(%q) == Encode(%q)", p[0], p[1])
		}
	}
}

// TestLocalKEK_RefusesAnyOtherBinding: a wrap only opens for the (owner, name)
// it was made for, under the key that made it.
func TestLocalKEK_RefusesAnyOtherBinding(t *testing.T) {
	ctx := context.Background()
	a, _ := newLocal("ikm-a", "rcpt-a", "local:", localInfo)
	b, _ := newLocal("ikm-b", "rcpt-b", "local:", localInfo)
	dek := seq(0x40, DEKSize)
	wrapped, err := a.Wrap(ctx, dek, Bind("alice", "k"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		k    *Local
		bind map[string]string
	}{
		"another owner":    {a, Bind("bob", "k")},
		"the operator":     {a, Bind("", "k")},
		"another name":     {a, Bind("alice", "k2")},
		"another KEK":      {b, Bind("alice", "k")},
		"a bind w/o owner": {a, map[string]string{BindName: "k"}},
	}
	for label, c := range cases {
		if got, err := c.k.Unwrap(ctx, wrapped, c.bind); err == nil {
			t.Errorf("%s: Unwrap succeeded (%x)", label, got)
		}
	}
	if _, err := a.Wrap(ctx, dek, map[string]string{BindOwner: "alice"}); err == nil {
		t.Error("Wrap accepted a bind without a name")
	}
	if got, err := a.Unwrap(ctx, wrapped, Bind("alice", "k")); err != nil || !bytes.Equal(got, dek) {
		t.Errorf("Unwrap under its own bind = (%x, %v)", got, err)
	}
}

// TestSeal_NeverRepeatsANonce: the same key and plaintext sealed many times
// must never produce the same nonce. A repeat under one AES-GCM key leaks the
// XOR of two plaintexts and the authentication key.
func TestSeal_NeverRepeatsANonce(t *testing.T) {
	key := seq(0x60, DEKSize)
	seen := map[string]bool{}
	for i := 0; i < 20000; i++ {
		ct, err := Seal(key, []byte("same"), nil)
		if err != nil {
			t.Fatal(err)
		}
		n := string(ct[:nonceSize])
		if seen[n] {
			t.Fatalf("nonce %x repeated after %d seals", ct[:nonceSize], i)
		}
		seen[n] = true
	}
}

// TestNewLocal_RefusesUnderFIPSOnly: GODEBUG=fips140=only forbids X25519, and
// age swallows that error, leaving every identity with the same empty
// recipient. NewLocal must refuse rather than give every age key one kek_id.
func TestNewLocal_RefusesUnderFIPSOnly(t *testing.T) {
	if !secretstoretest.UnderFIPSOnly(t) {
		return
	}
	for range 2 {
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		l, err := NewLocal(id)
		if err == nil {
			t.Fatalf("NewLocal derived %s under fips140=only", l.ID())
		}
		if !strings.Contains(err.Error(), "WARDYN_SECRET_STORE=vaultkv") {
			t.Fatalf("the refusal does not name the FIPS-only way out: %v", err)
		}
	}
}
