// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestLogoutWarnsWhenTheStoredSignInCannotBeRead: a stored sign-in that cannot be read may still be live at
// the server, so logout deletes it here and says the server was not told, instead of a clean sign-out.
func TestLogoutWarnsWhenTheStoredSignInCannotBeRead(t *testing.T) {
	const org = "https://org.example"
	for _, tc := range []struct {
		kind   string
		damage func(dir string) error
	}{
		{types.CLIStoreFile, func(dir string) error {
			return os.WriteFile(filepath.Join(dir, cliSessionFile), []byte("{"), 0o600)
		}},
		{types.CLIStoreKeychain, func(string) error { return keyring.Set(cliKeyringService, org, "{") }},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			dir := cliEnv(t, nil)
			if err := cliprofile.WriteSessionStore(dir, org, tc.kind); err != nil {
				t.Fatal(err)
			}
			if err := tc.damage(dir); err != nil {
				t.Fatal(err)
			}
			out, _, err := runCLI(t, "", "logout", "--url", org)
			want := fmt.Sprintf("signed out on this computer, but %s did not end the session (", org)
			if err == nil || !strings.HasPrefix(err.Error(), want) || out != "" {
				t.Errorf("logout: %q, %v; want no clean sign-out and an error starting %q", out, err, want)
			}
			if err != nil && !strings.Contains(err.Error(), copyWasDamaged+")") {
				t.Errorf("logout: %v; want the damage in the past and no advice to sign in again", err)
			}
			if _, ok, err := storeFor(tc.kind, dir).get(org); ok || err != nil {
				t.Errorf("the damaged item is still stored (found=%v, %v)", ok, err)
			}
			if k, _ := cliprofile.ReadSessionStore(dir, org); k != "" {
				t.Errorf("the profile still names %q", k)
			}
		})
	}
}

// TestLoginKeepsTheOldSignInWhenTheNewOneCannotBeStored: a sign-in that moves to another store writes the new
// item before it deletes the old one, so a store that refuses the write leaves the old sign-in, and the
// profile's choice of store, working as they were; once the write is taken, the old item goes.
func TestLoginKeepsTheOldSignInWhenTheNewOneCannotBeStored(t *testing.T) {
	f := newSignInOrg(t)
	dir := cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	keychainReady = func() error { return errors.New("no session bus (DBUS_SESSION_BUS_ADDRESS is unset)") }
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("first login: %v", err)
	}
	old, ok, err := fileStore{dir}.get(f.srv.URL)
	if err != nil || !ok {
		t.Fatalf("first login stored nothing in the file (%v)", err)
	}
	// The keychain answers now, so the second sign-in chooses it, then refuses the write.
	keychainReady = func() error { return nil }
	openBrowser = func(approve string) error {
		go func() {
			keyring.MockInitWithError(errors.New("the keychain is locked")) // after the store was chosen
			approveInBrowser(t, f, nil)(approve)
		}()
		return nil
	}
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err == nil {
		t.Fatal("second login succeeded with a keychain that refuses the write")
	}
	if got, ok, err := (fileStore{dir}).get(f.srv.URL); err != nil || !ok || got.SessionID != old.SessionID {
		t.Errorf("the old sign-in = %v, found=%v, %v; want it kept", got.SessionID, ok, err)
	}
	if k, _ := cliprofile.ReadSessionStore(dir, f.srv.URL); k != types.CLIStoreFile {
		t.Errorf("the profile names %q, want file", k)
	}
	if out, _, err := runCLI(t, "", "whoami", "--url", f.srv.URL); err != nil || !strings.Contains(out, "\nfile · session ends in") {
		t.Errorf("whoami after the failed sign-in: %q, %v", out, err)
	}
	// A keychain that takes the write: the new item is stored and named, and the file's item goes.
	keyring.MockInit()
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	if _, stderr, err := runCLI(t, "", "login", f.srv.URL); err != nil || strings.Contains(stderr, "could not be removed") {
		t.Fatalf("third login: %v, stderr %q; want no line about the old sign-in", err, stderr)
	}
	if storedItem(t, f.srv.URL).SessionID == old.SessionID {
		t.Error("the keychain holds the old sign-in")
	}
	if _, ok, err := (fileStore{dir}).get(f.srv.URL); ok || err != nil {
		t.Errorf("the file still holds a sign-in (found=%v, %v)", ok, err)
	}
	if k, _ := cliprofile.ReadSessionStore(dir, f.srv.URL); k != types.CLIStoreKeychain {
		t.Errorf("the profile names %q, want keychain", k)
	}
}

// TestLoginSaysWhenTheOldSignInCannotBeRemoved: a sign-in that moves to the other store and cannot delete the
// old store's item still signs in, with the new store selected, and says on stderr where the old one remains.
func TestLoginSaysWhenTheOldSignInCannotBeRemoved(t *testing.T) {
	f := newSignInOrg(t)
	dir := cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("first login: %v", err)
	}
	old := storedItem(t, f.srv.URL)
	// No session bus now: the second sign-in goes to the file, and the keychain's item cannot be deleted.
	noBus := errors.New("no session bus (DBUS_SESSION_BUS_ADDRESS is unset)")
	keychainReady = func() error { return noBus }
	_, stderr, err := runCLI(t, "", "login", f.srv.URL)
	want := fmt.Sprintf(copyPreviousNotRemoved+"\n", "the keychain", noBus)
	if err != nil || strings.Count(stderr, want) != 1 {
		t.Fatalf("second login: %v, stderr %q; want it signed in with one line %q", err, stderr, want)
	}
	if got, ok, err := (fileStore{dir}).get(f.srv.URL); err != nil || !ok || got.SessionID == old.SessionID {
		t.Errorf("the file holds %v, found=%v, %v; want the new sign-in", got.SessionID, ok, err)
	}
	if k, _ := cliprofile.ReadSessionStore(dir, f.srv.URL); k != types.CLIStoreFile {
		t.Errorf("the profile names %q, want file", k)
	}
	// The line is true: the keychain still holds the old item.
	keychainReady = func() error { return nil }
	if storedItem(t, f.srv.URL).SessionID != old.SessionID {
		t.Error("the keychain no longer holds the old sign-in")
	}
}

// TestLoginSaysWhenTheOldSignInCannotBeRead: a sign-in whose profile names the keychain, made with no session
// bus, goes to the file and cannot read the old item, so it cannot sign the old session out; it says so.
func TestLoginSaysWhenTheOldSignInCannotBeRead(t *testing.T) {
	f := newSignInOrg(t)
	cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("first login: %v", err)
	}
	noBus := errors.New("no session bus (DBUS_SESSION_BUS_ADDRESS is unset)")
	keychainReady = func() error { return noBus }
	_, stderr, err := runCLI(t, "", "login", f.srv.URL)
	want := fmt.Sprintf(copyPreviousNotEnded+"\n", noBus)
	if err != nil || strings.Count(stderr, want) != 1 {
		t.Fatalf("second login: %v, stderr %q; want it signed in with one line %q", err, stderr, want)
	}
	// The line is true: the old session was not signed out.
	if n := f.liveSessions(); n != 2 {
		t.Errorf("live sessions at the server = %d, want 2", n)
	}
}

// TestLoginOverADamagedSignIn: a sign-in that replaces an item too damaged to read says the old sign-in was
// not signed out, without the read error's advice to sign in again, which the person has just done.
func TestLoginOverADamagedSignIn(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		damage func(dir, org string) error
		cause  func(dir string) string
	}{
		{types.CLIStoreFile, func(dir, _ string) error {
			return os.WriteFile(filepath.Join(dir, cliSessionFile), []byte("{"), 0o600)
		}, func(dir string) string { return filepath.Join(dir, cliSessionFile) + copyWasDamaged }},
		{types.CLIStoreKeychain, func(_, org string) error { return keyring.Set(cliKeyringService, org, "{") },
			func(string) string { return "the keychain item for the CLI sign-in" + copyWasDamaged }},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			f := newSignInOrg(t)
			dir := cliEnv(t, nil)
			openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
			if err := cliprofile.WriteSessionStore(dir, f.srv.URL, tc.kind); err != nil {
				t.Fatal(err)
			}
			if err := tc.damage(dir, f.srv.URL); err != nil {
				t.Fatal(err)
			}
			_, stderr, err := runCLI(t, "", "login", f.srv.URL)
			want := fmt.Sprintf(copyPreviousNotEnded+"\n", tc.cause(dir))
			if err != nil || strings.Count(stderr, want) != 1 || strings.Contains(stderr, "run `wardyn login` again") {
				t.Fatalf("login: %v, stderr %q; want it signed in with one line %q and no advice to sign in again", err, stderr, want)
			}
			// The line is true: the new sign-in replaced the damaged item.
			if storedItem(t, f.srv.URL).AccessToken == "" {
				t.Error("the keychain does not hold the new sign-in")
			}
		})
	}
}

// TestFailedLoginSaysNothingAboutTheOldSignIn: the line about a sign-in that could not be read prints only
// once the new one is stored, so a login that fails does not print it.
func TestFailedLoginSaysNothingAboutTheOldSignIn(t *testing.T) {
	f := newSignInOrg(t)
	dir := cliEnv(t, nil)
	if err := cliprofile.WriteSessionStore(dir, f.srv.URL, types.CLIStoreFile); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, cliSessionFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The keychain is chosen, then refuses the write.
	openBrowser = func(approve string) error {
		go func() {
			keyring.MockInitWithError(errors.New("the keychain is locked"))
			approveInBrowser(t, f, nil)(approve)
		}()
		return nil
	}
	_, stderr, err := runCLI(t, "", "login", f.srv.URL)
	if err == nil || strings.Contains(stderr, "could not be signed out") {
		t.Fatalf("login: %v, stderr %q; want it to fail with no line about the old sign-in", err, stderr)
	}
}

// TestLoginOverAProfileThatCannotBeRead: login replaces a profile it cannot read before it signs in, so the
// profile names no store; the sign-in it replaces is still found in the store the new one overwrites, and
// signed out.
func TestLoginOverAProfileThatCannotBeRead(t *testing.T) {
	f := newSignInOrg(t)
	dir := cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, cliprofile.FileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runCLI(t, "", "login", f.srv.URL); err != nil || strings.Contains(stderr, "could not be signed out") {
		t.Fatalf("second login: %v, stderr %q; want it signed in with the old sign-in signed out", err, stderr)
	}
	if n := f.liveSessions(); n != 1 {
		t.Errorf("live sessions at the server = %d, want 1: the replaced one signed out", n)
	}
}

// TestLoginOverAProfileThatCannotBeReadSignsOutTheOtherStore: when the profile it replaced named no store and
// the new sign-in goes to the keychain, the one it replaces is found in the file, signed out and removed.
func TestLoginOverAProfileThatCannotBeReadSignsOutTheOtherStore(t *testing.T) {
	f := newSignInOrg(t)
	dir := cliEnv(t, nil)
	openBrowser = func(approve string) error { go approveInBrowser(t, f, nil)(approve); return nil }
	keychainReady = func() error { return errors.New("no session bus (DBUS_SESSION_BUS_ADDRESS is unset)") }
	if _, _, err := runCLI(t, "", "login", f.srv.URL); err != nil {
		t.Fatalf("first login: %v", err)
	}
	keychainReady = func() error { return nil }
	if err := os.WriteFile(filepath.Join(dir, cliprofile.FileName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runCLI(t, "", "login", f.srv.URL); err != nil || strings.Contains(stderr, "could not be") {
		t.Fatalf("second login: %v, stderr %q; want it signed in with the old sign-in signed out and removed", err, stderr)
	}
	if n := f.liveSessions(); n != 1 {
		t.Errorf("live sessions at the server = %d, want 1: the replaced one signed out", n)
	}
	if _, ok, err := (fileStore{dir}).get(f.srv.URL); ok || err != nil {
		t.Errorf("the file still holds a sign-in (found=%v, %v)", ok, err)
	}
}

// TestOrgTransportsBoundTheHeaderWait: the transport a stored sign-in's requests go through, and an
// organisation's anchor transport, both bound the wait for response headers, as the SDK's own transport
// does, so a streamed request cannot wait forever on a peer that never answers.
func TestOrgTransportsBoundTheHeaderWait(t *testing.T) {
	isolateStore(t)
	session, ok := orgTransport(orgClient("https://org.example", 30*time.Second)).(*http.Transport)
	if !ok || session.ResponseHeaderTimeout <= 0 {
		t.Errorf("session base transport %T has no response-header bound", orgTransport(orgClient("https://org.example", 30*time.Second)))
	}
	anchors, ok := orgClientTrusting("https://org.example", 0, []*x509.Certificate{newTestCA(t, "Org CA").ca}).Transport.(*http.Transport)
	if !ok || anchors.ResponseHeaderTimeout <= 0 {
		t.Errorf("anchor transport has no response-header bound (%v)", ok)
	}
}
