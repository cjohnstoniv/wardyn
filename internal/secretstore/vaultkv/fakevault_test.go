// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeVault is an in-process Vault: the KV v2 data/ and metadata/ routes
// (custom_metadata, cas, cas_required, max_versions, LIST), Kubernetes login,
// token lookup-self and renew-self, a namespace check, and forced status
// codes. A call to destroy/ or undelete/ fails the test: the policy wardynd
// is documented to need has no such stanza.
type fakeVault struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	mount     string
	namespace string            // required X-Vault-Namespace, "" = none
	tokens    map[string]bool   // live tokens
	jwts      map[string]string // projected SA token -> role it may log in as
	// platformRole, when set, is the documented two-role policy: that role's
	// token reaches only <prefix>/platform/, every other role's token
	// everything but it. The same projected token logs in as either role.
	platformRole string
	tokenRole    map[string]string
	ttl          int // lease/ttl handed out, seconds
	casRequired  bool
	revoked      bool // the policy is gone: every data/ and metadata/ call is denied
	kv           map[string]*kvEntry
	force        []int // statuses to answer with, one per request, before anything else
	calls        []string
	logins       int
	renews       int
	nextToken    int
}

type kvEntry struct {
	current     int
	maxVersions int
	custom      map[string]string
	versions    map[int]map[string]string // nil value = deleted
}

func newFakeVault(t *testing.T) *fakeVault {
	f := &fakeVault{t: t, mount: "wardyn", tokens: map[string]bool{}, jwts: map[string]string{}, tokenRole: map[string]string{}, kv: map[string]*kvEntry{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVault) issue() string {
	f.nextToken++
	tok := "tok-" + strconv.Itoa(f.nextToken)
	f.tokens[tok] = true
	return tok
}

func (f *fakeVault) callCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func fail(w http.ResponseWriter, status int, msg string) {
	reply(w, status, map[string]any{"errors": []string{msg}})
}

func (f *fakeVault) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1/")
	f.calls = append(f.calls, r.Method+" "+path)
	if len(f.force) > 0 {
		code := f.force[0]
		f.force = f.force[1:]
		fail(w, code, "forced")
		return
	}
	if r.Header.Get("X-Vault-Namespace") != f.namespace {
		fail(w, http.StatusForbidden, "wrong namespace")
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	if strings.HasPrefix(path, "auth/kubernetes/login") {
		f.login(w, body)
		return
	}
	if !f.tokens[r.Header.Get("X-Vault-Token")] {
		fail(w, http.StatusForbidden, "permission denied")
		return
	}
	switch {
	case path == "auth/token/lookup-self":
		reply(w, 200, map[string]any{"data": map[string]any{"ttl": f.ttl, "renewable": f.ttl > 0}})
	case path == "auth/token/renew-self":
		f.renews++
		reply(w, 200, map[string]any{"auth": map[string]any{"client_token": r.Header.Get("X-Vault-Token"), "lease_duration": f.ttl, "renewable": true}})
	case strings.HasPrefix(path, f.mount+"/destroy/"), strings.HasPrefix(path, f.mount+"/undelete/"):
		f.t.Errorf("wardynd called %s %s: the documented policy has no destroy/ or undelete/ stanza", r.Method, path)
		fail(w, http.StatusForbidden, "permission denied")
	case f.revoked && (strings.HasPrefix(path, f.mount+"/data/") || strings.HasPrefix(path, f.mount+"/metadata/")):
		fail(w, http.StatusForbidden, "permission denied")
	case f.platformRole != "" && (strings.HasPrefix(path, f.mount+"/data/") || strings.HasPrefix(path, f.mount+"/metadata/")) &&
		strings.Contains(path, "/platform/") != (f.tokenRole[r.Header.Get("X-Vault-Token")] == f.platformRole):
		fail(w, http.StatusForbidden, "permission denied")
	case strings.HasPrefix(path, f.mount+"/data/"):
		f.data(w, r.Method, strings.TrimPrefix(path, f.mount+"/data/"), body)
	case strings.HasPrefix(path, f.mount+"/metadata/"):
		f.metadata(w, r, strings.TrimPrefix(path, f.mount+"/metadata/"), body)
	default:
		fail(w, http.StatusNotFound, "no handler for route")
	}
}

func (f *fakeVault) login(w http.ResponseWriter, body map[string]any) {
	role, _ := body["role"].(string)
	jwt, _ := body["jwt"].(string)
	if want, ok := f.jwts[jwt]; !ok || (want != role && (f.platformRole == "" || role != f.platformRole)) {
		fail(w, http.StatusForbidden, "permission denied")
		return
	}
	f.logins++
	tok := f.issue()
	f.tokenRole[tok] = role
	reply(w, 200, map[string]any{"auth": map[string]any{"client_token": tok, "lease_duration": f.ttl, "renewable": f.ttl > 0,
		"metadata": map[string]string{"role": role}}})
}

func (f *fakeVault) data(w http.ResponseWriter, method, p string, body map[string]any) {
	e := f.kv[p]
	switch method {
	case http.MethodGet:
		if e == nil || e.versions[e.current] == nil {
			fail(w, http.StatusNotFound, "")
			return
		}
		reply(w, 200, map[string]any{"data": map[string]any{
			"data":     e.versions[e.current],
			"metadata": map[string]any{"version": e.current, "custom_metadata": e.custom},
		}})
	case http.MethodPost:
		opts, _ := body["options"].(map[string]any)
		cas, hasCAS := opts["cas"].(float64)
		if e == nil {
			e = &kvEntry{versions: map[int]map[string]string{}, maxVersions: 0}
		}
		if (f.casRequired || hasCAS) && (!hasCAS || int(cas) != e.current) {
			fail(w, http.StatusBadRequest, "check-and-set parameter did not match the current version")
			return
		}
		d := map[string]string{}
		for k, v := range body["data"].(map[string]any) {
			d[k] = v.(string)
		}
		e.current++
		e.versions[e.current] = d
		if e.maxVersions > 0 {
			for v := range e.versions {
				if v <= e.current-e.maxVersions {
					delete(e.versions, v)
				}
			}
		}
		f.kv[p] = e
		reply(w, 200, map[string]any{"data": map[string]any{"version": e.current}})
	case http.MethodDelete:
		if e != nil {
			e.versions[e.current] = nil
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeVault) metadata(w http.ResponseWriter, r *http.Request, p string, body map[string]any) {
	if r.URL.Query().Get("list") == "true" {
		f.list(w, p)
		return
	}
	e := f.kv[p]
	switch r.Method {
	case http.MethodGet:
		if e == nil {
			fail(w, http.StatusNotFound, "")
			return
		}
		versions := map[string]any{}
		for v, d := range e.versions {
			dt := ""
			if d == nil {
				dt = "2026-01-01T00:00:00Z"
			}
			versions[strconv.Itoa(v)] = map[string]any{"deletion_time": dt, "destroyed": false}
		}
		reply(w, 200, map[string]any{"data": map[string]any{"current_version": e.current, "max_versions": e.maxVersions,
			"cas_required": f.casRequired, "custom_metadata": e.custom, "versions": versions}})
	case http.MethodPost:
		cm, hasCM := body["custom_metadata"].(map[string]any)
		for k, v := range cm {
			if v.(string) == "" { // as Vault: 0 < len(value) <= 512
				fail(w, http.StatusBadRequest, "custom_metadata validation failed: length of value for key "+k+" is 0")
				return
			}
		}
		if e == nil {
			e = &kvEntry{versions: map[int]map[string]string{}}
			f.kv[p] = e
		}
		if mv, ok := body["max_versions"].(float64); ok {
			e.maxVersions = int(mv)
		}
		if hasCM {
			e.custom = map[string]string{}
			for k, v := range cm {
				e.custom[k] = v.(string)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		delete(f.kv, p)
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fakeVault) list(w http.ResponseWriter, dir string) {
	seen := map[string]bool{}
	for p := range f.kv {
		rest, ok := strings.CutPrefix(p, dir)
		if !ok {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i+1]
		}
		seen[rest] = true
	}
	if len(seen) == 0 {
		fail(w, http.StatusNotFound, "")
		return
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	reply(w, 200, map[string]any{"data": map[string]any{"keys": keys}})
}

// writeFile writes content to a fresh file in a temp dir and returns its path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// newFakeStore returns a Store logged in to f with Kubernetes auth.
func newFakeStore(t *testing.T, f *fakeVault) *Store {
	t.Helper()
	f.mu.Lock()
	f.jwts["sa-jwt"] = "wardyn"
	f.mu.Unlock()
	s, err := New(t.Context(), Config{
		Addr: f.srv.URL, Namespace: f.namespace, Auth: AuthKubernetes, AuthMount: "kubernetes", Role: "wardyn",
		K8sTokenFile: writeFile(t, "sa-jwt\n"), Mount: f.mount, Prefix: "ns1", MaxVersions: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.c.backoff = 0
	return s
}
