// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// KEKConfig configures the Key Vault KEK (WARDYN_KEK=azurekv, docs/ENV.md).
// The identity settings are the store's (Config).
type KEKConfig struct {
	// Key and SigningKey are versionless key identifiers in one vault,
	// https://<vault>.vault.azure.net/keys/<name>: the RSA key that wraps each
	// data key and the EC P-256 key that signs each wrap.
	Key, SigningKey                                                         string
	Auth, TenantID, ClientID, FederatedTokenFile, AuthorityHost, CACertFile string
	Timeout                                                                 time.Duration
	// KeySetting and SigningKeySetting name the settings Key and SigningKey
	// came from, for the boot errors; empty = WARDYN_AZURE_KEK_KEY and
	// WARDYN_AZURE_KEK_SIGNING_KEY. The platform pair sets its own.
	KeySetting, SigningKeySetting string
}

// KEK is the Azure Key Vault KEK (WARDYN_KEK=azurekv). Each data key is
// wrapped by the RSA key's wrapkey (RSA-OAEP-256), and the wrap is signed by
// the EC key's sign (ES256) over the row's AAD_kek, both key versions and the
// ciphertext:
//
//	msg         = Encode(sigLabel, AAD_kek, wv, sv, c)
//	wrapped_dek = Encode(blobLabel, wv, sv, c, sig)
//
// RSA alone binds nothing and forges easily: anyone holding the public key
// (Key Vault Reader) can wrap a DEK of their own choosing for any row. The
// signature needs the private EC key, which never leaves Key Vault, so an
// Unwrap verifies it locally first, and a moved or forged wrap is refused
// without a Key Vault call: unwrapkey only ever sees a ciphertext Key Vault
// itself signed for this row. The signature bytes are not an identity, so
// low-s is not enforced. Only the two versioned crypto calls and GET key are
// made; alg is fixed, never read from a row.
type KEK struct {
	c                  *client
	wrapName, signName string // lowercase
	id                 string
	now                func() time.Time

	mu sync.Mutex
	// pubs are the signing-key versions seen enabled and P-256, for the life
	// of the process; misses are versions Key Vault refused, until retry.
	pubs   map[string]*ecdsa.PublicKey
	misses map[string]miss
}

type miss struct {
	until time.Time
	err   error
}

var _ kek.Versioned = (*KEK)(nil)

const (
	// blobLabel is the wrap's format; v2 is reserved for an oct-HSM A256GCM
	// wrap once Key Vault Premium's symmetric keys are GA.
	blobLabel = "wardyn/kek/azurekv/v1"
	sigLabel  = "wardyn/kek/azurekv/sig/v1"
	// missTTL is how long a refused signing-key version is not asked for
	// again: a database writer can cost the vault one GET per bad version per
	// missTTL, not one per read.
	missTTL   = time.Minute
	maxMisses = 1024
	probeName = "wardyn-kek-probe"
)

var (
	keyNameRE = regexp.MustCompile(`^[0-9a-zA-Z-]{1,127}$`)
	versionRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// NewKEK validates cfg, fetches a first token and proves the keys fit before
// any row depends on them: a probe data key must round-trip, and must not
// unwrap under another row. Any failure, a transient one included, fails
// boot (K6).
func NewKEK(ctx context.Context, cfg KEKConfig) (*KEK, error) {
	k, err := newKEK(cfg)
	if err != nil {
		return nil, err
	}
	return k, k.start(ctx)
}

func newKEK(cfg KEKConfig) (*KEK, error) {
	keySetting, sigSetting := cmp.Or(cfg.KeySetting, "WARDYN_AZURE_KEK_KEY"), cmp.Or(cfg.SigningKeySetting, "WARDYN_AZURE_KEK_SIGNING_KEY")
	wrapURL, wrapName, err := parseKeyID(keySetting, cfg.Key)
	if err != nil {
		return nil, err
	}
	signURL, signName, err := parseKeyID(sigSetting, cfg.SigningKey)
	if err != nil {
		return nil, err
	}
	if wrapURL.Scheme != signURL.Scheme || !strings.EqualFold(wrapURL.Host, signURL.Host) {
		return nil, fmt.Errorf("%s is in %s, %s in %s; both keys must be in the same vault", sigSetting, signURL.Host, keySetting, wrapURL.Host)
	}
	if wrapName == signName {
		return nil, fmt.Errorf("%s and %s name the same key; the wrapping key and the signing key are two keys", keySetting, sigSetting)
	}
	c, err := newClient(Config{
		VaultURL: wrapURL.Scheme + "://" + wrapURL.Host, Auth: cfg.Auth, TenantID: cfg.TenantID, ClientID: cfg.ClientID,
		FederatedTokenFile: cfg.FederatedTokenFile, AuthorityHost: cfg.AuthorityHost, CACertFile: cfg.CACertFile, Timeout: cfg.Timeout,
	})
	if err != nil {
		return nil, err
	}
	return &KEK{
		c: c, wrapName: wrapName, signName: signName, now: time.Now,
		id:   kek.AzureKeyIDPrefix + c.host + "/" + wrapName + "/" + signName,
		pubs: map[string]*ecdsa.PublicKey{}, misses: map[string]miss{},
	}, nil
}

// parseKeyID checks a versionless key identifier and returns it with the
// key's lowercase name. The token is scoped to the public cloud's Key Vault
// (client scope), so a Managed HSM or a sovereign cloud's vault is refused
// here by name rather than by a bare 401.
func parseKeyID(setting, raw string) (*url.URL, string, error) {
	u, err := httpsURL(setting, raw)
	if err != nil {
		return nil, "", err
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case strings.HasSuffix(host, ".managedhsm.azure.net"):
		return nil, "", fmt.Errorf("%s %q is a Managed HSM key; only a Key Vault key (https://<vault>.vault.azure.net/keys/<name>) is supported", setting, raw)
	case !strings.HasSuffix(host, ".vault.azure.net") && !loopback(host):
		return nil, "", fmt.Errorf("%s %q is not in a public-cloud Key Vault (*.vault.azure.net); sovereign clouds are not supported", setting, raw)
	}
	name, ok := strings.CutPrefix(u.Path, "/keys/")
	switch {
	case !ok || name == "":
		return nil, "", fmt.Errorf("%s %q is not a key identifier; want https://<vault>.vault.azure.net/keys/<name>", setting, raw)
	case strings.Contains(name, "/"):
		return nil, "", fmt.Errorf("%s %q names a key version; give the versionless id: wardynd follows the latest version and `wardynd -rewrap` moves rows onto it", setting, raw)
	case !keyNameRE.MatchString(name):
		return nil, "", fmt.Errorf("%s %q: the key name must be 1-127 letters, digits and \"-\"", setting, raw)
	}
	return u, strings.ToLower(name), nil
}

// KeyIdentity is the normalised identity of a versionless key id: its
// lowercase vault host and lowercase key name, which is how Key Vault tells
// two keys apart. Two spellings that differ only by case name one key, so a
// check that two settings name different keys compares these, never the raw
// strings.
func KeyIdentity(setting, raw string) (string, error) {
	u, name, err := parseKeyID(setting, raw)
	if err != nil {
		return "", err
	}
	return strings.ToLower(u.Host) + "/" + name, nil
}

func (k *KEK) start(ctx context.Context) error {
	if _, err := k.c.accessToken(ctx); err != nil {
		return fmt.Errorf("key vault %s: %w", k.c.host, err)
	}
	if err := k.selfTest(ctx); err != nil {
		return fmt.Errorf("key vault KEK %s: %w", k.id, err)
	}
	return nil
}

func (k *KEK) selfTest(ctx context.Context) error {
	dek := make([]byte, kek.DEKSize)
	if _, err := rand.Read(dek); err != nil {
		return err
	}
	bind := kek.Bind("", probeName)
	w, err := k.Wrap(ctx, dek, bind)
	if err != nil {
		return err
	}
	got, err := k.Unwrap(ctx, w, bind)
	if err != nil {
		return fmt.Errorf("a probe data key did not unwrap: %w", err)
	}
	if !bytes.Equal(got, dek) {
		return errors.New("a probe data key unwrapped to different bytes")
	}
	_, err = k.Unwrap(ctx, w, kek.Bind("", probeName+"-moved"))
	switch {
	case err == nil:
		return errors.New("refusing it: a probe data key unwrapped under another row, so this KEK does not bind a wrap to its row")
	case errors.Is(err, secretstore.ErrUnavailable):
		return err
	}
	return nil
}

// ID implements kek.KEK: "azurekv-key:<vault-host>/<key>/<signing-key>".
func (k *KEK) ID() string { return k.id }

// Describe names the key service for an operator ("Key Vault myvault"), the
// store's spelling.
func (k *KEK) Describe() string {
	vault, _, _ := strings.Cut(k.c.host, ".")
	return "Key Vault " + vault
}

// Wrap implements kek.KEK. It reads both keys' latest versions first, so a
// rotation applies at the next write with no restart, and a latest version
// that does not fit refuses writes by name rather than writing weaker rows.
func (k *KEK) Wrap(ctx context.Context, dek []byte, bind map[string]string) ([]byte, error) {
	if len(dek) != kek.DEKSize {
		return nil, fmt.Errorf("azurekv KEK: data key is %d bytes, want %d", len(dek), kek.DEKSize)
	}
	aad, err := kek.WrapAAD(bind, k.id)
	if err != nil {
		return nil, fmt.Errorf("azurekv KEK: %w", err)
	}
	// ponytail: two GETs per wrap; cache them only if writes ever get hot.
	wv, size, err := k.latestWrapping(ctx)
	if err != nil {
		return nil, err
	}
	sv, pub, err := k.latestSigning(ctx)
	if err != nil {
		return nil, err
	}
	c, err := k.crypt(ctx, k.wrapName, wv, "wrapkey", "RSA-OAEP-256", dek)
	if err != nil {
		return nil, err
	}
	if len(c) != size {
		return nil, k.serviceErr(fmt.Errorf("wrapkey answered %d bytes, want the %d-byte modulus", len(c), size))
	}
	msg := sigMsg(aad, wv, sv, c)
	sig, err := k.crypt(ctx, k.signName, sv, "sign", "ES256", digest(msg))
	if err != nil {
		return nil, err
	}
	if !verify(pub, msg, sig) {
		return nil, k.serviceErr(fmt.Errorf("sign answered a signature that does not verify under %s version %s", k.signName, sv))
	}
	return kek.Encode(blobLabel, wv, sv, string(c), string(sig)), nil
}

// Unwrap implements kek.KEK: parse, verify the signature for this row, and
// only then unwrapkey. A malformed, moved or forged wrap is a definitive
// refusal with no Key Vault call (a signing-key version not yet seen costs
// one GET per missTTL), and kek.ErrCorrupt: the versions a wrap names are
// globally unique, so it is proof about the row.
func (k *KEK) Unwrap(ctx context.Context, wrapped []byte, bind map[string]string) ([]byte, error) {
	aad, err := kek.WrapAAD(bind, k.id)
	if err != nil {
		return nil, fmt.Errorf("azurekv KEK: %w", err)
	}
	b, err := parseWrap(wrapped)
	switch {
	case errors.Is(err, errUnknownFormat):
		return nil, fmt.Errorf("azurekv KEK %s: the row holds %w", k.id, err) // not proof about the row: a reader fails closed
	case err != nil:
		return nil, fmt.Errorf("azurekv KEK %s: %w: the row holds %w", k.id, kek.ErrCorrupt, err)
	}
	pub, err := k.signingVersion(ctx, b.sv)
	if err != nil {
		return nil, err
	}
	if !verify(pub, sigMsg(aad, b.wv, b.sv, b.c), b.sig) {
		return nil, fmt.Errorf("azurekv KEK %s: %w: the wrap is not signed for this row: moved, forged or corrupted", k.id, kek.ErrCorrupt)
	}
	dek, err := k.crypt(ctx, k.wrapName, b.wv, "unwrapkey", "RSA-OAEP-256", b.c)
	if err != nil {
		return nil, err
	}
	if len(dek) != kek.DEKSize {
		return nil, k.serviceErr(fmt.Errorf("unwrapkey did not answer a %d-byte data key", kek.DEKSize))
	}
	return dek, nil
}

// WrapVersion implements kek.Versioned: "<wrapping version>/<signing version>".
func (k *KEK) WrapVersion(wrapped []byte) (string, error) {
	b, err := parseWrap(wrapped)
	if err != nil {
		return "", err
	}
	return b.wv + "/" + b.sv, nil
}

// LatestVersion implements kek.Versioned: both keys' latest versions, as a
// wrap made now would name them.
func (k *KEK) LatestVersion(ctx context.Context) (string, error) {
	wv, _, err := k.latestWrapping(ctx)
	if err != nil {
		return "", err
	}
	sv, _, err := k.latestSigning(ctx)
	if err != nil {
		return "", err
	}
	return wv + "/" + sv, nil
}

type wrap struct {
	wv, sv string
	c, sig []byte
}

var errUnknownFormat = errors.New("a wrap whose format is not " + blobLabel + ": written by a newer wardynd, or not a Key Vault wrap")

func parseWrap(w []byte) (wrap, error) {
	fs, err := decodeFields(w)
	switch {
	case err != nil:
		return wrap{}, fmt.Errorf("no Key Vault wrap: %w", err)
	case len(fs) == 0 || fs[0] != blobLabel:
		return wrap{}, errUnknownFormat
	case len(fs) != 5:
		return wrap{}, fmt.Errorf("a Key Vault wrap of %d fields, want 5", len(fs))
	case !versionRE.MatchString(fs[1]) || !versionRE.MatchString(fs[2]):
		return wrap{}, errors.New("a Key Vault wrap naming a malformed key version")
	case (len(fs[3]) != 384 && len(fs[3]) != 512) || len(fs[4]) != 64:
		return wrap{}, errors.New("a Key Vault wrap with a malformed ciphertext or signature")
	}
	return wrap{wv: fs[1], sv: fs[2], c: []byte(fs[3]), sig: []byte(fs[4])}, nil
}

// decodeFields reverses kek.Encode exactly: every byte belongs to a field.
func decodeFields(b []byte) ([]string, error) {
	var out []string
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, errors.New("truncated")
		}
		n := binary.BigEndian.Uint32(b)
		b = b[4:]
		if uint64(n) > uint64(len(b)) {
			return nil, errors.New("truncated")
		}
		out = append(out, string(b[:n]))
		b = b[n:]
	}
	return out, nil
}

func sigMsg(aad []byte, wv, sv string, c []byte) []byte {
	return kek.Encode(sigLabel, string(aad), wv, sv, string(c))
}

func digest(msg []byte) []byte {
	d := sha256.Sum256(msg)
	return d[:]
}

// verify checks an ES256 signature in its JWS form, r‖s (RFC 7518 §3.4).
func verify(pub *ecdsa.PublicKey, msg, sig []byte) bool {
	if len(sig) != 64 {
		return false
	}
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	return ecdsa.Verify(pub, digest(msg), r, s)
}

// keyBundle is GET key's answer: the public JWK and the version's attributes.
type keyBundle struct {
	Key struct {
		Kid    string   `json:"kid"`
		Kty    string   `json:"kty"`
		KeyOps []string `json:"key_ops"`
		N      string   `json:"n"`
		Crv    string   `json:"crv"`
		X      string   `json:"x"`
		Y      string   `json:"y"`
	} `json:"key"`
	Attributes struct {
		Enabled    bool   `json:"enabled"`
		Nbf        *int64 `json:"nbf"`
		Exp        *int64 `json:"exp"`
		Exportable bool   `json:"exportable"`
	} `json:"attributes"`
}

// getKey reads name at version ("" for the latest) and returns the answer
// and the version its kid names.
func (k *KEK) getKey(ctx context.Context, name, version string) (keyBundle, string, error) {
	var kb keyBundle
	path := "keys/" + name
	if version != "" {
		path += "/" + version
	}
	if err := k.call(ctx, http.MethodGet, path, nil, &kb); err != nil {
		return kb, "", err
	}
	v, err := k.kidVersion(kb.Key.Kid, name)
	if err != nil {
		return kb, "", k.serviceErr(fmt.Errorf("GET %s answered %w", path, err))
	}
	if version != "" && v != version {
		return kb, "", k.serviceErr(fmt.Errorf("GET %s answered version %s", path, v))
	}
	return kb, v, nil
}

// kidVersion is the version of a kid that must name key name on this vault.
func (k *KEK) kidVersion(kid, name string) (string, error) {
	u, err := url.Parse(kid)
	if err != nil || !strings.EqualFold(u.Host, k.c.base.Host) {
		return "", fmt.Errorf("a kid %q off this vault", kid)
	}
	rest, ok := strings.CutPrefix(u.Path, "/keys/")
	n, v, _ := strings.Cut(rest, "/")
	if !ok || !strings.EqualFold(n, name) || !versionRE.MatchString(v) {
		return "", fmt.Errorf("a kid %q that is not a version of %s", kid, name)
	}
	return v, nil
}

// usable refuses a version that is disabled, exportable, or outside its
// nbf/exp window, which Key Vault would refuse wrapkey and sign under.
func (k *KEK) usable(name, v string, kb keyBundle) error {
	a := kb.Attributes
	now := k.now()
	switch {
	case !a.Enabled:
		return fmt.Errorf("%s version %s is disabled; enable it, or rotate the key", name, v)
	case a.Exportable:
		return fmt.Errorf("%s version %s is exportable, so it could leave Key Vault; create the key without --exportable", name, v)
	case a.Nbf != nil && now.Before(time.Unix(*a.Nbf, 0)):
		return fmt.Errorf("%s version %s is not valid until %s", name, v, time.Unix(*a.Nbf, 0).UTC().Format(time.RFC3339))
	case a.Exp != nil && !now.Before(time.Unix(*a.Exp, 0)):
		return fmt.Errorf("%s version %s expired at %s; rotate the key", name, v, time.Unix(*a.Exp, 0).UTC().Format(time.RFC3339))
	}
	return nil
}

// sameOps reports whether ops is exactly want, in any order.
func sameOps(ops []string, want ...string) bool {
	return slices.Equal(slices.Sorted(slices.Values(ops)), slices.Sorted(slices.Values(want)))
}

// latestWrapping reads the wrapping key's latest version and returns it and
// its modulus length in bytes.
func (k *KEK) latestWrapping(ctx context.Context) (string, int, error) {
	kb, v, err := k.getKey(ctx, k.wrapName, "")
	if err != nil {
		return "", 0, err
	}
	refuse := func(format string, a ...any) (string, int, error) {
		return "", 0, fmt.Errorf("azurekv KEK %s: refusing the wrapping key: %s", k.id, fmt.Sprintf(format, a...))
	}
	if err := k.usable(k.wrapName, v, kb); err != nil {
		return refuse("%v", err)
	}
	if kb.Key.Kty != "RSA" && kb.Key.Kty != "RSA-HSM" {
		return refuse("%s version %s has kty %q; want an RSA key of 3072 or 4096 bits", k.wrapName, v, kb.Key.Kty)
	}
	n, err := b64(kb.Key.N)
	if err != nil {
		return refuse("%s version %s has no readable modulus", k.wrapName, v)
	}
	bits := new(big.Int).SetBytes(n).BitLen()
	if bits != 3072 && bits != 4096 {
		return refuse("%s version %s is a %d-bit RSA key; want 3072 or 4096 bits", k.wrapName, v, bits)
	}
	// Microsoft: for RSA, WRAPKEY/UNWRAPKEY are ENCRYPT/DECRYPT, so decrypt on
	// this key would open data keys too.
	if !sameOps(kb.Key.KeyOps, "wrapKey", "unwrapKey") {
		return refuse("%s version %s has key_ops %v; want exactly wrapKey and unwrapKey", k.wrapName, v, kb.Key.KeyOps)
	}
	return v, bits / 8, nil
}

// latestSigning reads the signing key's latest version and returns it and its
// public key, which it caches.
func (k *KEK) latestSigning(ctx context.Context) (string, *ecdsa.PublicKey, error) {
	kb, v, err := k.getKey(ctx, k.signName, "")
	if err != nil {
		return "", nil, err
	}
	err = k.usable(k.signName, v, kb)
	if err == nil && !sameOps(kb.Key.KeyOps, "sign", "verify") {
		err = fmt.Errorf("%s version %s has key_ops %v; want exactly sign and verify", k.signName, v, kb.Key.KeyOps)
	}
	var pub *ecdsa.PublicKey
	if err == nil {
		pub, err = k.p256(v, kb)
	}
	if err != nil {
		return "", nil, fmt.Errorf("azurekv KEK %s: refusing the signing key: %w", k.id, err)
	}
	k.mu.Lock()
	k.pubs[v] = pub
	k.mu.Unlock()
	return v, pub, nil
}

// p256 is the version's public key, refused unless an EC P-256 key.
func (k *KEK) p256(v string, kb keyBundle) (*ecdsa.PublicKey, error) {
	if kb.Key.Kty != "EC" && kb.Key.Kty != "EC-HSM" {
		return nil, fmt.Errorf("%s version %s has kty %q; want an EC P-256 key", k.signName, v, kb.Key.Kty)
	}
	if kb.Key.Crv != "P-256" {
		return nil, fmt.Errorf("%s version %s is on curve %q; want P-256", k.signName, v, kb.Key.Crv)
	}
	x, errX := b64(kb.Key.X)
	y, errY := b64(kb.Key.Y)
	if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
		return nil, fmt.Errorf("%s version %s has no readable P-256 point", k.signName, v)
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil {
		return nil, fmt.Errorf("%s version %s: %w", k.signName, v, err)
	}
	return pub, nil
}

// signingVersion is the public key of signing-key version sv, from the cache
// or one GET. A version Key Vault refuses, or that is disabled or not P-256,
// is a definitive refusal remembered for missTTL; an unreachable vault is
// transient and not remembered.
func (k *KEK) signingVersion(ctx context.Context, sv string) (*ecdsa.PublicKey, error) {
	now := k.now()
	k.mu.Lock()
	pub, m := k.pubs[sv], k.misses[sv]
	k.mu.Unlock()
	if pub != nil {
		return pub, nil
	}
	if now.Before(m.until) {
		return nil, m.err
	}
	kb, _, err := k.getKey(ctx, k.signName, sv)
	if err == nil && !kb.Attributes.Enabled {
		err = fmt.Errorf("%w: %s version %s is disabled", kek.ErrKeyMissing, k.signName, sv)
	}
	if err == nil {
		pub, err = k.p256(sv, kb)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case errors.Is(err, secretstore.ErrUnavailable):
		return nil, err
	case err != nil:
		err = fmt.Errorf("azurekv KEK %s: the wrap's signing key: %w", k.id, err)
		if len(k.misses) >= maxMisses {
			maps.DeleteFunc(k.misses, func(_ string, m miss) bool { return !now.Before(m.until) })
		}
		k.misses[sv] = miss{until: now.Add(missTTL), err: err}
		return nil, err
	}
	k.pubs[sv] = pub
	return pub, nil
}

// crypt calls name/version/op with alg on in and returns Key Vault's answer,
// after checking its kid names that very version.
func (k *KEK) crypt(ctx context.Context, name, version, op, alg string, in []byte) ([]byte, error) {
	var out struct {
		Kid   string `json:"kid"`
		Value string `json:"value"`
	}
	path := "keys/" + name + "/" + version + "/" + op
	if err := k.call(ctx, http.MethodPost, path, map[string]string{"alg": alg, "value": base64.RawURLEncoding.EncodeToString(in)}, &out); err != nil {
		return nil, err
	}
	if v, err := k.kidVersion(out.Kid, name); err != nil || v != version {
		return nil, k.serviceErr(fmt.Errorf("%s answered for %q, not %s version %s", op, out.Kid, name, version))
	}
	b, err := b64(out.Value)
	if err != nil {
		return nil, k.serviceErr(fmt.Errorf("%s answered a value that is not base64", op))
	}
	return b, nil
}

// call is client.call classified: an unreachable vault stays transient
// (secretstore.ErrUnavailable); any answer Key Vault gives with an error
// status, and a 404, is kek.ErrService — never not-found, so a boot key under
// a deleted key fails boot rather than being minted over.
func (k *KEK) call(ctx context.Context, method, path string, in, out any) error {
	status, err := k.c.call(ctx, method, path, in, out)
	switch {
	case errors.Is(err, secretstore.ErrUnavailable):
		return fmt.Errorf("azurekv KEK %s: %w", k.id, err)
	case err != nil:
		return k.serviceErr(err)
	case status == http.StatusNotFound:
		return fmt.Errorf("azurekv KEK %s: %w: %w: key vault %s %s: 404 (no such key or version)", k.id, kek.ErrService, kek.ErrKeyMissing, method, path)
	}
	return nil
}

func (k *KEK) serviceErr(err error) error {
	return fmt.Errorf("azurekv KEK %s: %w: %w", k.id, kek.ErrService, err)
}

// b64 decodes Key Vault's base64url, padded or not, and tolerates the
// standard alphabet its own samples use.
func b64(s string) ([]byte, error) {
	s = strings.NewReplacer("+", "-", "/", "_").Replace(strings.TrimRight(s, "="))
	return base64.RawURLEncoding.DecodeString(s)
}
