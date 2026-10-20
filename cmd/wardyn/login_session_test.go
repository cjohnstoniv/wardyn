// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zalando/go-keyring"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// signInOrg is an organisation with CLI sign-in on, speaking the wire internal/api/cli_sign_in.go and
// cli_sessions.go ship: it checks the PKCE verifier, the device proof against its own URL, single-use codes,
// secret rotation and reuse, and the per-device sign-out.
type signInOrg struct {
	t       *testing.T
	srv     *httptest.Server
	paste   bool
	version string // /healthz.version
	// issueLife and refreshLife are the access token lifetimes of the token call and of a refresh.
	issueLife, refreshLife time.Duration
	// refreshBarrier holds the first refresh up to this long for a second to arrive.
	refreshBarrier time.Duration

	mu        sync.Mutex
	signIns   map[uuid.UUID]*fakeSignIn
	sessions  map[uuid.UUID]*fakeSession
	tokens    map[string]fakeToken // bearer -> owner
	refreshes int
	inflight  chan struct{}
	bearers   []string // every Authorization header seen
}

type fakeSignIn struct {
	req  types.CLISignInCreateRequest
	code string // K once approved
	used bool
}

type fakeSession struct {
	pub     ed25519.PublicKey
	secret  string
	ended   string
	machine string
}

type fakeToken struct {
	session uuid.UUID
	kind    string
}

const fakeEmail = "you@example.com"

func newSignInOrg(t *testing.T) *signInOrg {
	t.Helper()
	f := &signInOrg{t: t, issueLife: time.Hour, refreshLife: time.Hour, signIns: map[uuid.UUID]*fakeSignIn{},
		sessions: map[uuid.UUID]*fakeSession{}, tokens: map[string]fakeToken{}, inflight: make(chan struct{}, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeFake(w, http.StatusOK, map[string]any{"status": "ok", "cli_sign_in": true, "cli_paste_sign_in": f.paste, "version": f.version})
	})
	mux.HandleFunc("POST /api/v1/cli/sign-ins", f.create)
	mux.HandleFunc("POST /api/v1/cli/sign-ins/{id}/token", f.token)
	mux.HandleFunc("POST /api/v1/cli/sessions/{id}/refresh", f.refresh)
	mux.HandleFunc("GET /api/v1/me", f.me)
	mux.HandleFunc("DELETE /api/v1/me/cli-sessions/{id}", f.signOut)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if a := r.Header.Get("Authorization"); a != "" {
			f.bearers = append(f.bearers, a)
		}
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func writeFake(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func refuseFake(w http.ResponseWriter, status int, reason, msg string) {
	writeFake(w, status, map[string]string{"error": msg, "reason": reason})
}

// decodeFake reads a body strictly, as the server's decodeStrict does, so a wire field the CLI invents fails.
func decodeFake(t *testing.T, r *http.Request, v any) bool {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		t.Errorf("%s %s: body does not decode strictly: %v", r.Method, r.URL.Path, err)
		return false
	}
	return true
}

func (f *signInOrg) proofOK(pub ed25519.PublicKey, route string, id uuid.UUID, secret string, p types.CLIProof) bool {
	sig, err := base64.RawURLEncoding.DecodeString(p.Signature)
	at := time.Unix(p.Time, 0)
	return err == nil && time.Since(at).Abs() <= types.CLIProofSkew &&
		ed25519.Verify(pub, types.CLIProofMessage(f.srv.URL, route, id, secret, p.Time), sig)
}

func (f *signInOrg) create(w http.ResponseWriter, r *http.Request) {
	var req types.CLISignInCreateRequest
	if !decodeFake(f.t, r, &req) {
		refuseFake(w, 400, "cli_sign_in_invalid_request", "bad body")
		return
	}
	if req.Mode == types.CLISignInModePaste && !f.paste {
		refuseFake(w, 403, "cli_paste_sign_in_off", "paste off")
		return
	}
	id := uuid.New()
	f.mu.Lock()
	f.signIns[id] = &fakeSignIn{req: req}
	f.mu.Unlock()
	writeFake(w, http.StatusCreated, types.CLISignInCreateResponse{ID: id, UserCode: "KQ7M-2XJD", ExpiresIn: 600})
}

// approve is the console's Approve: it returns K and, for loopback, the redirect the page navigates to.
func (f *signInOrg) approve(id uuid.UUID) (code, redirect string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	si := f.signIns[id]
	si.code = "k-" + id.String()[:8]
	if si.req.Mode == types.CLISignInModeLoopback {
		redirect = fmt.Sprintf("http://127.0.0.1:%d/callback?id=%s&code=%s", si.req.Port, id, si.code)
	}
	return si.code, redirect
}

func (f *signInOrg) grant(sess uuid.UUID, kind string, life time.Duration) types.CLISessionGrant {
	tok := "wdn_" + uuid.NewString()
	f.tokens[tok] = fakeToken{session: sess, kind: kind}
	now := time.Now()
	return types.CLISessionGrant{SessionID: sess, AccessToken: tok, Kind: kind, AccessExpiresAt: now.Add(life),
		SessionExpiresAt: now.Add(30 * 24 * time.Hour), IdleExpiresAt: now.Add(7 * 24 * time.Hour), Store: types.CLIStoreKeychain}
}

func (f *signInOrg) token(w http.ResponseWriter, r *http.Request) {
	id, _ := uuid.Parse(r.PathValue("id"))
	var req types.CLISignInTokenRequest
	if !decodeFake(f.t, r, &req) {
		refuseFake(w, 401, "cli_sign_in_invalid", "invalid")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	si := f.signIns[id]
	if si == nil {
		refuseFake(w, 401, "cli_sign_in_invalid", "invalid")
		return
	}
	sum := sha256.Sum256([]byte(req.Verifier))
	pub, _ := base64.RawURLEncoding.DecodeString(si.req.PublicKey)
	if si.used || si.code == "" || req.Code != si.code ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != si.req.Challenge ||
		!f.proofOK(pub, "/api/v1/cli/sign-ins/"+id.String()+"/token", id, req.Code, req.Proof) {
		refuseFake(w, 401, "cli_sign_in_invalid", "this sign-in cannot be completed: run wardyn login again")
		return
	}
	si.used = true
	if si.req.Admin {
		sess := f.sessions[*si.req.SessionID]
		if sess == nil || sess.ended != "" || !sess.pub.Equal(ed25519.PublicKey(pub)) {
			refuseFake(w, 401, "cli_sign_in_invalid", "invalid")
			return
		}
		writeFake(w, http.StatusCreated, f.grant(*si.req.SessionID, types.APITokenKindCLIAdmin, time.Hour))
		return
	}
	sid := uuid.New()
	secret := "wdc_" + uuid.NewString()
	f.sessions[sid] = &fakeSession{pub: pub, secret: secret, machine: si.req.Machine}
	g := f.grant(sid, types.APITokenKindCLIAccess, f.issueLife)
	g.Secret, g.Store = secret, si.req.Store
	writeFake(w, http.StatusCreated, g)
}

func (f *signInOrg) refresh(w http.ResponseWriter, r *http.Request) {
	id, _ := uuid.Parse(r.PathValue("id"))
	var req types.CLISessionRefreshRequest
	if !decodeFake(f.t, r, &req) {
		refuseFake(w, 401, "cli_session_invalid", "x")
		return
	}
	f.inflight <- struct{}{}
	f.mu.Lock()
	f.refreshes++
	first := f.refreshes == 1
	f.mu.Unlock()
	if first && f.refreshBarrier > 0 {
		// Hold the first refresh until a second one is in flight, or the barrier passes.
		deadline := time.Now().Add(f.refreshBarrier)
		for len(f.inflight) < 2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	defer func() { <-f.inflight }()
	f.mu.Lock()
	defer f.mu.Unlock()
	sess := f.sessions[id]
	ended := func() {
		refuseFake(w, 401, "cli_session_invalid", "this CLI sign-in has ended: run wardyn login again")
	}
	if sess == nil || !f.proofOK(sess.pub, "/api/v1/cli/sessions/"+id.String()+"/refresh", id, req.Secret, req.Proof) || sess.ended != "" {
		ended()
		return
	}
	if req.Secret != sess.secret {
		sess.ended = types.CLISessionEndReuseDetected
		ended()
		return
	}
	sess.secret = "wdc_" + uuid.NewString()
	g := f.grant(id, types.APITokenKindCLIAccess, f.refreshLife)
	g.Secret = sess.secret
	writeFake(w, http.StatusOK, g)
}

// owner is the live session a bearer belongs to, or nil.
func (f *signInOrg) owner(r *http.Request) (*fakeToken, *fakeSession) {
	tok, ok := f.tokens[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
	if !ok {
		return nil, nil
	}
	sess := f.sessions[tok.session]
	if sess == nil || sess.ended != "" {
		return nil, nil
	}
	return &tok, sess
}

func (f *signInOrg) me(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer env-") {
		writeFake(w, http.StatusOK, map[string]any{"principal": "env-caller", "role": "admin", "email": ""})
		return
	}
	tok, _ := f.owner(r)
	if tok == nil {
		refuseFake(w, 401, "sign_in_required", "missing bearer token")
		return
	}
	role := "user"
	if tok.kind == types.APITokenKindCLIAdmin {
		role = "admin"
	}
	writeFake(w, http.StatusOK, map[string]any{"principal": "sub-1", "email": fakeEmail, "role": role})
}

func (f *signInOrg) signOut(w http.ResponseWriter, r *http.Request) {
	id, _ := uuid.Parse(r.PathValue("id"))
	f.mu.Lock()
	defer f.mu.Unlock()
	tok, sess := f.owner(r)
	if tok == nil {
		refuseFake(w, 401, "sign_in_required", "missing bearer token")
		return
	}
	if tok.session != id {
		refuseFake(w, 404, "cli_session_not_found", "no such CLI sign-in")
		return
	}
	sess.ended = types.CLISessionEndSignedOut
	w.WriteHeader(http.StatusNoContent)
}

// onlySignIn is the one sign-in request the fake has seen.
func (f *signInOrg) onlySignIn(t *testing.T) (uuid.UUID, *fakeSignIn) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.signIns) == 1 {
		for id, si := range f.signIns {
			return id, si
		}
	}
	t.Fatalf("the fake saw %d sign-in requests, want 1", len(f.signIns))
	return uuid.Nil, nil
}

func (f *signInOrg) liveSessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.sessions {
		if s.ended == "" {
			n++
		}
	}
	return n
}

// signInIDFrom is the sign-in id in the approve URL the CLI opened.
func signInIDFrom(t *testing.T, approve string) uuid.UUID {
	t.Helper()
	u, err := url.Parse(approve)
	if err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(u.Query().Get("id"))
	if err != nil {
		t.Fatalf("approve URL %q carries no sign-in id", approve)
	}
	return id
}

// fetch GETs u (the loopback callback) and returns its status and body.
func fetch(t *testing.T, u string) (int, string) {
	t.Helper()
	resp, err := http.Get(u) //nolint:noctx // a test playing the browser
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// cliEnv isolates the profile, swaps go-keyring for its in-memory mock and makes this a computer with a
// keychain and a browser. browser plays the person in the browser for each approve URL the CLI opens.
func cliEnv(t *testing.T, browser func(approve string)) string {
	t.Helper()
	dir := isolateProfile(t)
	keyring.MockInit()
	prevReady, prevOpen, prevCan, prevWait := keychainReady, openBrowser, canOpenBrowser, loopbackWait
	t.Cleanup(func() {
		keychainReady, openBrowser, canOpenBrowser, loopbackWait = prevReady, prevOpen, prevCan, prevWait
	})
	keychainReady = func() error { return nil }
	canOpenBrowser = func() bool { return true }
	openBrowser = func(approve string) error {
		go browser(approve)
		return nil
	}
	return dir
}

// approveInBrowser is the person pressing Approve: the console navigates to the loopback redirect. The
// page the browser got arrives on page, when it is not nil.
func approveInBrowser(t *testing.T, f *signInOrg, page chan<- string) func(string) {
	return func(approve string) {
		_, redirect := f.approve(signInIDFrom(t, approve))
		_, body := fetch(t, redirect)
		if page != nil {
			page <- body
		}
	}
}

// runCLI runs the CLI with stdin and returns stdout, stderr and the error.
func runCLI(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	root := rootCmd()
	root.SetArgs(args)
	var out, errOut strings.Builder
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(strings.NewReader(stdin))
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func storedItem(t *testing.T, origin string) cliItem {
	t.Helper()
	raw, err := keyring.Get(cliKeyringService, origin)
	if err != nil {
		t.Fatalf("keychain item for %s: %v", origin, err)
	}
	var it cliItem
	if err := json.Unmarshal([]byte(raw), &it); err != nil {
		t.Fatal(err)
	}
	return it
}

// TestLoginLoopbackSignsInAndWhoamiPrintsNoSecret: a browser sign-in stores the session, the device key
// and the access token in the keychain under its own URL, prints the board's three lines, and whoami then
// names the person, the user role and the store without printing a secret.
func TestLoginLoopbackSignsInAndWhoamiPrintsNoSecret(t *testing.T) {
	pages := make(chan string, 1)
	f := newSignInOrg(t)
	dir := cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, pages)(approve); return nil }

	out, _, err := runCLI(t, "", "login", f.srv.URL)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	id, si := f.onlySignIn(t)
	host := strings.TrimPrefix(f.srv.URL, "http://")
	want := fmt.Sprintf("Opening %s/cli/sign-in?id=%s in your browser.\nCheck that the page shows this code: KQ7M-2XJD\nSigned in to %s as %s on %s\n",
		f.srv.URL, id, host, fakeEmail, machineName())
	if out != want {
		t.Errorf("login printed\n%q\nwant\n%q", out, want)
	}
	if page := <-pages; !strings.Contains(page, pageSignedIn) {
		t.Errorf("the browser's page = %q, want it to say %q", page, pageSignedIn)
	}
	if si.req.Mode != types.CLISignInModeLoopback || si.req.Store != types.CLIStoreKeychain || si.req.Port == 0 || si.req.Admin {
		t.Errorf("sign-in request = %+v, want loopback, keychain, a port, no admin", si.req)
	}
	it := storedItem(t, f.srv.URL)
	if !strings.HasPrefix(it.Secret, "wdc_") || it.AccessToken == "" || it.KeySeed == "" {
		t.Fatalf("stored item = %+v, want secret, access token and key", it)
	}
	if k, _ := cliprofile.ReadSessionStore(dir, f.srv.URL); k != types.CLIStoreKeychain {
		t.Errorf("profile names store %q for %s, want keychain", k, f.srv.URL)
	}

	out, _, err = runCLI(t, "", "whoami", "--url", f.srv.URL)
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 3 || lines[0] != f.srv.URL || lines[1] != fakeEmail+" (user; admin commands need `wardyn login --admin`)" ||
		!strings.HasPrefix(lines[2], "keychain · session ends in ") || !strings.Contains(lines[2], " days · idle end in ") {
		t.Errorf("whoami printed %q", out)
	}
	for _, secret := range []string{it.Secret, it.AccessToken, it.KeySeed} {
		if strings.Contains(out, secret) {
			t.Errorf("whoami printed a secret: %q", out)
		}
	}
}

// TestLoginLoopbackAcceptsOnlyItsIDAndCloses: the callback answers a request for another sign-in with a 404
// and keeps waiting, takes its own once, and then stops listening.
func TestLoginLoopbackAcceptsOnlyItsIDAndCloses(t *testing.T) {
	f := newSignInOrg(t)
	cliEnv(t, nil)
	var stranger int
	var redirect string
	done := make(chan struct{})
	openBrowser = func(approve string) error {
		go func() {
			defer close(done)
			_, redirect = f.approve(signInIDFrom(t, approve))
			u, _ := url.Parse(redirect)
			q := u.Query()
			q.Set("id", uuid.NewString())
			u.RawQuery = q.Encode()
			stranger, _ = fetch(t, u.String())
			fetch(t, redirect)
		}()
		return nil
	}
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("login: %v", err)
	}
	<-done
	if stranger != http.StatusNotFound {
		t.Errorf("a callback for another sign-in got %d, want 404", stranger)
	}
	if resp, err := http.Get(redirect); err == nil { //nolint:noctx
		resp.Body.Close()
		t.Errorf("the callback still answers after the sign-in (%d); it must close", resp.StatusCode)
	}
}

// TestLoginDeniedSavesNothing: Deny in the browser ends login with the 11.14 line, exit 1, nothing stored.
func TestLoginDeniedSavesNothing(t *testing.T) {
	f := newSignInOrg(t)
	dir := cliEnv(t, nil)
	pages := make(chan string, 1)
	openBrowser = func(approve string) error {
		go func() {
			id := signInIDFrom(t, approve)
			f.mu.Lock()
			port := f.signIns[id].req.Port
			f.mu.Unlock()
			_, page := fetch(t, fmt.Sprintf("http://127.0.0.1:%d/callback?id=%s&error=denied", port, id))
			pages <- page
		}()
		return nil
	}
	_, _, err := runCLI(t, "", "login", f.srv.URL)
	if err == nil || err.Error() != errDenied || exitCodeFor(err) != 1 {
		t.Fatalf("login after Deny: %v (exit %d), want %q and exit 1", err, exitCodeFor(err), errDenied)
	}
	if page := <-pages; !strings.Contains(page, pageDeniedBody) {
		t.Errorf("denied page = %q", page)
	}
	if _, err := keyring.Get(cliKeyringService, f.srv.URL); !errors.Is(err, keyring.ErrNotFound) {
		t.Errorf("a denied sign-in left a keychain item (%v)", err)
	}
	if k, _ := cliprofile.ReadSessionStore(dir, f.srv.URL); k != "" {
		t.Errorf("a denied sign-in left the profile naming store %q", k)
	}
}

// TestLoginNoBrowser covers paste-back and the no-browser refusals.
func TestLoginNoBrowser(t *testing.T) {
	t.Run("--no-browser pastes the code back", func(t *testing.T) {
		f := newSignInOrg(t)
		f.paste = true
		cliEnv(t, nil)
		openBrowser = func(string) error { t.Error("paste-back opened a browser"); return nil }
		// The person reads K off the approve page; the fake's K is derived from the id, so approve on create.
		r, w := io.Pipe()
		go func() {
			for {
				f.mu.Lock()
				n := len(f.signIns)
				f.mu.Unlock()
				if n == 1 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			id, _ := f.onlySignIn(t)
			code, _ := f.approve(id)
			_, _ = io.WriteString(w, code+"\n")
			_ = w.Close()
		}()
		root := rootCmd()
		root.SetArgs([]string{"login", "--no-browser", f.srv.URL})
		var out, errOut strings.Builder
		root.SetOut(&out)
		root.SetErr(&errOut)
		root.SetIn(r)
		if err := root.Execute(); err != nil {
			t.Fatalf("login --no-browser: %v", err)
		}
		id, si := f.onlySignIn(t)
		if si.req.Mode != types.CLISignInModePaste || si.req.Port != 0 {
			t.Errorf("request = %+v, want paste with no port", si.req)
		}
		if !strings.HasPrefix(out.String(), fmt.Sprintf("Open %s/cli/sign-in?id=%s in a browser on any computer.\n", f.srv.URL, id)) ||
			!strings.Contains(errOut.String(), copyPastePrompt) {
			t.Errorf("stdout %q stderr %q", out.String(), errOut.String())
		}
		if storedItem(t, f.srv.URL).AccessToken == "" {
			t.Error("paste-back stored no session")
		}
	})
	for _, tc := range []struct {
		name           string
		args           []string
		paste, canOpen bool
		want           string
	}{
		{"--no-browser while the organisation has paste off", []string{"--no-browser"}, false, true, errNoPasteMode},
		{"no browser here and paste off", nil, false, false, errNoBrowserNoPaste},
		{"no browser here and paste on", nil, true, false, errNoBrowserPaste},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSignInOrg(t)
			f.paste = tc.paste
			cliEnv(t, nil)
			canOpenBrowser = func() bool { return tc.canOpen }
			_, _, err := runCLI(t, "", append(append([]string{"login"}, tc.args...), f.srv.URL)...)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("login: %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("the browser never comes back", func(t *testing.T) {
		f := newSignInOrg(t)
		f.paste = true
		cliEnv(t, nil)
		loopbackWait = 50 * time.Millisecond
		openBrowser = func(string) error { return nil }
		if _, _, err := runCLI(t, "", "login", f.srv.URL); err == nil || err.Error() != errNoCallbackPaste {
			t.Fatalf("login: %v, want %q", err, errNoCallbackPaste)
		}
	})
}

// TestSessionFileFallbackWithNoBus: with no session bus the sign-in goes to a 0600 file in a 0700
// directory, the server is told "file", and stderr says so.
func TestSessionFileFallbackWithNoBus(t *testing.T) {
	f := newSignInOrg(t)
	dir := cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	keychainReady = func() error { return errors.New("no session bus (DBUS_SESSION_BUS_ADDRESS is unset)") }
	_, stderr, err := runCLI(t, "", "login", f.srv.URL)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	path := filepath.Join(dir, cliSessionFile)
	// The cap in the line is the one the server enforces on a file-store session, in words.
	if want := fmt.Sprintf(copyFileStore, path, inWords(types.CLIFileStoreSessionCap)); !strings.Contains(stderr, want) {
		t.Errorf("stderr %q, want the file-store line %q", stderr, want)
	}
	_, si := f.onlySignIn(t)
	if si.req.Store != types.CLIStoreFile {
		t.Errorf("store reported = %q, want file", si.req.Store)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	di, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Errorf("file mode %o, directory mode %o; want 600 and 700", fi.Mode().Perm(), di.Mode().Perm())
	}
	if k, _ := cliprofile.ReadSessionStore(dir, f.srv.URL); k != types.CLIStoreFile {
		t.Errorf("profile names %q, want file", k)
	}
	if out, _, err := runCLI(t, "", "whoami", "--url", f.srv.URL); err != nil || !strings.Contains(out, "\nfile · session ends in") {
		t.Errorf("whoami: %q, %v", out, err)
	}
}

// TestSessionRefreshUnderLockIsOne: two commands that find the access token spent at once make one refresh;
// the second waits for the lock and reads the rotated item, so the server never sees a replayed secret.
func TestSessionRefreshUnderLockIsOne(t *testing.T) {
	f := newSignInOrg(t)
	cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	// The file store: go-keyring's mock is a map with no lock, where a real keychain is a store two
	// processes share; the lock under test is the same for both.
	keychainReady = func() error { return errors.New("no session bus") }
	f.issueLife = 0 // the first access token is spent at once
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("login: %v", err)
	}
	f.refreshBarrier = time.Second
	var wg sync.WaitGroup
	got := make([]storedSession, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = loadSession(context.Background(), f.srv.URL)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("command %d: %v", i, err)
		}
	}
	f.mu.Lock()
	refreshes := f.refreshes
	f.mu.Unlock()
	if refreshes != 1 || got[0].token == "" || got[0].token != got[1].token {
		t.Errorf("refreshes = %d, tokens %q / %q; want one refresh shared by both", refreshes, got[0].token, got[1].token)
	}
	if f.liveSessions() != 1 {
		t.Error("the server ended the session (a reuse)")
	}
}

// TestSessionEndedIsNamedAndNotSentAgain: a refresh the server refuses (here a replayed old secret, which
// ends the session as a reuse) marks the item ended; the error names the server's reason, and the next
// command reports it without another refresh.
func TestSessionEndedIsNamedAndNotSentAgain(t *testing.T) {
	f := newSignInOrg(t)
	cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	f.issueLife = 0
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("login: %v", err)
	}
	// A crash after the server rotated: the stored secret is the spent one.
	it := storedItem(t, f.srv.URL)
	it.Secret = "wdc_spent"
	if err := (keychainStore{}).put(f.srv.URL, it); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(errSessionEnded, f.srv.URL, fmt.Sprintf(endReasonServer, "cli_session_invalid"))
	_, _, err := runCLI(t, "", "whoami", "--url", f.srv.URL)
	if err == nil || err.Error() != want {
		t.Fatalf("whoami: %v, want %q", err, want)
	}
	if f.liveSessions() != 0 {
		t.Error("the replayed secret did not end the session at the server")
	}
	before := f.refreshes
	if _, _, err := runCLI(t, "", "whoami", "--url", f.srv.URL); err == nil || err.Error() != want {
		t.Fatalf("second whoami: %v, want %q", err, want)
	}
	if f.refreshes != before {
		t.Errorf("an ended sign-in was presented again (%d refreshes, want %d)", f.refreshes, before)
	}
	// Past its absolute lifetime on its own clock, it ends without asking the server.
	it = storedItem(t, f.srv.URL)
	it.Ended, it.SessionExpiresAt = "", time.Now().Add(-time.Minute)
	_ = (keychainStore{}).put(f.srv.URL, it)
	if _, _, err := runCLI(t, "", "whoami", "--url", f.srv.URL); err == nil || err.Error() != fmt.Sprintf(errSessionEnded, f.srv.URL, endReasonExpired) {
		t.Errorf("expired: %v", err)
	}
}

// TestSessionTokenPrecedence: --token > WARDYN_ADMIN_TOKEN > WARDYN_TOKEN > the stored sign-in.
func TestSessionTokenPrecedence(t *testing.T) {
	f := newSignInOrg(t)
	cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("login: %v", err)
	}
	stored := "Bearer " + storedItem(t, f.srv.URL).AccessToken
	for _, tc := range []struct {
		name, flag, admin, env, want, from string
	}{
		{"--token wins", "env-flag", "env-admin", "env-tok", "Bearer env-flag", "token from --token"},
		{"then WARDYN_ADMIN_TOKEN", "", "env-admin", "env-tok", "Bearer env-admin", "token from WARDYN_ADMIN_TOKEN"},
		{"then WARDYN_TOKEN", "", "", "env-tok", "Bearer env-tok", "token from WARDYN_TOKEN"},
		{"then the stored sign-in", "", "", "", stored, "keychain · "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WARDYN_ADMIN_TOKEN", tc.admin)
			t.Setenv("WARDYN_TOKEN", tc.env)
			args := []string{"whoami", "--url", f.srv.URL}
			if tc.flag != "" {
				args = append(args, "--token", tc.flag)
			}
			f.mu.Lock()
			f.bearers = nil
			f.mu.Unlock()
			out, _, err := runCLI(t, "", args...)
			if err != nil {
				t.Fatalf("whoami: %v", err)
			}
			if len(f.bearers) != 1 || f.bearers[0] != tc.want {
				t.Errorf("bearers sent %q, want [%q]", f.bearers, tc.want)
			}
			if !strings.Contains(out, "\n"+tc.from) {
				t.Errorf("whoami printed %q, want a line starting %q", out, tc.from)
			}
		})
	}
}

// TestSessionNeverLeavesItsOrigin: a sign-in stored for one organisation is not sent to another, even
// when the profile's URL is the first one.
func TestSessionNeverLeavesItsOrigin(t *testing.T) {
	a, b := newSignInOrg(t), newSignInOrg(t)
	cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, a, nil)(approve); return nil }
	if _, _, err := runCLI(t, "", "login", a.srv.URL); err != nil {
		t.Fatalf("login: %v", err)
	}
	_, _, err := runCLI(t, "", "whoami", "--url", b.srv.URL)
	if err == nil || err.Error() != fmt.Sprintf(errNotSignedIn, b.srv.URL) {
		t.Errorf("whoami against another organisation: %v", err)
	}
	if len(b.bearers) != 0 {
		t.Errorf("organisation B was sent %q", b.bearers)
	}
}

// TestLogoutEndsTheSessionAndDeletesTheItem, from either store.
func TestLogoutEndsTheSessionAndDeletesTheItem(t *testing.T) {
	for _, kind := range []string{types.CLIStoreKeychain, types.CLIStoreFile} {
		t.Run(kind, func(t *testing.T) {
			f := newSignInOrg(t)
			dir := cliEnv(t, nil)
			openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
			if kind == types.CLIStoreFile {
				keychainReady = func() error { return errors.New("no session bus") }
			}
			if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
				t.Fatalf("login: %v", err)
			}
			out, _, err := runCLI(t, "", "logout", "--url", f.srv.URL)
			if err != nil || out != fmt.Sprintf(copyLoggedOut+"\n", f.srv.URL) {
				t.Fatalf("logout: %q, %v", out, err)
			}
			if f.liveSessions() != 0 {
				t.Error("logout did not end the session at the server")
			}
			if _, ok, _ := storeFor(kind, dir).get(f.srv.URL); ok {
				t.Error("logout left the stored item")
			}
			if k, _ := cliprofile.ReadSessionStore(dir, f.srv.URL); k != "" {
				t.Errorf("logout left the profile naming %q", k)
			}
			if out, _, err := runCLI(t, "", "logout", "--url", f.srv.URL); err != nil || out != fmt.Sprintf(copyNotSignedIn+"\n", f.srv.URL) {
				t.Errorf("second logout: %q, %v", out, err)
			}
		})
	}
}

// TestLoginAdminStepUp: login --admin asks for a step-up on this computer's live session with its device
// key, and the cli_admin token is then the one sent, with the real role.
func TestLoginAdminStepUp(t *testing.T) {
	f := newSignInOrg(t)
	cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("login: %v", err)
	}
	base := storedItem(t, f.srv.URL)
	out, _, err := runCLI(t, "", "login", "--admin", f.srv.URL)
	if err != nil {
		t.Fatalf("login --admin: %v", err)
	}
	host := strings.TrimPrefix(f.srv.URL, "http://")
	if !strings.Contains(out, "Admin access to "+host+" as "+fakeEmail+" until ") {
		t.Errorf("login --admin printed %q", out)
	}
	var admin *fakeSignIn
	f.mu.Lock()
	for _, si := range f.signIns {
		if si.req.Admin {
			admin = si
		}
	}
	f.mu.Unlock()
	if admin == nil || admin.req.SessionID == nil || *admin.req.SessionID != base.SessionID {
		t.Fatalf("no step-up request naming session %s", base.SessionID)
	}
	it := storedItem(t, f.srv.URL)
	if it.AdminToken == "" || it.Secret != base.Secret || it.KeySeed != base.KeySeed {
		t.Errorf("after the step-up the item is %+v; want the admin token beside the same session", it)
	}
	out, _, err = runCLI(t, "", "whoami", "--url", f.srv.URL)
	if err != nil || !strings.Contains(out, fakeEmail+" (admin; admin access ends in ") {
		t.Errorf("whoami after the step-up: %q, %v", out, err)
	}
}

// TestLoginVersionNotice: after a successful login or whoami, at most once a day per URL, one stderr line
// says the organisation runs another release; never on stdout, and never from a development build.
func TestLoginVersionNotice(t *testing.T) {
	f := newSignInOrg(t)
	cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	prev := cliVersion
	t.Cleanup(func() { cliVersion = prev })
	cliVersion = func() string { return "0.10.0" }
	f.version = "0.10.2+abc1234"
	want := fmt.Sprintf(copyNoticeServerNewer, "0.10.0", f.srv.URL, "0.10.2+abc1234") + "\n"

	out, stderr, err := runCLI(t, "", "login", f.srv.URL)
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if strings.Count(stderr, want) != 1 || strings.Contains(out, "this CLI is") {
		t.Errorf("login: stderr %q stdout %q, want the notice once on stderr only", stderr, out)
	}
	out, stderr, err = runCLI(t, "", "whoami", "--url", f.srv.URL)
	if err != nil || strings.Contains(stderr+out, "this CLI is") {
		t.Errorf("whoami within the day printed the notice again: stderr %q, %v", stderr, err)
	}
	// A day later it checks, and says it, again.
	path := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "wardyn", noticeCacheFile)
	checks := map[string]noticeCheck{}
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &checks); err != nil || checks[f.srv.URL].Version == "" {
		t.Fatalf("cache %s = %q, %v", path, raw, err)
	}
	checks[f.srv.URL] = noticeCheck{CheckedAt: time.Now().Add(-25 * time.Hour), Version: "0.10.2"}
	raw, _ = json.Marshal(checks)
	_ = os.WriteFile(path, raw, 0o644)
	if _, stderr, err = runCLI(t, "", "whoami", "--url", f.srv.URL); err != nil || strings.Count(stderr, want) != 1 {
		t.Errorf("whoami a day later: stderr %q, %v; want the notice", stderr, err)
	}

	t.Run("a development build never checks", func(t *testing.T) {
		cliVersion = func() string { return "0.10.0+80852d67f-dirty" }
		_ = os.Remove(path)
		if _, stderr, err := runCLI(t, "", "whoami", "--url", f.srv.URL); err != nil || strings.Contains(stderr, "this CLI is") {
			t.Errorf("a development build printed %q (%v)", stderr, err)
		}
	})
	for _, tc := range []struct{ cli, server, want string }{
		{"0.10.0", "0.10.0", ""},
		{"0.10.0", "0.10.0+abc1234", ""},
		{"0.10.0", "0.10.2", fmt.Sprintf(copyNoticeServerNewer, "0.10.0", "https://o", "0.10.2")},
		{"0.10.2", "0.9.6", fmt.Sprintf(copyNoticeServerOlder, "0.10.2", "https://o", "0.9.6")},
		{"0.10.0", "dev", fmt.Sprintf(copyNoticeServerNewer, "0.10.0", "https://o", "dev")},
	} {
		if got := noticeLine(tc.cli, "https://o", tc.server); got != tc.want {
			t.Errorf("noticeLine(%q, %q) = %q, want %q", tc.cli, tc.server, got, tc.want)
		}
	}
}
