// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package vaultkv is Wardyn's Vault client and what it serves: the KV v2
// external store (credential-storage design §2.3a.1), and the Transit KEK
// (transit.go, §2.3). In store mode each credential's value lives in the
// organisation's Vault (OpenBao is a supported, API-compatible endpoint), and
// the Postgres row is a pointer to it. Wardyn does no at-rest cryptography
// for such a row.
//
// Path scheme, under the mount and a per-install prefix:
//
//	<prefix>/platform/<name>              owner "" and a boot key (secretstore.PlatformNames)
//	<prefix>/operator/<name>              owner "" (operator-namespace credentials)
//	<prefix>/people/<owner-b32>/<name>    every other owner
//
// owner-b32 is base32hex (RFC 4648 §7), lowercase, unpadded: one path segment
// that can never hold a "/" and that an operator (and -reconcile) can reverse.
//
// The binding that replaces local mode's associated data is a pair: the path
// is DERIVED from the row's (owned_by, name) and a row naming any other path
// is refused; then the value's custom_metadata must name the same owner and
// name. A pointer moved under another person's row therefore derives a path
// that holds nothing, or a value bound to someone else: a refusal either way.
//
// Wardyn only ever calls data/ and metadata/ (and auth/): never destroy/,
// undelete/ or a DELETE on data/, so the policy grants none of them. "Remove"
// is DELETE metadata/, which drops every version.
package vaultkv

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// Name is the registered store name and the kek_id prefix.
const Name = "vaultkv"

// The custom_metadata keys that bind a value to its row.
const (
	metaOwner  = "wardyn-owner"
	metaName   = "wardyn-name"
	metaKind   = "wardyn-kind"
	metaFormat = "wardyn-format"
)

var b32 = base32.HexEncoding.WithPadding(base32.NoPadding)

// Config is the store's configuration (docs/ENV.md, WARDYN_VAULT_*).
type Config struct {
	Addr, Namespace string
	// Auth is AuthKubernetes (Role, AuthMount, K8sTokenFile) or AuthTokenFile
	// (TokenFile). There is no token-in-env option, by design.
	Auth, AuthMount, Role, K8sTokenFile, TokenFile string
	// CACertFile is added to the system roots for this client only.
	CACertFile string
	// Mount is the KV v2 mount; Prefix the per-install path prefix.
	Mount, Prefix string
	// MaxVersions is set on every secret this store creates (1: a replaced
	// value does not linger).
	MaxVersions int
	// Timeout bounds each HTTP call.
	Timeout time.Duration
}

// Store is the Vault KV v2 implementation of secretstore.External.
type Store struct {
	c           *client
	mount       string
	prefix      string
	maxVersions int
}

var _ secretstore.External = (*Store)(nil)

// New validates cfg and logs in. A store that cannot log in refuses to be
// built, so boot fails closed (K11). The token is kept alive until ctx ends.
func New(ctx context.Context, cfg Config) (*Store, error) {
	if err := validSegments(cfg.Mount); err != nil {
		return nil, fmt.Errorf("WARDYN_VAULT_KV_MOUNT: %w", err)
	}
	if err := validSegments(cfg.Prefix); err != nil {
		return nil, fmt.Errorf("WARDYN_VAULT_KV_PREFIX: %w", err)
	}
	if cfg.MaxVersions < 1 {
		return nil, fmt.Errorf("WARDYN_VAULT_KV_MAX_VERSIONS must be at least 1")
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
	go c.keepAlive(ctx)
	return &Store{c: c, mount: cfg.Mount, prefix: cfg.Prefix, maxVersions: cfg.MaxVersions}, nil
}

// Name implements secretstore.External.
func (s *Store) Name() string { return Name }

// Describe implements secretstore.External.
func (s *Store) Describe() string { return "Vault at " + s.c.base.Host }

// validSegments accepts a "/"-separated path whose every segment is non-empty,
// not "." or "..", and made of [A-Za-z0-9._-]. Anything else could change
// which path Vault's router resolves.
func validSegments(p string) error {
	if p == "" {
		return fmt.Errorf("is empty")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%q has an empty, \".\" or \"..\" segment", p)
		}
		for _, r := range seg {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
				return fmt.Errorf("%q holds %q; only letters, digits, \".\", \"_\" and \"-\" are allowed", p, r)
			}
		}
	}
	return nil
}

func kind(owner, name string) string { return secretstore.Kind(owner, name) }

// rel is the path under the mount DERIVED from the row. It is the only way
// this store ever computes where a value lives.
func (s *Store) rel(owner, name string) (string, error) {
	if strings.Contains(name, "/") {
		return "", fmt.Errorf("secret name %q cannot be a Vault path: it holds a \"/\"", name)
	}
	if err := validSegments(name); err != nil {
		return "", fmt.Errorf("secret name cannot be a Vault path: %w", err)
	}
	k := kind(owner, name)
	if k == "people" {
		return s.prefix + "/people/" + strings.ToLower(b32.EncodeToString([]byte(owner))) + "/" + name, nil
	}
	return s.prefix + "/" + k + "/" + name, nil
}

// ref is what the row records after "vaultkv:": the mount and the derived path.
func (s *Store) ref(rel string) string { return s.mount + "/" + rel }

// derive returns the derived path for the row and refuses a recorded ref
// that names any other (design rule 16): a pointer moved or forged by a
// database writer never selects where a read goes.
func (s *Store) derive(owner, name, ref string) (string, error) {
	rel, err := s.rel(owner, name)
	if err != nil {
		return "", err
	}
	if ref != s.ref(rel) {
		return "", fmt.Errorf("refused: the row points to %q, but its owner and name derive %q (a moved or forged pointer)", ref, s.ref(rel))
	}
	return rel, nil
}

// binding is the custom_metadata a value for (owner, name) carries. Vault
// refuses an empty metadata value, so the operator's "" owner is written as no
// wardyn-owner key at all; the kind (platform/operator vs people) says which.
func binding(owner, name string) map[string]string {
	m := map[string]string{metaName: name, metaKind: kind(owner, name), metaFormat: "v2"}
	if owner != "" {
		m[metaOwner] = owner
	}
	return m
}

// bound checks the value's own custom_metadata against the row, key by key
// (other keys the organisation adds are left alone). A wardyn-format other
// than v2 is a value this wardynd does not know how to read.
func bound(meta map[string]string, owner, name string) error {
	want := binding(owner, name)
	for _, k := range []string{metaOwner, metaName, metaKind, metaFormat} {
		if meta[k] != want[k] {
			return fmt.Errorf("refused: the value's metadata %s is %q, but this row needs %q", k, meta[k], want[k])
		}
	}
	return nil
}

type kvMeta struct {
	CurrentVersion int               `json:"current_version"`
	MaxVersions    int               `json:"max_versions"`
	CustomMetadata map[string]string `json:"custom_metadata"`
	Versions       map[string]struct {
		DeletionTime string `json:"deletion_time"`
		Destroyed    bool   `json:"destroyed"`
	} `json:"versions"`
}

// live reports whether the metadata's current version holds a value.
func (m kvMeta) live() bool {
	v, ok := m.Versions[fmt.Sprint(m.CurrentVersion)]
	return m.CurrentVersion > 0 && ok && v.DeletionTime == "" && !v.Destroyed
}

// metadata reads rel's metadata; found is false on a 404.
func (s *Store) metadata(ctx context.Context, rel string) (m kvMeta, found bool, err error) {
	var r struct {
		Data kvMeta `json:"data"`
	}
	status, err := s.c.call(ctx, http.MethodGet, s.mount+"/metadata/"+rel, nil, &r)
	if err != nil || status == http.StatusNotFound {
		return kvMeta{}, false, err
	}
	return r.Data, true, nil
}

// Put implements secretstore.External. It writes the binding metadata first
// (so a value is never readable without it), then the value with a
// check-and-set on the version it read: a concurrent writer fails the write
// instead of being overwritten silently. createOnly refuses when a value is
// already there.
func (s *Store) Put(ctx context.Context, owner, name, _ string, value []byte, createOnly bool) (string, error) {
	rel, err := s.rel(owner, name)
	if err != nil {
		return "", err
	}
	m, found, err := s.metadata(ctx, rel)
	if err != nil {
		return "", err
	}
	if found && len(m.CustomMetadata) > 0 {
		if err := bound(m.CustomMetadata, owner, name); err != nil {
			return "", fmt.Errorf("vault path %s: %w", s.ref(rel), err)
		}
	}
	if createOnly && m.live() {
		return "", fmt.Errorf("vault already holds a value at %s that this row does not point to: a Put landing concurrently (re-run), or an orphan (`wardynd -reconcile` lists it; remove it at Vault, then re-run)", s.ref(rel))
	}
	want := binding(owner, name)
	if !found || m.MaxVersions != s.maxVersions || !sameMap(m.CustomMetadata, want) {
		body := map[string]any{"max_versions": s.maxVersions, "custom_metadata": want}
		if _, err := s.c.call(ctx, http.MethodPost, s.mount+"/metadata/"+rel, body, nil); err != nil {
			return "", err
		}
	}
	body := map[string]any{
		"data":    map[string]string{"value": base64.StdEncoding.EncodeToString(value)},
		"options": map[string]int{"cas": m.CurrentVersion},
	}
	if _, err := s.c.call(ctx, http.MethodPost, s.mount+"/data/"+rel, body, nil); err != nil {
		return "", err
	}
	return s.ref(rel), nil
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}

// Get implements secretstore.External.
func (s *Store) Get(ctx context.Context, owner, name, ref string) ([]byte, error) {
	rel, err := s.derive(owner, name, ref)
	if err != nil {
		return nil, err
	}
	var r struct {
		Data struct {
			Data     map[string]string `json:"data"`
			Metadata struct {
				CustomMetadata map[string]string `json:"custom_metadata"`
			} `json:"metadata"`
		} `json:"data"`
	}
	status, err := s.c.call(ctx, http.MethodGet, s.mount+"/data/"+rel, nil, &r)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound || r.Data.Data == nil {
		return nil, fmt.Errorf("refused: Vault no longer holds this credential at %s", ref)
	}
	if err := bound(r.Data.Metadata.CustomMetadata, owner, name); err != nil {
		return nil, err
	}
	// A data map with no "value" key is refused like a value that is not
	// base64: read as zero bytes, it would let a boot key be minted over.
	raw, ok := r.Data.Data["value"]
	v, err := base64.StdEncoding.DecodeString(raw)
	if !ok || err != nil {
		return nil, fmt.Errorf("refused: the value at %s is not in Wardyn's format", ref)
	}
	return v, nil
}

// Check implements secretstore.External from metadata alone.
func (s *Store) Check(ctx context.Context, owner, name, ref string) error {
	rel, err := s.derive(owner, name, ref)
	if err != nil {
		return err
	}
	m, found, err := s.metadata(ctx, rel)
	if err != nil {
		return err
	}
	if !found || !m.live() {
		return fmt.Errorf("Vault no longer holds this credential at %s", ref)
	}
	return bound(m.CustomMetadata, owner, name)
}

// Delete implements secretstore.External: every version and the metadata go.
// The path is always DERIVED from (owner, name), never taken from the row, so
// a forged pointer can never make Wardyn delete someone else's value.
func (s *Store) Delete(ctx context.Context, owner, name, _ string) error {
	rel, err := s.rel(owner, name)
	if err != nil {
		return err
	}
	_, err = s.c.call(ctx, http.MethodDelete, s.mount+"/metadata/"+rel, nil, nil)
	return err
}

// Walk implements secretstore.External by listing the three kinds under the
// prefix. A people/ segment that does not decode is still reported, with the
// raw path as its name.
func (s *Store) Walk(ctx context.Context) ([]secretstore.ExternalEntry, error) {
	var out []secretstore.ExternalEntry
	for _, k := range []string{"platform", "operator", "people"} {
		keys, err := s.list(ctx, s.prefix+"/"+k+"/")
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			out = append(out, s.entry(k, key))
		}
	}
	return out, nil
}

func (s *Store) entry(kind, key string) secretstore.ExternalEntry {
	rel := s.prefix + "/" + kind + "/" + key
	if kind != "people" {
		return secretstore.ExternalEntry{Name: key, Ref: s.ref(rel)}
	}
	seg, name, _ := strings.Cut(key, "/")
	owner, err := b32.DecodeString(strings.ToUpper(seg))
	if err != nil {
		return secretstore.ExternalEntry{Owner: "(undecodable " + seg + ")", Name: name, Ref: s.ref(rel)}
	}
	return secretstore.ExternalEntry{Owner: string(owner), Name: name, Ref: s.ref(rel)}
}

// list returns every leaf key under dir (which ends in "/"), relative to it,
// descending into sub-folders.
func (s *Store) list(ctx context.Context, dir string) ([]string, error) {
	var r struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	status, err := s.c.call(ctx, http.MethodGet, s.mount+"/metadata/"+dir+"?list=true", nil, &r)
	if err != nil || status == http.StatusNotFound {
		return nil, err
	}
	var out []string
	for _, k := range r.Data.Keys {
		if !strings.HasSuffix(k, "/") {
			out = append(out, k)
			continue
		}
		sub, err := s.list(ctx, dir+k)
		if err != nil {
			return nil, err
		}
		for _, leaf := range sub {
			out = append(out, k+leaf)
		}
	}
	return out, nil
}
