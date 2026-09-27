// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package azurekv is the Azure Key Vault Secrets external store
// (credential-storage design §2.3a.3): in store mode each credential's value
// lives in the organisation's Key Vault, and the Postgres row is a pointer to
// it. Wardyn does no at-rest cryptography for such a row, and needs no Azure
// SDK: the Entra token exchange and the Key Vault REST calls are plain HTTP.
//
// Key Vault names hold only [0-9a-zA-Z-], so a secret's name is a hash of its
// row, one name per (owner, name), and a replace is a new VERSION of it:
//
//	stem = <prefix>-<kind>-<hex(SHA-256(enc(owner, name)))[:32]>   derived from the row
//	name = <stem>-g<gen>        gen: base36 of the unix second the generation began
//	ref  = <vault-host>/<name>#<n>     n: versions the name held after the write
//
// Key Vault cannot delete a previous version, so every Put disables the
// versions before it, and once the name holds WARDYN_AZURE_KV_MAX_VERSIONS
// versions (the vault's own count; the row's n is only a record) the next Put
// starts a new generation (a fresh name) and the old one is deleted. The
// binding that replaces local mode's associated data is the pair vaultkv uses:
// the stem is DERIVED from the row and a row naming any other is refused; then
// the value's tags must name the same owner and name.
package azurekv

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// Name is the registered store name and the kek_id prefix.
const Name = "azurekv"

// Purge modes (WARDYN_AZURE_KV_PURGE).
const (
	PurgeAuto  = "auto"
	PurgeNever = "never"
)

// maxValue is the largest value Put accepts: 18 KiB of bytes is 24 KiB of
// base64, under Key Vault's 25 KB value limit.
const maxValue = 18 * 1024

// contentType marks a value Wardyn wrote: the base64 of the bytes.
const contentType = "application/octet-stream;base64"

// The tags that bind a value to its row.
const (
	tagOwner  = "wardyn-owner"
	tagName   = "wardyn-name"
	tagKind   = "wardyn-kind"
	tagFormat = "wardyn-format"
)

// Config is the store's configuration (docs/ENV.md, WARDYN_AZURE_*).
type Config struct {
	// VaultURL is the vault's base URL, https://<vault>.vault.azure.net.
	VaultURL string
	// Auth is AuthWorkloadIdentity (TenantID, ClientID, FederatedTokenFile,
	// AuthorityHost) or AuthManagedIdentity (ClientID optional: a
	// user-assigned identity).
	Auth, TenantID, ClientID, FederatedTokenFile, AuthorityHost string
	// CACertFile is added to the system roots for this client only.
	CACertFile string
	// Prefix starts every secret name this install writes.
	Prefix string
	// MaxVersions is how many versions a generation holds before the next Put
	// starts a new one.
	MaxVersions int
	// Purge is PurgeAuto (purge after a delete, when the vault allows it) or
	// PurgeNever.
	Purge string
	// Timeout bounds each HTTP call.
	Timeout time.Duration
}

// Store is the Key Vault implementation of secretstore.External.
type Store struct {
	c           *client
	prefix      string
	ours        *regexp.Regexp // a secret name this install derives
	maxVersions int
	purge       bool
	now         func() time.Time
	// pollEvery spaces the checks for a soft delete to finish before a purge.
	pollEvery time.Duration
}

var _ secretstore.External = (*Store)(nil)

var prefixRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// New validates cfg and fetches a first token. A store that cannot get one
// refuses to be built, so boot fails closed (K11).
func New(ctx context.Context, cfg Config) (*Store, error) {
	s, err := build(cfg)
	if err != nil {
		return nil, err
	}
	if _, err := s.c.accessToken(ctx); err != nil {
		return nil, fmt.Errorf("key vault %s: %w", s.c.host, err)
	}
	return s, nil
}

func build(cfg Config) (*Store, error) {
	prefix := strings.ToLower(cfg.Prefix)
	if !prefixRE.MatchString(prefix) {
		return nil, fmt.Errorf("WARDYN_AZURE_KV_PREFIX %q must be 1-63 letters, digits and inner \"-\"", cfg.Prefix)
	}
	if cfg.MaxVersions < 1 || cfg.MaxVersions > 500 {
		return nil, fmt.Errorf("WARDYN_AZURE_KV_MAX_VERSIONS must be 1-500 (Key Vault cannot back up a secret with more than 500 versions)")
	}
	if cfg.Purge != PurgeAuto && cfg.Purge != PurgeNever {
		return nil, fmt.Errorf("WARDYN_AZURE_KV_PURGE %q is not %q or %q", cfg.Purge, PurgeAuto, PurgeNever)
	}
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	ours := regexp.MustCompile(`^` + regexp.QuoteMeta(prefix) + `-(platform|operator|people)-[0-9a-f]{32}-g[0-9a-z]+$`)
	return &Store{c: c, prefix: prefix, ours: ours, maxVersions: cfg.MaxVersions, purge: cfg.Purge == PurgeAuto, now: time.Now, pollEvery: time.Second}, nil
}

// Name implements secretstore.External.
func (s *Store) Name() string { return Name }

// Describe implements secretstore.External: "Key Vault <vault>".
func (s *Store) Describe() string {
	vault, _, _ := strings.Cut(s.c.base.Hostname(), ".")
	return "Key Vault " + vault
}

// enc is the injective field encoding the envelope uses: for each field, its
// uint32 big-endian length, then its bytes.
func enc(fields ...string) []byte {
	var b []byte
	for _, f := range fields {
		b = binary.BigEndian.AppendUint32(b, uint32(len(f)))
		b = append(b, f...)
	}
	return b
}

// stem is the name stem DERIVED from the row. It is the only way this store
// ever computes where a value lives.
func (s *Store) stem(owner, name string) string {
	h := sha256.Sum256(enc(owner, name))
	return s.prefix + "-" + secretstore.Kind(owner, name) + "-" + hex.EncodeToString(h[:16])
}

func (s *Store) secretName(owner, name string, gen int64) string {
	return s.stem(owner, name) + "-g" + strconv.FormatInt(gen, 36)
}

func (s *Store) ref(secretName string, n int) string {
	return s.c.host + "/" + secretName + "#" + strconv.Itoa(n)
}

// parse checks a recorded ref against the row and returns the secret name,
// generation and version count it records (design rule 16): the vault must be
// this one and the stem the one the row derives, so a pointer moved or forged
// by a database writer never selects what is read or deleted.
func (s *Store) parse(owner, name, ref string) (secretName string, gen int64, n int, err error) {
	refuse := func(why string) (string, int64, int, error) {
		return "", 0, 0, fmt.Errorf("refused: the row points to %q, which %s (a moved or forged pointer)", ref, why)
	}
	host, rest, ok := strings.Cut(ref, "/")
	if !ok || host != s.c.host {
		return refuse("is not in this vault (" + s.c.host + ")")
	}
	secretName, count, ok := strings.Cut(rest, "#")
	genStr, found := strings.CutPrefix(secretName, s.stem(owner, name)+"-g")
	if !ok || !found {
		return refuse("is not the name its owner and name derive")
	}
	gen, gerr := strconv.ParseInt(genStr, 36, 64)
	n, nerr := strconv.Atoi(count)
	if gerr != nil || gen <= 0 || strconv.FormatInt(gen, 36) != genStr || nerr != nil || n < 1 || strconv.Itoa(n) != count {
		return refuse("is not in Wardyn's format")
	}
	return secretName, gen, n, nil
}

// Ref implements secretstore.External: the secret the row's value lives in.
// The stem is DERIVED from (owner, name); only the generation comes from the
// recorded ref, and a ref naming any other stem is refused, so a forged
// pointer claims nothing.
func (s *Store) Ref(owner, name, ref string) (string, error) {
	secretName, _, _, err := s.parse(owner, name, ref)
	if err != nil {
		return "", err
	}
	return s.c.host + "/" + secretName, nil
}

// binding is the tag set a value for (owner, name) carries. The operator's
// "" owner is written as no wardyn-owner tag; the kind says which.
func binding(owner, name string) map[string]string {
	m := map[string]string{tagName: name, tagKind: secretstore.Kind(owner, name), tagFormat: "v2"}
	if owner != "" {
		m[tagOwner] = owner
	}
	return m
}

// bound checks the value's own tags against the row, tag by tag (other tags
// the organisation adds are left alone).
func bound(tags map[string]string, owner, name string) error {
	want := binding(owner, name)
	for _, k := range []string{tagOwner, tagName, tagKind, tagFormat} {
		if tags[k] != want[k] {
			return fmt.Errorf("refused: the value's tag %s is %q, but this row needs %q", k, tags[k], want[k])
		}
	}
	return nil
}

type attributes struct {
	Enabled         *bool `json:"enabled,omitempty"`
	Created         int64 `json:"created,omitempty"`
	RecoverableDays int   `json:"recoverableDays,omitempty"`
}

type bundle struct {
	ID          string            `json:"id"`
	Value       string            `json:"value,omitempty"`
	ContentType string            `json:"contentType,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	Attributes  attributes        `json:"attributes"`
	// ScheduledPurgeDate (unix seconds) is when the vault purges a deleted
	// secret; only the deleted-secrets list carries it.
	ScheduledPurgeDate int64 `json:"scheduledPurgeDate,omitempty"`
}

func (b bundle) enabled() bool { return b.Attributes.Enabled == nil || *b.Attributes.Enabled }

// version is the last path segment of a secret id (…/secrets/<name>/<version>).
func (b bundle) version() string { return b.ID[strings.LastIndex(b.ID, "/")+1:] }

// newGen is a generation after old: the current unix second, or old+1.
func (s *Store) newGen(old int64) int64 {
	return max(s.now().Unix(), old+1)
}

// Put implements secretstore.External. It lists the row's current generation,
// writes a new version into it, then disables the versions that listing held.
// The listing comes BEFORE the write, so every version it holds is older than
// the new one: a Put never disables a version a concurrent Put wrote after it,
// which could leave a row with no enabled version (a definitive refusal). It
// also catches a version an earlier, failed Put left enabled. The vault's
// count decides the rollover: a new generation is taken when there is none,
// the name holds WARDYN_AZURE_KV_MAX_VERSIONS versions, or createOnly asks for
// a name nothing holds yet. A name held by a DELETED secret (a credential
// removed and re-added within the vault's retention) is purged and reused when
// allowed, else skipped for a new generation; Wardyn never recovers a deleted
// secret.
func (s *Store) Put(ctx context.Context, owner, name, prev string, value []byte, createOnly bool) (string, error) {
	if len(value) > maxValue {
		return "", fmt.Errorf("refused: the value is %d bytes; Key Vault holds at most %d", len(value), maxValue)
	}
	var gen int64
	var older []bundle // the versions the new one supersedes, listed before it
	if prev != "" {
		// A prev that is not this row's own is not reused, and not touched.
		if _, g, _, err := s.parse(owner, name, prev); err == nil {
			gen = g
			if !createOnly {
				if older, err = s.versions(ctx, s.secretName(owner, name, g)); err != nil {
					return "", err
				}
			}
		}
	}
	if gen == 0 || len(older) >= s.maxVersions || createOnly {
		gen, older = s.newGen(gen), nil
	}
	body := map[string]any{
		"value":       base64.StdEncoding.EncodeToString(value),
		"contentType": contentType,
		"tags":        binding(owner, name),
	}
	for attempt := 0; ; attempt++ {
		sn := s.secretName(owner, name, gen)
		if createOnly {
			if err := s.refuseExisting(ctx, sn); err != nil {
				return "", err
			}
		}
		_, err := s.c.call(ctx, http.MethodPut, "secrets/"+sn, body, nil)
		if statusOf(err) == http.StatusConflict && attempt < 3 {
			// Purge the deleted secret and reuse the name, or move on.
			if attempt > 0 || !s.purge || s.purgeDeleted(ctx, sn, true, putPolls) != nil {
				gen = s.newGen(gen)
			}
			older = nil
			continue
		}
		if err != nil {
			return "", err
		}
		if err := s.disable(ctx, sn, older); err != nil {
			// older is only ever non-empty in the row's own generation, so the
			// new value is already the one a Get of the row reads (rule 18).
			return "", fmt.Errorf("the new value is written to %s and served, but an earlier version is still enabled (the next save retries): %w: %w", sn, secretstore.ErrRowNotWritten, err)
		}
		return s.ref(sn, len(older)+1), nil
	}
}

// refuseExisting refuses a name that already holds a version: the migrator's
// guard against writing into a value it did not put there.
func (s *Store) refuseExisting(ctx context.Context, secretName string) error {
	var page listPage
	status, err := s.c.call(ctx, http.MethodGet, "secrets/"+secretName+"/versions?maxresults=1", nil, &page)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound && len(page.Value) > 0 {
		return fmt.Errorf("key vault already holds %s, which this row does not point to: a Put landing concurrently (re-run), or an orphan (`wardynd -reconcile` lists it)", secretName)
	}
	return nil
}

type listPage struct {
	Value    []bundle `json:"value"`
	NextLink string   `json:"nextLink"`
}

// versions lists every version of secretName (metadata only, never a value).
func (s *Store) versions(ctx context.Context, secretName string) ([]bundle, error) {
	var all []bundle
	next := "secrets/" + secretName + "/versions?maxresults=25"
	for next != "" {
		var page listPage
		status, err := s.c.call(ctx, http.MethodGet, next, nil, &page)
		if err != nil {
			return nil, err
		}
		if status == http.StatusNotFound {
			return all, nil
		}
		all = append(all, page.Value...)
		next = page.NextLink
	}
	return all, nil
}

// disable disables each version in older that is still enabled, so only the
// value a Get reads stays readable.
func (s *Store) disable(ctx context.Context, secretName string, older []bundle) error {
	off := false
	for _, v := range older {
		if !v.enabled() {
			continue
		}
		body := map[string]any{"attributes": attributes{Enabled: &off}}
		if _, err := s.c.call(ctx, http.MethodPatch, "secrets/"+secretName+"/"+v.version(), body, nil); err != nil {
			return err
		}
	}
	return nil
}

// Get implements secretstore.External: the latest version of the row's
// generation, which must be enabled, tagged for the row, and in Wardyn's
// format.
func (s *Store) Get(ctx context.Context, owner, name, ref string) ([]byte, error) {
	sn, _, _, err := s.parse(owner, name, ref)
	if err != nil {
		return nil, err
	}
	// A disabled latest version answers 403 SecretDisabled: definitive, like
	// every 403.
	var b bundle
	status, err := s.c.call(ctx, http.MethodGet, "secrets/"+sn+"/", nil, &b)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, fmt.Errorf("refused: Key Vault no longer holds this credential (%s)", sn)
	}
	if err := bound(b.Tags, owner, name); err != nil {
		return nil, err
	}
	v, err := base64.StdEncoding.DecodeString(b.Value)
	if b.ContentType != contentType || err != nil {
		return nil, fmt.Errorf("refused: the value of %s is not in Wardyn's format", sn)
	}
	return v, nil
}

// Check implements secretstore.External from the version list alone: the
// newest version must be enabled and tagged for the row.
func (s *Store) Check(ctx context.Context, owner, name, ref string) error {
	sn, _, _, err := s.parse(owner, name, ref)
	if err != nil {
		return err
	}
	all, err := s.versions(ctx, sn)
	if err != nil {
		return err
	}
	var latest *bundle
	for i := range all {
		if latest == nil || all[i].Attributes.Created > latest.Attributes.Created ||
			(all[i].Attributes.Created == latest.Attributes.Created && all[i].enabled()) {
			latest = &all[i]
		}
	}
	if latest == nil || !latest.enabled() {
		return fmt.Errorf("refused: Key Vault no longer holds this credential (%s)", sn)
	}
	return bound(latest.Tags, owner, name)
}

// Delete implements secretstore.External: a soft delete of the row's
// generation, then, with WARDYN_AZURE_KV_PURGE=auto, a purge. A vault that
// refuses the purge (purge protection, or a role without it) leaves the value
// soft-deleted and recoverable by the organisation; the delete still succeeds
// and says so (secretstore.ReportDelete). The name is always one the row
// derives, never one a forged pointer supplies.
func (s *Store) Delete(ctx context.Context, owner, name, ref string) error {
	sn, _, _, err := s.parse(owner, name, ref)
	if err != nil {
		return err
	}
	var deleted bundle
	status, err := s.c.call(ctx, http.MethodDelete, "secrets/"+sn, nil, &deleted)
	if err != nil {
		return err
	}
	rep := secretstore.DeleteReport{Store: Name, RecoverableDays: deleted.Attributes.RecoverableDays}
	if s.purge {
		if err := s.purgeDeleted(ctx, sn, status != http.StatusNotFound, deletePolls); err != nil {
			slog.Warn("azurekv: the credential is soft-deleted but was not purged; the organisation can recover it until the vault's retention ends",
				slog.String("secret", sn), slog.Int("recoverable_days", rep.RecoverableDays), slog.Any("err", err))
		} else {
			rep.Purged, rep.RecoverableDays = true, 0
		}
	}
	secretstore.ReportDelete(ctx, rep)
	return nil
}

// How many times a purge re-checks a soft delete still in progress. A Put
// holds the row's lock and a database connection while it waits, so it gives
// up sooner, and takes a new generation instead.
const (
	putPolls    = 3
	deletePolls = 10
)

// purgeDeleted purges the deleted secret secretName; nothing to purge is
// success. A soft delete finishes asynchronously: with pending (the caller has
// just deleted it, or the vault said a deleted secret holds the name) a 404
// or a 409 means "still being deleted", and it waits, up to polls re-checks.
func (s *Store) purgeDeleted(ctx context.Context, secretName string, pending bool, polls int) error {
	for i := 0; ; i++ {
		status, err := s.c.call(ctx, http.MethodDelete, "deletedsecrets/"+secretName, nil, nil)
		switch {
		case err == nil && (status != http.StatusNotFound || !pending):
			return nil
		case err != nil && statusOf(err) != http.StatusConflict:
			return err
		case i == polls:
			return fmt.Errorf("the vault was still deleting %s after %d checks", secretName, polls)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.pollEvery):
		}
	}
}

// Walk implements secretstore.External by listing the vault's secrets, then
// its deleted secrets (metadata only, never a value), and keeping this
// install's: the prefix, and Wardyn's tags. A deleted one is reported
// soft-deleted, with the days left before the vault purges it: a value whose
// purge failed or was withheld stays recoverable by the organisation, and
// -reconcile says so.
func (s *Store) Walk(ctx context.Context) ([]secretstore.ExternalEntry, error) {
	live, err := s.walk(ctx, "secrets?maxresults=25", false)
	if err != nil {
		return nil, err
	}
	deleted, err := s.walk(ctx, "deletedsecrets?maxresults=25", true)
	if err != nil {
		return nil, err
	}
	return append(live, deleted...), nil
}

func (s *Store) walk(ctx context.Context, next string, deleted bool) ([]secretstore.ExternalEntry, error) {
	var out []secretstore.ExternalEntry
	for next != "" {
		var page listPage
		status, err := s.c.call(ctx, http.MethodGet, next, nil, &page)
		if err != nil || status == http.StatusNotFound {
			return out, err
		}
		for _, b := range page.Value {
			sn := b.ID[strings.LastIndex(b.ID, "/")+1:]
			if !s.ours.MatchString(strings.ToLower(sn)) || b.Tags[tagFormat] != "v2" {
				continue
			}
			e := secretstore.ExternalEntry{Owner: b.Tags[tagOwner], Name: b.Tags[tagName], Ref: s.c.host + "/" + strings.ToLower(sn), SoftDeleted: deleted}
			if left := time.Unix(b.ScheduledPurgeDate, 0).Sub(s.now()); deleted && b.ScheduledPurgeDate > 0 && left > 0 {
				e.RecoverableDays = int((left + 24*time.Hour - 1) / (24 * time.Hour))
			}
			out = append(out, e)
		}
		next = page.NextLink
	}
	return out, nil
}
