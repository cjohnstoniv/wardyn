// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

import (
	"context"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

func TestNew_RefusesPlainHTTPOffLoopback(t *testing.T) {
	f := newFakeKV(t)
	for _, c := range []struct{ name, vault, authority string }{
		{"vault", "http://kv.example", f.srv.URL},
		{"authority", f.srv.URL, "http://login.example"},
	} {
		cfg := fakeConfig(f, writeFile(t, "projected-sa-1"))
		cfg.VaultURL, cfg.AuthorityHost = c.vault, c.authority
		if _, err := New(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "plain http://") {
			t.Errorf("%s over plain http = %v; want a refusal", c.name, err)
		}
	}
}

func TestNew_FailsClosedWhenTheTokenExchangeFails(t *testing.T) {
	f := newFakeKV(t)
	_, err := New(t.Context(), fakeConfig(f, writeFile(t, "not-a-federated-token")))
	if err == nil || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("New with a rejected assertion = %v; want boot to fail naming the error", err)
	}
	if strings.Contains(err.Error(), "not-a-federated-token") || strings.Contains(err.Error(), "trace id") {
		t.Fatalf("the error carries the assertion or a second line: %v", err)
	}
}

// The documented federated-credential exchange, with the projected token
// re-read at every exchange (the kubelet rotates it in place).
func TestWorkloadIdentity_RereadsTheProjectedTokenAtEveryExchange(t *testing.T) {
	f := newFakeKV(t)
	file := writeFile(t, "projected-sa-1")
	s := newFakeStore(t, f, func(c *Config) { c.FederatedTokenFile = file })
	f.mu.Lock()
	f.assertions["projected-sa-2"] = true
	f.mu.Unlock()
	if err := os.WriteFile(file, []byte("projected-sa-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.c.mu.Lock()
	s.c.refreshAt = time.Time{} // due
	s.c.mu.Unlock()
	if _, err := s.Put(t.Context(), "", "k", "", []byte("v"), false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exchanges != 2 || f.lastAssertion != "projected-sa-2" {
		t.Fatalf("exchanges = %d, last assertion %q; want 2, the rotated file", f.exchanges, f.lastAssertion)
	}
}

// The token is cached until five minutes before it expires, and not fetched
// per call.
func TestToken_CachedUntilFiveMinutesBeforeExpiry(t *testing.T) {
	f := newFakeKV(t)
	s := newFakeStore(t, f)
	for i := 0; i < 3; i++ {
		if _, err := s.Put(t.Context(), "", "k", "", []byte("v"), false); err != nil {
			t.Fatal(err)
		}
	}
	s.c.mu.Lock()
	left := time.Until(s.c.refreshAt)
	s.c.mu.Unlock()
	if f.exchanges != 1 || left < 54*time.Minute || left > 55*time.Minute {
		t.Fatalf("exchanges = %d, refresh in %v; want 1 exchange, refresh 55 min into a 60-min token", f.exchanges, left)
	}
}

func TestManagedIdentity_AsksIMDSWithTheMetadataHeader(t *testing.T) {
	f := newFakeKV(t)
	cfg := fakeConfig(f, "")
	cfg.Auth, cfg.TenantID = AuthManagedIdentity, ""
	s, err := build(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if s.c.imdsHTTP.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("the IMDS client would use a proxy")
	}
	s.c.imds = f.srv.URL + "/metadata/identity/oauth2/token"
	if _, err := s.Put(t.Context(), "", "k", "", []byte("v"), false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.imdsCalls != 1 || f.imdsHeader != "true" || f.imdsClientID != "client-1" {
		t.Fatalf("IMDS calls %d, Metadata %q, client_id %q", f.imdsCalls, f.imdsHeader, f.imdsClientID)
	}
}

// A 401 fetches a new token and retries, at most once per refreshEvery.
func TestUnauthorized_RefreshesTheTokenBounded(t *testing.T) {
	f := newFakeKV(t)
	s := newFakeStore(t, f)
	s.c.lastFetched = time.Time{}
	f.mu.Lock()
	f.tokens = map[string]bool{} // every token revoked
	f.mu.Unlock()
	if _, err := s.Put(t.Context(), "", "k", "", []byte("v"), false); err != nil {
		t.Fatalf("Put after the token was revoked = %v; want a refresh and a retry", err)
	}
	f.mu.Lock()
	f.tokens = map[string]bool{}
	f.mu.Unlock()
	_, err := s.Put(t.Context(), "", "k", "", []byte("v"), false)
	if statusOf(err) != http.StatusUnauthorized || errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("second 401 inside the window = %v; want a definitive 401", err)
	}
	if f.exchanges != 2 {
		t.Fatalf("exchanges = %d; want 2 (boot, one refresh)", f.exchanges)
	}
}

// Transient vs definitive (design §2.3a.3, §2.3a.4).
func TestClassification(t *testing.T) {
	f := newFakeKV(t)
	s := newFakeStore(t, f)
	ref, err := s.Put(t.Context(), "", "k", "", []byte("v"), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		force     []int
		transient bool
	}{
		{[]int{429, 429, 429, 429}, true},
		{[]int{500, 503, 502, 504}, true},
		{[]int{403}, false},
		{[]int{400}, false},
	} {
		f.mu.Lock()
		f.force = c.force
		f.mu.Unlock()
		_, err := s.Get(t.Context(), "", "k", ref)
		if err == nil || errors.Is(err, secretstore.ErrUnavailable) != c.transient {
			t.Errorf("forced %v: Get = %v; transient want %v", c.force, err, c.transient)
		}
	}
	// A token endpoint that stops answering is transient at runtime.
	s.c.mu.Lock()
	s.c.refreshAt = time.Time{}
	s.c.mu.Unlock()
	f.mu.Lock()
	f.assertions = map[string]bool{}
	f.mu.Unlock()
	if _, err := s.Get(t.Context(), "", "k", ref); !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("Get with the token endpoint refusing = %v; want transient", err)
	}
	// A network failure is transient.
	f.srv.Close()
	s.c.mu.Lock()
	s.c.refreshAt = time.Now().Add(time.Hour)
	s.c.token = "kv-access-1"
	s.c.mu.Unlock()
	if _, err := s.Get(t.Context(), "", "k", ref); !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("Get with the vault down = %v; want transient", err)
	}
}

var validName = regexp.MustCompile(`^[0-9a-z-]{1,127}$`)

func TestNaming_OneDerivedNamePerOwnerAndName(t *testing.T) {
	f := newFakeKV(t)
	s := newFakeStore(t, f, func(c *Config) { c.Prefix = "Team-A" })
	cases := map[[2]string]string{
		{"", "wardyn-signing-key"}:   "team-a-platform-",
		{"", "github-app-key"}:       "team-a-operator-",
		{"alice@example.com", "pat"}: "team-a-people-",
	}
	seen := map[string]bool{}
	for r, prefix := range cases {
		stem := s.stem(r[0], r[1])
		name := s.secretName(r[0], r[1], 1<<40)
		if !strings.HasPrefix(stem, prefix) || len(stem) != len(prefix)+32 || !validName.MatchString(name) || seen[stem] {
			t.Errorf("%v: stem %q, name %q", r, stem, name)
		}
		seen[stem] = true
	}
	// Length-prefixed, so ("ab","c") and ("a","bc") never share a stem.
	if s.stem("ab", "c") == s.stem("a", "bc") {
		t.Fatal("the stem encoding is not injective")
	}
	// Golden: hex(SHA-256(u32(5)"alice" u32(3)"pat"))[:32].
	if got := s.stem("alice", "pat"); got != "team-a-people-9184cc8f77ba4afa1462251b19211298" {
		t.Fatalf("stem(alice, pat) = %s", got)
	}
}

// Rule 16: a ref the row does not derive is refused before any call.
func TestGet_RefusesARefThatIsNotDerived(t *testing.T) {
	f := newFakeKV(t)
	s := newFakeStore(t, f)
	ctx := t.Context()
	bobRef, err := s.Put(ctx, "bob", "pat", "", []byte("bob-secret"), false)
	if err != nil {
		t.Fatal(err)
	}
	host, rest, _ := strings.Cut(bobRef, "/")
	sn, _, _ := strings.Cut(rest, "#")
	calls := f.count("GET")
	for _, ref := range []string{
		bobRef,                        // another owner's value
		"other.vault.example/" + rest, // another vault
		host + "/" + sn,               // no count
		host + "/" + sn + "#0",        // a count that was never written
		host + "/" + sn + "#01",       // not canonical
		host + "/" + sn + "X#1",       // a generation that is not base36
		host + "/" + s.stem("alice", "pat") + "-g#1",
	} {
		v, err := s.Get(ctx, "alice", "pat", ref)
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("Get(alice, %q) = (%q, %v); want a refusal", ref, v, err)
		}
		if err := s.Delete(ctx, "alice", "pat", ref); err == nil {
			t.Errorf("Delete(alice, %q) accepted a ref alice's row does not derive", ref)
		}
	}
	if f.count("GET") != calls || f.count("DELETE") != 0 {
		t.Fatal("a refused ref still reached the vault")
	}
}

// The value's own tags are the second check (rule 16).
func TestGet_RefusesTagsNamingAnotherRow(t *testing.T) {
	f := newFakeKV(t)
	s := newFakeStore(t, f)
	ref, err := s.Put(t.Context(), "alice", "pat", "", []byte("v"), false)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	for _, sec := range f.secrets {
		sec.versions[len(sec.versions)-1].tags[tagOwner] = "bob"
	}
	f.mu.Unlock()
	if _, err := s.Get(t.Context(), "alice", "pat", ref); err == nil || !strings.Contains(err.Error(), "wardyn-owner") {
		t.Fatalf("Get with the tags naming bob = %v; want a refusal naming the tag", err)
	}
	if err := s.Check(t.Context(), "alice", "pat", ref); err == nil {
		t.Fatal("Check accepted tags naming another row")
	}
}

// Rule 17: gone, disabled or not Wardyn's format is a refusal, never
// ErrNotFound and never an empty value.
func TestGet_GoneDisabledOrForeignIsARefusal(t *testing.T) {
	f := newFakeKV(t)
	s := newFakeStore(t, f)
	ctx := t.Context()
	for _, c := range []struct {
		name string
		mess func(sec *kvSecret)
		want string
	}{
		{"disabled", func(sec *kvSecret) { sec.versions[len(sec.versions)-1].enabled = false }, "disabled secret"},
		{"content type", func(sec *kvSecret) { sec.versions[len(sec.versions)-1].contentType = "text/plain" }, "format"},
		{"not base64", func(sec *kvSecret) { sec.versions[len(sec.versions)-1].value = "%%%" }, "format"},
		{"gone", nil, "no longer holds"},
	} {
		ref, err := s.Put(ctx, "", c.name[:3], "", []byte("v"), false)
		if err != nil {
			t.Fatal(err)
		}
		sn, _, _, _ := s.parse("", c.name[:3], ref)
		f.mu.Lock()
		if c.mess == nil {
			delete(f.secrets, sn)
		} else {
			c.mess(f.secrets[sn])
		}
		f.mu.Unlock()
		v, err := s.Get(ctx, "", c.name[:3], ref)
		if err == nil || errors.Is(err, secretstore.ErrNotFound) || errors.Is(err, secretstore.ErrUnavailable) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Get = (%q, %v); want a definitive refusal mentioning %q", c.name, v, err, c.want)
		}
	}
}

// Key Vault cannot delete old versions: every Put leaves exactly one enabled.
func TestPut_DisablesEveryPreviousVersion(t *testing.T) {
	f := newFakeKV(t)
	f.pageSize = 2 // the version list pages
	s := newFakeStore(t, f)
	ctx := t.Context()
	ref := ""
	for i, v := range []string{"one", "two", "three", "four", "five"} {
		next, err := s.Put(ctx, "alice", "pat", ref, []byte(v), false)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(next, "#"+string(rune('1'+i))) || (ref != "" && secretstore.RefObject(next) != secretstore.RefObject(ref)) {
			t.Fatalf("Put %d: ref %q after %q; want the same name, count %d", i+1, next, ref, i+1)
		}
		ref = next
	}
	sn, _, _, _ := s.parse("alice", "pat", ref)
	if enabled, total := f.liveVersions(sn); enabled != 1 || total != 5 {
		t.Fatalf("versions: %d enabled of %d; want 1 of 5", enabled, total)
	}
	if v, err := s.Get(ctx, "alice", "pat", ref); err != nil || string(v) != "five" {
		t.Fatalf("Get = (%q, %v)", v, err)
	}
}

// At WARDYN_AZURE_KV_MAX_VERSIONS the next Put starts a new generation.
func TestPut_RollsTheGenerationAtTheCap(t *testing.T) {
	f := newFakeKV(t)
	s := newFakeStore(t, f, func(c *Config) { c.MaxVersions = 2 })
	ctx := t.Context()
	r1, _ := s.Put(ctx, "", "k", "", []byte("a"), false)
	r2, _ := s.Put(ctx, "", "k", r1, []byte("b"), false)
	r3, err := s.Put(ctx, "", "k", r2, []byte("c"), false)
	if err != nil {
		t.Fatal(err)
	}
	if secretstore.RefObject(r1) != secretstore.RefObject(r2) || secretstore.RefObject(r3) == secretstore.RefObject(r2) || !strings.HasSuffix(r3, "#1") {
		t.Fatalf("refs %q, %q, %q; want a new name at #1 after two versions", r1, r2, r3)
	}
	if v, err := s.Get(ctx, "", "k", r3); err != nil || string(v) != "c" {
		t.Fatalf("Get new generation = (%q, %v)", v, err)
	}
}

// A name a deleted secret holds (removed and re-added within the retention):
// purge and reuse it when allowed, else a new generation; never recover.
func TestPut_DeletedNameConflict(t *testing.T) {
	for _, c := range []struct {
		name      string
		forbidden bool
		purge     string
		sameName  bool
	}{
		{"purge allowed", false, PurgeAuto, true},
		{"purge forbidden", true, PurgeAuto, false},
		{"purge never", false, PurgeNever, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeKV(t)
			f.purgeForbidden = c.forbidden
			s := newFakeStore(t, f, func(cfg *Config) { cfg.Purge = c.purge })
			ctx := t.Context()
			ref, err := s.Put(ctx, "", "k", "", []byte("old"), false)
			if err != nil {
				t.Fatal(err)
			}
			sn, _, _, _ := s.parse("", "k", ref)
			// The organisation deletes it at the vault; the row still points there.
			f.mu.Lock()
			f.deleted[sn] = f.secrets[sn]
			delete(f.secrets, sn)
			f.mu.Unlock()
			next, err := s.Put(ctx, "", "k", ref, []byte("new"), false)
			if err != nil {
				t.Fatalf("Put over a deleted name = %v", err)
			}
			if (secretstore.RefObject(next) == secretstore.RefObject(ref)) != c.sameName || !strings.HasSuffix(next, "#1") {
				t.Fatalf("ref %q after %q; same name want %v", next, ref, c.sameName)
			}
			if v, err := s.Get(ctx, "", "k", next); err != nil || string(v) != "new" {
				t.Fatalf("Get = (%q, %v)", v, err)
			}
		})
	}
}

func TestPut_RefusesAValueKeyVaultCannotHold(t *testing.T) {
	s := newFakeStore(t, newFakeKV(t))
	if _, err := s.Put(t.Context(), "", "k", "", make([]byte, maxValue+1), false); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("oversized Put = %v", err)
	}
	if _, err := s.Put(t.Context(), "", "k", "", make([]byte, maxValue), false); err != nil {
		t.Fatalf("Put at the limit = %v", err)
	}
}

// The migrator's createOnly never writes into a name that already holds a
// version.
func TestPut_CreateOnlyRefusesANameThatHoldsAValue(t *testing.T) {
	f := newFakeKV(t)
	s := newFakeStore(t, f)
	fixed := time.Unix(1_900_000_000, 0)
	s.now = func() time.Time { return fixed }
	if _, err := s.Put(t.Context(), "", "k", "", []byte("other"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(t.Context(), "", "k", "", []byte("mine"), true); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("createOnly over a live name = %v; want a refusal", err)
	}
}

func TestDelete_SoftDeletesThenPurgesAndReports(t *testing.T) {
	for _, c := range []struct {
		name       string
		forbidden  bool
		purge      string
		lag        int
		wantPurged bool
	}{
		{"purged", false, PurgeAuto, 0, true},
		{"purged after the delete settles", false, PurgeAuto, 5, true},
		{"purge forbidden degrades", true, PurgeAuto, 0, false},
		{"purge never", false, PurgeNever, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeKV(t)
			f.purgeForbidden, f.deleteLag, f.retention = c.forbidden, c.lag, 30
			s := newFakeStore(t, f, func(cfg *Config) { cfg.Purge = c.purge })
			ref, err := s.Put(t.Context(), "alice", "pat", "", []byte("v"), false)
			if err != nil {
				t.Fatal(err)
			}
			ctx, rep := secretstore.WithDeleteReport(t.Context())
			if err := s.Delete(ctx, "alice", "pat", ref); err != nil {
				t.Fatalf("Delete = %v", err)
			}
			if rep.Store != Name || rep.Purged != c.wantPurged || (!c.wantPurged && rep.RecoverableDays != 30) {
				t.Fatalf("report = %+v; want purged %v, 30 recoverable days when not", *rep, c.wantPurged)
			}
			sn, _, _, _ := s.parse("alice", "pat", ref)
			f.mu.Lock()
			_, live := f.secrets[sn]
			_, soft := f.deleted[sn]
			f.mu.Unlock()
			if live || soft == c.wantPurged {
				t.Fatalf("after Delete: live %v, soft-deleted %v", live, soft)
			}
			if err := s.Delete(t.Context(), "alice", "pat", ref); err != nil {
				t.Fatalf("Delete is not idempotent: %v", err)
			}
		})
	}
}

// Walk lists this install's secrets from their tags, and never follows a
// nextLink off the vault (it would carry the bearer token).
func TestWalk_ListsThisInstallAndStaysOnTheVault(t *testing.T) {
	f := newFakeKV(t)
	f.pageSize = 1
	s := newFakeStore(t, f)
	ctx := t.Context()
	for _, r := range [][2]string{{"", "wardyn-session-key"}, {"", "op"}, {"bob", "pat"}} {
		if _, err := s.Put(ctx, r[0], r[1], "", []byte("v"), false); err != nil {
			t.Fatal(err)
		}
	}
	other := newFakeStore(t, f, func(c *Config) { c.Prefix = "ns2" })
	if _, err := other.Put(ctx, "", "op", "", []byte("v"), false); err != nil {
		t.Fatal(err)
	}
	got, err := s.Walk(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := map[[2]string]bool{}
	for _, e := range got {
		found[[2]string{e.Owner, e.Name}] = true
		if !strings.HasPrefix(e.Ref, s.c.host+"/ns1-") || strings.Contains(e.Ref, "#") {
			t.Errorf("entry ref %q", e.Ref)
		}
	}
	if len(got) != 3 || !found[[2]string{"bob", "pat"}] || !found[[2]string{"", "wardyn-session-key"}] {
		t.Fatalf("Walk = %+v; want this install's three", got)
	}

	thief := newFakeKV(t)
	f.mu.Lock()
	f.nextLinkHost = thief.srv.URL
	f.mu.Unlock()
	if _, err := s.Walk(ctx); err == nil || !strings.Contains(err.Error(), "off this vault") {
		t.Fatalf("Walk with a nextLink to another host = %v; want a refusal", err)
	}
	if thief.count("") != 0 {
		t.Fatal("the bearer token was sent to another host")
	}
}

func TestCheck_ReadsMetadataOnly(t *testing.T) {
	f := newFakeKV(t)
	s := newFakeStore(t, f)
	ctx := context.Background()
	ref, _ := s.Put(ctx, "", "k", "", []byte("v"), false)
	ref, _ = s.Put(ctx, "", "k", ref, []byte("w"), false)
	gets := f.count("GET")
	if err := s.Check(ctx, "", "k", ref); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	if f.count("GET") != gets+1 || !strings.HasSuffix(last, "/versions") {
		t.Fatalf("Check made %d GETs, last %q; want one version listing", f.count("GET")-gets, last)
	}
	sn, _, _, _ := s.parse("", "k", ref)
	f.mu.Lock()
	sec := f.secrets[sn]
	sec.versions[len(sec.versions)-1].enabled = false
	f.mu.Unlock()
	if err := s.Check(ctx, "", "k", ref); err == nil {
		t.Fatal("Check accepted a disabled latest version")
	}
}
