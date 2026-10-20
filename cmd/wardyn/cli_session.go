// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
	"github.com/cjohnstoniv/wardyn/internal/filelock"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// This computer's CLI sign-in (design ATTACH 11.2): one item per organisation URL holding the CLI session's
// wdc_ secret, the Ed25519 device key's seed and the cached tokens. It lives in the keychain (go-keyring:
// macOS Keychain, Windows Credential Manager, the Linux Secret Service) or, on Linux with no keychain, in
// <UserConfigDir>/wardyn/cli-session.json, mode 0600 in a 0700 directory. The profile names which store
// holds each URL's item. The secret and the key never go into argv, the environment or a log line.

const (
	cliKeyringService = "wardyn-cli"
	cliSessionFile    = "cli-session.json"
	cliLockFile       = "cli.lock"
	// cliTokenMargin is how long before its end a cached token stops being used, so a request never
	// leaves with one that expires on the way.
	cliTokenMargin = time.Minute
)

// cliLockWait is how long a command waits for another one's refresh before it re-reads the item.
var cliLockWait = 10 * time.Second

// cliItem is the stored sign-in for one organisation URL.
type cliItem struct {
	SessionID        uuid.UUID `json:"session_id"`
	Secret           string    `json:"secret"`
	KeySeed          string    `json:"key_seed"` // base64url, ed25519.SeedSize bytes
	Machine          string    `json:"machine"`
	AccessToken      string    `json:"access_token"` // cli_access
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	SessionExpiresAt time.Time `json:"session_expires_at"`
	IdleExpiresAt    time.Time `json:"idle_expires_at"`
	AdminToken       string    `json:"admin_token,omitempty"` // cli_admin, from login --admin
	AdminExpiresAt   time.Time `json:"admin_expires_at,omitzero"`
	// Ended is why the server refused the last renewal; a refused item is never sent again.
	Ended string `json:"ended,omitempty"`
}

// bearer is the token a request uses as of now: the live cli_admin token, else the live cli_access one,
// else "" (the item needs a refresh).
func (it cliItem) bearer(now time.Time) (token, kind string) {
	switch {
	case it.AdminToken != "" && now.Add(cliTokenMargin).Before(it.AdminExpiresAt):
		return it.AdminToken, types.APITokenKindCLIAdmin
	case it.AccessToken != "" && now.Add(cliTokenMargin).Before(it.AccessExpiresAt):
		return it.AccessToken, types.APITokenKindCLIAccess
	}
	return "", ""
}

// endedWhy is why the item can no longer renew as of now, "" when it can: the server refused it, or on
// its own clock it is past its absolute or idle lifetime.
func (it cliItem) endedWhy(now time.Time) string {
	switch {
	case it.Ended != "":
		return it.Ended
	case !now.Before(it.SessionExpiresAt):
		return endReasonExpired
	case !now.Before(it.IdleExpiresAt):
		return endReasonIdle
	}
	return ""
}

func (it cliItem) key() (ed25519.PrivateKey, error) {
	seed, err := base64.RawURLEncoding.DecodeString(it.KeySeed)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("the stored device key is damaged: run `wardyn login` again")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// applyGrant takes a token-call or refresh grant: a new secret and cli_access token, or, for a step-up, the
// cli_admin token.
func (it *cliItem) applyGrant(g types.CLISessionGrant) {
	it.SessionID, it.SessionExpiresAt, it.IdleExpiresAt = g.SessionID, g.SessionExpiresAt, g.IdleExpiresAt
	if g.Kind == types.APITokenKindCLIAdmin {
		it.AdminToken, it.AdminExpiresAt = g.AccessToken, g.AccessExpiresAt
		return
	}
	it.Secret, it.AccessToken, it.AccessExpiresAt = g.Secret, g.AccessToken, g.AccessExpiresAt
}

// cliProof signs types.CLIProofMessage, as the server verifies it, with the device key at now.
func cliProof(key ed25519.PrivateKey, origin, route string, id uuid.UUID, secret string, now time.Time) types.CLIProof {
	at := now.Unix()
	sig := ed25519.Sign(key, types.CLIProofMessage(origin, route, id, secret, at))
	return types.CLIProof{Time: at, Signature: base64.RawURLEncoding.EncodeToString(sig)}
}

// sessionStore is where items live; kind is the store the server is told (types.CLIStore*).
type sessionStore interface {
	get(origin string) (cliItem, bool, error)
	put(origin string, it cliItem) error
	del(origin string) error
}

func storeFor(kind, dir string) sessionStore {
	if kind == types.CLIStoreFile {
		return fileStore{dir}
	}
	return keychainStore{}
}

// keychainReady reports whether this computer has a keychain go-keyring can reach (cli_session_linux.go).
// A var so a test can stand in for the session bus.
var keychainReady = keychainReachable

// chooseStore is the store a new sign-in for origin uses: the keychain when one answers, else the file.
func chooseStore(origin string) string {
	if keychainReady() != nil {
		return types.CLIStoreFile
	}
	if _, err := keyring.Get(cliKeyringService, origin); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return types.CLIStoreFile
	}
	return types.CLIStoreKeychain
}

type keychainStore struct{}

func (keychainStore) get(origin string) (cliItem, bool, error) {
	if err := keychainReady(); err != nil {
		return cliItem{}, false, err
	}
	raw, err := keyring.Get(cliKeyringService, origin)
	if errors.Is(err, keyring.ErrNotFound) {
		return cliItem{}, false, nil
	}
	if err != nil {
		return cliItem{}, false, fmt.Errorf("read the keychain: %w", err)
	}
	var it cliItem
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		return cliItem{}, false, errors.New("the keychain item for the CLI sign-in is damaged: run `wardyn login` again")
	}
	return it, true, nil
}

func (keychainStore) put(origin string, it cliItem) error {
	if err := keychainReady(); err != nil {
		return err
	}
	raw, err := json.Marshal(it)
	if err != nil {
		return err
	}
	if err := keyring.Set(cliKeyringService, origin, string(raw)); err != nil {
		return fmt.Errorf("write the keychain: %w", err)
	}
	return nil
}

func (keychainStore) del(origin string) error {
	if err := keychainReady(); err != nil {
		return err
	}
	if err := keyring.Delete(cliKeyringService, origin); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("delete from the keychain: %w", err)
	}
	return nil
}

// fileStore is the no-keychain store: one 0600 file in a 0700 directory, a map of origin to item.
type fileStore struct{ dir string }

func (f fileStore) path() string { return filepath.Join(f.dir, cliSessionFile) }

func (f fileStore) load() (map[string]cliItem, error) {
	items := map[string]cliItem{}
	raw, err := os.ReadFile(f.path())
	if errors.Is(err, fs.ErrNotExist) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%s is damaged: run `wardyn login` again", f.path())
	}
	return items, nil
}

func (f fileStore) get(origin string) (cliItem, bool, error) {
	items, err := f.load()
	it, ok := items[origin]
	return it, ok, err
}

func (f fileStore) put(origin string, it cliItem) error {
	items, err := f.load()
	if err != nil {
		items = map[string]cliItem{}
	}
	items[origin] = it
	return f.save(items)
}

func (f fileStore) del(origin string) error {
	items, err := f.load()
	if err != nil {
		return os.Remove(f.path())
	}
	if _, ok := items[origin]; !ok {
		return nil
	}
	delete(items, origin)
	if len(items) == 0 {
		return os.Remove(f.path())
	}
	return f.save(items)
}

// save writes items through a 0600 sibling renamed into place, after closing the directory to 0700.
func (f fileStore) save(items map[string]cliItem) error {
	if err := os.MkdirAll(f.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(f.dir, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(items)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(f.dir, "."+cliSessionFile+".*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(body)
	cerr := tmp.Chmod(0o600)
	if err := errors.Join(werr, cerr, tmp.Close()); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), f.path()); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return nil
}

// lockRefresh takes <dir>/cli.lock, waiting up to cliLockWait. Every write of a stored item happens under
// it (a refresh, login, the admin step-up, logout), as one read-modify-write against the current item, so
// two commands never refresh at once and look like a replay, and none writes back a rotated-past secret.
// ok is false when another holder kept it that long.
func lockRefresh(dir string) (unlock func(), ok bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, cliLockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	deadline := time.Now().Add(cliLockWait)
	for {
		err := filelock.TryLock(f)
		if err == nil {
			return func() { _ = f.Close() }, true, nil
		}
		if !errors.Is(err, filelock.ErrLocked) {
			_ = f.Close()
			return nil, false, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, false, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// storedSession is the stored sign-in for one organisation URL, as a command found it.
type storedSession struct {
	origin, kind string // kind "" = none stored
	item         cliItem
	token        string // the bearer to send, "" when none
	tokenKind    string // types.APITokenKindCLIAccess or types.APITokenKindCLIAdmin
}

// loadSession reads origin's stored sign-in and returns a live bearer for it, refreshing under the lock
// when the cached tokens are spent. No stored sign-in is a zero token and a nil error.
func loadSession(ctx context.Context, origin string) (storedSession, error) {
	s := storedSession{origin: origin}
	dir, err := cliprofile.Dir()
	if err != nil {
		return s, err
	}
	if s.kind, err = cliprofile.ReadSessionStore(dir, origin); err != nil || s.kind == "" {
		return s, err
	}
	st := storeFor(s.kind, dir)
	read := func() (bool, error) {
		it, ok, err := st.get(origin)
		s.item = it
		if ok {
			s.token, s.tokenKind = it.bearer(time.Now())
		}
		return ok, err
	}
	if ok, err := read(); err != nil || !ok {
		s.kind = ""
		return s, err
	}
	if s.token != "" {
		return s, nil
	}
	if why := s.item.endedWhy(time.Now()); why != "" {
		return s, fmt.Errorf(errSessionEnded, origin, why)
	}
	unlock, locked, err := lockRefresh(dir)
	if err != nil {
		return s, fmt.Errorf(errRenew, origin, err)
	}
	if !locked {
		// The holder had longer than any refresh takes: read what it stored.
		if _, err := read(); err != nil || s.token != "" {
			return s, err
		}
		return s, fmt.Errorf(errRenewBusy, origin)
	}
	defer unlock()
	// Another command may have refreshed while this one waited: its secret is the current one now.
	if _, err := read(); err != nil || s.token != "" {
		return s, err
	}
	if why := s.item.endedWhy(time.Now()); why != "" {
		return s, fmt.Errorf(errSessionEnded, origin, why)
	}
	err = s.refresh(ctx, st)
	return s, err
}

// refresh renews s's session at the server and stores the rotated secret before anything else reads it.
// The caller holds the lock. A refusal that ends the session is recorded in the item, so it is not
// presented again.
func (s *storedSession) refresh(ctx context.Context, st sessionStore) error {
	key, err := s.item.key()
	if err != nil {
		return err
	}
	route := "/api/v1/cli/sessions/" + s.item.SessionID.String() + "/refresh"
	req := types.CLISessionRefreshRequest{Secret: s.item.Secret,
		Proof: cliProof(key, s.origin, route, s.item.SessionID, s.item.Secret, time.Now())}
	var g types.CLISessionGrant
	err = cliCall(ctx, s.origin, http.MethodPost, route, "", req, &g)
	var ae *sdk.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized && ae.Reason == "cli_session_invalid" {
		// The secret, the key and the tokens are of no further use: only the session id and why stay.
		s.item.Ended = fmt.Sprintf(endReasonServer, ae.Reason)
		s.item.Secret, s.item.KeySeed, s.item.AccessToken, s.item.AdminToken = "", "", "", ""
		if perr := st.put(s.origin, s.item); perr != nil {
			return perr
		}
		return fmt.Errorf(errSessionEnded, s.origin, s.item.Ended)
	}
	if err != nil {
		return fmt.Errorf(errRenew, s.origin, err)
	}
	s.item.applyGrant(g)
	if err := st.put(s.origin, s.item); err != nil {
		return err
	}
	s.token, s.tokenKind = g.AccessToken, types.APITokenKindCLIAccess
	return nil
}

// cliCall sends one JSON request to origin+route through orgClient, with bearer when it is not "". A
// non-2xx answer is an *sdk.APIError carrying the server's reason.
func cliCall(ctx context.Context, origin, method, route, bearer string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, origin+route, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := orgClient(origin, 30*time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return sdk.NewAPIError(resp.StatusCode, raw)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
