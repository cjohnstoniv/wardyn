// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// Transit is the Vault Transit KEK (credential-storage design §2.3,
// WARDYN_KEK=transit): each row's data key is wrapped by Vault's encrypt and
// unwrapped by its decrypt, over the same client, auth and TLS as the KV
// store. The key never leaves Vault; every unwrap is a line in its audit log.
//
// The wrap is bound to its row by associated_data, AAD_kek exactly as the
// local KEK binds it (kek.WrapAAD), so a data key moved to another row does
// not unwrap. Only encrypt/ and decrypt/ are called: never rewrap/ (Vault does
// not document associated_data on it; `wardynd -rewrap` rewraps client-side),
// never keys/, so the policy grants update on those two paths and nothing
// else.
type Transit struct {
	c          *client
	mount, key string
	id         string
}

var _ kek.Versioned = (*Transit)(nil)

// probeName is the row the boot self-test binds its probe wraps to. Nothing
// is stored under it.
const probeName = "wardyn-kek-probe"

// NewTransit logs in and proves the key fit before any row depends on it: a
// probe data key must round-trip, and must NOT unwrap under another row's
// associated data. A key that ignored associated_data would let a database
// writer move a wrapped data key between rows, so boot refuses it (fail
// closed, K6). The token is kept alive until ctx ends.
func NewTransit(ctx context.Context, cfg Config, mount, key string) (*Transit, error) {
	if err := validSegments(mount); err != nil {
		return nil, fmt.Errorf("WARDYN_VAULT_TRANSIT_MOUNT: %w", err)
	}
	if err := validSegments(key); err != nil || strings.Contains(key, "/") {
		return nil, fmt.Errorf("WARDYN_VAULT_TRANSIT_KEY %q must be one path segment of letters, digits, \".\", \"_\" and \"-\"", key)
	}
	if cfg.Auth == AuthKubernetes && cfg.Role == "" {
		return nil, fmt.Errorf("WARDYN_VAULT_ROLE is required with WARDYN_VAULT_AUTH=%s", AuthKubernetes)
	}
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	if err := c.login(ctx); err != nil {
		return nil, fmt.Errorf("vault at %s: %w", c.base.Host, err)
	}
	t := &Transit{c: c, mount: mount, key: key, id: "transit:" + mount + "/" + key}
	if err := t.selfTest(ctx); err != nil {
		return nil, fmt.Errorf("vault transit key %s at %s: %w", t.id, c.base.Host, err)
	}
	go c.keepAlive(ctx)
	return t, nil
}

func (t *Transit) selfTest(ctx context.Context) error {
	dek := make([]byte, kek.DEKSize)
	if _, err := rand.Read(dek); err != nil {
		return err
	}
	bind := kek.Bind("", probeName)
	w, err := t.Wrap(ctx, dek, bind)
	if err != nil {
		return err
	}
	got, err := t.Unwrap(ctx, w, bind)
	if err != nil {
		return fmt.Errorf("a probe data key did not unwrap: %w", err)
	}
	if !bytes.Equal(got, dek) {
		return errors.New("a probe data key unwrapped to different bytes")
	}
	_, err = t.Unwrap(ctx, w, kek.Bind("", probeName+"-moved"))
	switch {
	case err == nil:
		return errors.New("refusing it: a probe data key unwrapped under associated data it was not wrapped under, so this key does not bind a wrap to its row — use a key of type aes256-gcm96")
	case errors.Is(err, secretstore.ErrUnavailable):
		return err
	}
	return nil
}

// ID implements kek.KEK: "transit:<mount>/<key>".
func (t *Transit) ID() string { return t.id }

// Describe names the key service for an operator ("Vault Transit at
// vault.example:8200").
func (t *Transit) Describe() string { return "Vault Transit at " + t.c.base.Host }

// Wrap implements kek.KEK.
func (t *Transit) Wrap(ctx context.Context, dek []byte, bind map[string]string) ([]byte, error) {
	if len(dek) != kek.DEKSize {
		return nil, fmt.Errorf("transit KEK: data key is %d bytes, want %d", len(dek), kek.DEKSize)
	}
	aad, err := kek.WrapAAD(bind, t.id)
	if err != nil {
		return nil, fmt.Errorf("transit KEK: %w", err)
	}
	var r struct {
		Data struct {
			Ciphertext string `json:"ciphertext"`
		} `json:"data"`
	}
	if err := t.post(ctx, "encrypt", map[string]string{
		"plaintext":       base64.StdEncoding.EncodeToString(dek),
		"associated_data": base64.StdEncoding.EncodeToString(aad),
	}, &r); err != nil {
		return nil, err
	}
	w := []byte(r.Data.Ciphertext)
	if _, err := t.WrapVersion(w); err != nil {
		return nil, fmt.Errorf("transit KEK %s: encrypt answered %w", t.id, err)
	}
	return w, nil
}

// Unwrap implements kek.KEK. A wrap moved to another row, made under another
// key, or under a version min_decryption_version has retired is a definitive
// refusal; only an unreachable Vault is transient.
func (t *Transit) Unwrap(ctx context.Context, wrapped []byte, bind map[string]string) ([]byte, error) {
	if _, err := t.WrapVersion(wrapped); err != nil {
		return nil, fmt.Errorf("transit KEK %s: the row holds %w", t.id, err)
	}
	aad, err := kek.WrapAAD(bind, t.id)
	if err != nil {
		return nil, fmt.Errorf("transit KEK: %w", err)
	}
	var r struct {
		Data struct {
			Plaintext string `json:"plaintext"`
		} `json:"data"`
	}
	if err := t.post(ctx, "decrypt", map[string]string{
		"ciphertext":      string(wrapped),
		"associated_data": base64.StdEncoding.EncodeToString(aad),
	}, &r); err != nil {
		return nil, err
	}
	dek, err := base64.StdEncoding.DecodeString(r.Data.Plaintext)
	if err != nil || len(dek) != kek.DEKSize {
		return nil, fmt.Errorf("transit KEK %s: decrypt did not answer a %d-byte data key", t.id, kek.DEKSize)
	}
	return dek, nil
}

// post calls <mount>/<op>/<key>. A 404 (no such mount or key) is definitive.
func (t *Transit) post(ctx context.Context, op string, in map[string]string, out any) error {
	path := t.mount + "/" + op + "/" + t.key
	status, err := t.c.call(ctx, http.MethodPost, path, in, out)
	if err != nil {
		return fmt.Errorf("transit KEK %s: %w", t.id, err)
	}
	if status == http.StatusNotFound {
		return fmt.Errorf("transit KEK %s: vault POST %s: 404 (no such mount or key)", t.id, path)
	}
	return nil
}

// WrapVersion implements kek.Versioned: the N of a "vault:vN:…" ciphertext.
func (t *Transit) WrapVersion(wrapped []byte) (int, error) {
	rest, ok := strings.CutPrefix(string(wrapped), "vault:v")
	if ok {
		num, _, found := strings.Cut(rest, ":")
		if n, err := strconv.Atoi(num); found && err == nil && n > 0 {
			return n, nil
		}
	}
	return 0, errors.New("no Transit ciphertext (want vault:v<N>:…)")
}

// LatestVersion implements kek.Versioned. It wraps a probe data key, which
// Vault always does under the latest version: no read on keys/ is needed.
func (t *Transit) LatestVersion(ctx context.Context) (int, error) {
	dek := make([]byte, kek.DEKSize)
	if _, err := rand.Read(dek); err != nil {
		return 0, err
	}
	w, err := t.Wrap(ctx, dek, kek.Bind("", probeName))
	if err != nil {
		return 0, err
	}
	return t.WrapVersion(w)
}
