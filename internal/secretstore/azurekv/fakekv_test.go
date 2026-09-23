// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package azurekv

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeKV is an in-process Azure Key Vault (Secrets data plane) plus the two
// token endpoints wardynd uses, faithful to the documented REST behaviour the
// store relies on: set with versions, get latest or by version (403
// SecretDisabled on a disabled one), update attributes, versions and secrets
// lists with absolute nextLink paging, soft delete (with an asynchronous
// "being deleted" window), the deleted-name 409 on set, purge with a 403
// variant, names that are case-insensitive and [0-9a-zA-Z-]{1,127}, the
// api-version query, bearer tokens, forced status codes, and the Entra v2
// token endpoint (federated client assertion) and IMDS. A call to recover
// fails the test: Wardyn never recovers a deleted secret.
type fakeKV struct {
	t   *testing.T
	srv *httptest.Server

	mu              sync.Mutex
	secrets         map[string]*kvSecret // live, by lowercase name
	deleted         map[string]*kvSecret // soft-deleted
	deleting        map[string]int       // name -> calls left before the delete shows up
	deleteLag       int                  // lag given to each new soft delete
	retention       int                  // recoverableDays
	purgeForbidden  bool
	tokens          map[string]bool
	assertions      map[string]bool // federated tokens the tenant accepts
	tenant, client  string
	expiresIn       int
	exchanges       int
	lastAssertion   string
	imdsCalls       int
	imdsHeader      string
	imdsClientID    string
	force           []int // statuses to answer vault calls with, one per call
	calls           []string
	pageSize        int
	nextLinkHost    string // non-empty: nextLinks point here instead
	nextToken, tick int
}

type kvSecret struct {
	versions []*kvVersion // creation order
}

type kvVersion struct {
	id          string
	value       string
	contentType string
	tags        map[string]string
	enabled     bool
	created     int64
}

var kvNameRE = regexp.MustCompile(`^[0-9a-zA-Z-]{1,127}$`)

func newFakeKV(t *testing.T) *fakeKV {
	f := &fakeKV{
		t: t, secrets: map[string]*kvSecret{}, deleted: map[string]*kvSecret{}, deleting: map[string]int{},
		retention: 90, tokens: map[string]bool{}, assertions: map[string]bool{"projected-sa-1": true},
		tenant: "tenant-1", client: "client-1", expiresIn: 3600, pageSize: 25,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func kvReply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

func kvFail(w http.ResponseWriter, status int, code, inner, msg string) {
	e := map[string]any{"code": code, "message": msg}
	if inner != "" {
		e["innererror"] = map[string]any{"code": inner}
	}
	kvReply(w, status, map[string]any{"error": e})
}

func (f *fakeKV) count(prefix string) int {
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

func (f *fakeKV) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/"+f.tenant+"/oauth2/v2.0/token":
		f.exchange(w, r)
		return
	case r.URL.Path == "/metadata/identity/oauth2/token":
		f.imds(w, r)
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if len(f.force) > 0 {
		code := f.force[0]
		f.force = f.force[1:]
		kvFail(w, code, "Forced", "", "forced")
		return
	}
	if r.URL.Query().Get("api-version") != apiVersion {
		kvFail(w, http.StatusBadRequest, "BadParameter", "", "missing or unsupported api-version")
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !f.tokens[tok] {
		w.Header().Set("WWW-Authenticate", `Bearer authorization="https://login.example/`+f.tenant+`", resource="https://vault.azure.net"`)
		kvFail(w, http.StatusUnauthorized, "Unauthorized", "", "AKV10000: Request is missing a Bearer or PoP token.")
		return
	}
	for k := range f.deleting { // an asynchronous delete makes progress per call
		if f.deleting[k]--; f.deleting[k] <= 0 {
			delete(f.deleting, k)
		}
	}
	seg := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(seg) == 1 && seg[0] == "secrets" && r.Method == http.MethodGet:
		f.list(w, r)
	case len(seg) >= 2 && seg[0] == "secrets":
		name := strings.ToLower(seg[1])
		if !kvNameRE.MatchString(seg[1]) {
			kvFail(w, http.StatusBadRequest, "BadParameter", "", "invalid secret name")
			return
		}
		f.secret(w, r, name, seg[2:])
	case len(seg) == 2 && seg[0] == "deletedsecrets":
		f.deletedSecret(w, r, strings.ToLower(seg[1]))
	case len(seg) == 3 && seg[0] == "deletedsecrets" && seg[2] == "recover":
		f.t.Errorf("wardynd called %s %s: it never recovers a deleted secret", r.Method, r.URL.Path)
		kvFail(w, http.StatusForbidden, "Forbidden", "", "not allowed")
	default:
		kvFail(w, http.StatusNotFound, "NotFound", "", "no route")
	}
}

func (f *fakeKV) exchange(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		kvReply(w, 400, map[string]any{"error": "invalid_request"})
		return
	}
	f.exchanges++
	f.lastAssertion = r.PostForm.Get("client_assertion")
	if r.PostForm.Get("grant_type") != "client_credentials" ||
		r.PostForm.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" ||
		r.PostForm.Get("scope") != "https://vault.azure.net/.default" || r.PostForm.Get("client_id") != f.client {
		kvReply(w, 400, map[string]any{"error": "invalid_request", "error_description": "malformed exchange"})
		return
	}
	if !f.assertions[f.lastAssertion] {
		kvReply(w, 401, map[string]any{"error": "invalid_client", "error_description": "no federated credential matches the assertion\ntrace id: x"})
		return
	}
	kvReply(w, 200, map[string]any{"token_type": "Bearer", "expires_in": f.expiresIn, "access_token": f.issue()})
}

func (f *fakeKV) imds(w http.ResponseWriter, r *http.Request) {
	f.imdsCalls++
	f.imdsHeader = r.Header.Get("Metadata")
	f.imdsClientID = r.URL.Query().Get("client_id")
	if f.imdsHeader != "true" || r.URL.Query().Get("resource") != "https://vault.azure.net" || r.URL.Query().Get("api-version") != "2018-02-01" {
		kvReply(w, 400, map[string]any{"error": "invalid_request"})
		return
	}
	// IMDS answers expires_in as a string.
	kvReply(w, 200, map[string]any{"access_token": f.issue(), "expires_in": strconv.Itoa(f.expiresIn), "resource": "https://vault.azure.net", "token_type": "Bearer"})
}

func (f *fakeKV) issue() string {
	f.nextToken++
	tok := "kv-access-" + strconv.Itoa(f.nextToken)
	f.tokens[tok] = true
	return tok
}

func (f *fakeKV) id(parts ...string) string {
	return f.srv.URL + "/" + strings.Join(parts, "/")
}

func (f *fakeKV) item(name string, v *kvVersion, withVersion bool) map[string]any {
	id := f.id("secrets", name)
	if withVersion {
		id = f.id("secrets", name, v.id)
	}
	m := map[string]any{"id": id, "attributes": map[string]any{
		"enabled": v.enabled, "created": v.created, "updated": v.created,
		"recoverableDays": f.retention, "recoveryLevel": "Recoverable+Purgeable",
	}}
	if v.contentType != "" {
		m["contentType"] = v.contentType
	}
	if len(v.tags) > 0 {
		m["tags"] = v.tags
	}
	return m
}

func (f *fakeKV) secret(w http.ResponseWriter, r *http.Request, name string, rest []string) {
	s := f.secrets[name]
	switch {
	case r.Method == http.MethodPut && len(rest) == 0:
		f.set(w, r, name)
	case r.Method == http.MethodGet && len(rest) == 1 && rest[0] == "versions":
		if s == nil {
			kvFail(w, 404, "SecretNotFound", "", "A secret with (name/id) "+name+" was not found in this key vault.")
			return
		}
		items := make([]map[string]any, 0, len(s.versions))
		for _, v := range s.versions {
			items = append(items, f.item(name, v, true))
		}
		f.page(w, r, items)
	case r.Method == http.MethodGet && (len(rest) == 0 || len(rest) == 1):
		if s == nil {
			kvFail(w, 404, "SecretNotFound", "", "A secret with (name/id) "+name+" was not found in this key vault.")
			return
		}
		v := s.versions[len(s.versions)-1]
		if len(rest) == 1 && rest[0] != "" {
			if v = s.version(rest[0]); v == nil {
				kvFail(w, 404, "SecretNotFound", "", "no such version")
				return
			}
		}
		if !v.enabled {
			kvFail(w, 403, "Forbidden", "SecretDisabled", "Operation get is not allowed on a disabled secret.")
			return
		}
		m := f.item(name, v, true)
		m["value"] = v.value
		kvReply(w, 200, m)
	case r.Method == http.MethodPatch && len(rest) == 1:
		var v *kvVersion
		if s != nil {
			v = s.version(rest[0])
		}
		if v == nil {
			kvFail(w, 404, "SecretNotFound", "", "no such version")
			return
		}
		var body struct {
			Attributes struct {
				Enabled *bool `json:"enabled"`
			} `json:"attributes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Attributes.Enabled != nil {
			v.enabled = *body.Attributes.Enabled
		}
		kvReply(w, 200, f.item(name, v, true))
	case r.Method == http.MethodDelete && len(rest) == 0:
		if s == nil {
			kvFail(w, 404, "SecretNotFound", "", "A secret with (name/id) "+name+" was not found in this key vault.")
			return
		}
		delete(f.secrets, name)
		f.deleted[name] = s
		if f.deleteLag > 0 {
			f.deleting[name] = f.deleteLag
		}
		m := f.item(name, s.versions[len(s.versions)-1], false)
		m["recoveryId"] = f.id("deletedsecrets", name)
		kvReply(w, 200, m)
	default:
		kvFail(w, 405, "MethodNotAllowed", "", r.Method)
	}
}

func (s *kvSecret) version(id string) *kvVersion {
	for _, v := range s.versions {
		if v.id == id {
			return v
		}
	}
	return nil
}

func (f *fakeKV) set(w http.ResponseWriter, r *http.Request, name string) {
	if _, ok := f.deleted[name]; ok {
		kvFail(w, 409, "Conflict", "ObjectIsDeletedButRecoverable", "Secret "+name+" is currently in a deleted but recoverable state, and its name cannot be reused; in this state, the secret can only be recovered or purged.")
		return
	}
	var body struct {
		Value       string            `json:"value"`
		ContentType string            `json:"contentType"`
		Tags        map[string]string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Value) > 25*1024 || len(body.Tags) > 15 {
		kvFail(w, 400, "BadParameter", "", "bad secret")
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	f.tick++
	v := &kvVersion{id: hex.EncodeToString(b), value: body.Value, contentType: body.ContentType, tags: body.Tags, enabled: true, created: time.Now().Unix()*1000 + int64(f.tick)}
	s := f.secrets[name]
	if s == nil {
		s = &kvSecret{}
		f.secrets[name] = s
	}
	s.versions = append(s.versions, v)
	m := f.item(name, v, true)
	m["value"] = v.value
	kvReply(w, 200, m)
}

func (f *fakeKV) deletedSecret(w http.ResponseWriter, r *http.Request, name string) {
	s, ok := f.deleted[name]
	if !ok || f.deleting[name] > 0 {
		if ok && r.Method == http.MethodDelete && f.deleting[name]%2 == 1 {
			kvFail(w, 409, "Conflict", "", "Secret "+name+" is currently being deleted.")
			return
		}
		kvFail(w, 404, "SecretNotFound", "", "Deleted Secret not found: "+name)
		return
	}
	switch r.Method {
	case http.MethodGet:
		kvReply(w, 200, f.item(name, s.versions[len(s.versions)-1], false))
	case http.MethodDelete:
		if f.purgeForbidden {
			kvFail(w, 403, "Forbidden", "ForbiddenByPolicy", "The user, group or application does not have secrets purge permission on this key vault.")
			return
		}
		delete(f.deleted, name)
		w.WriteHeader(http.StatusNoContent)
	default:
		kvFail(w, 405, "MethodNotAllowed", "", r.Method)
	}
}

// list answers GET /secrets: one item per live secret, carrying its current
// version's attributes and tags, and no value.
func (f *fakeKV) list(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(f.secrets))
	for n := range f.secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]map[string]any, 0, len(names))
	for _, n := range names {
		s := f.secrets[n]
		items = append(items, f.item(n, s.versions[len(s.versions)-1], false))
	}
	f.page(w, r, items)
}

// page answers one page of items, with an absolute nextLink carrying the
// api-version and a skip token, as Key Vault does.
func (f *fakeKV) page(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	size := f.pageSize
	if m, err := strconv.Atoi(r.URL.Query().Get("maxresults")); err == nil && m < size {
		size = m
	}
	skip, _ := strconv.Atoi(r.URL.Query().Get("$skiptoken"))
	end := min(skip+size, len(items))
	out := map[string]any{"value": items[skip:end]}
	if end < len(items) {
		host := f.srv.URL
		if f.nextLinkHost != "" {
			host = f.nextLinkHost
		}
		out["nextLink"] = fmt.Sprintf("%s%s?api-version=%s&maxresults=%d&$skiptoken=%d", host, r.URL.Path, apiVersion, size, end)
	}
	kvReply(w, 200, out)
}

// liveVersions returns name's enabled version count and total, or -1, -1.
func (f *fakeKV) liveVersions(name string) (enabled, total int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.secrets[strings.ToLower(name)]
	if s == nil {
		return -1, -1
	}
	for _, v := range s.versions {
		if v.enabled {
			enabled++
		}
	}
	return enabled, len(s.versions)
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func fakeConfig(f *fakeKV, tokenFile string) Config {
	return Config{
		VaultURL: f.srv.URL, Auth: AuthWorkloadIdentity, TenantID: f.tenant, ClientID: f.client,
		FederatedTokenFile: tokenFile, AuthorityHost: f.srv.URL, Prefix: "ns1", MaxVersions: 100, Purge: PurgeAuto,
	}
}

// newFakeStore returns a Store with a workload-identity token from f; edit
// the config with opts before it is built.
func newFakeStore(t *testing.T, f *fakeKV, opts ...func(*Config)) *Store {
	t.Helper()
	cfg := fakeConfig(f, writeFile(t, "projected-sa-1\n"))
	for _, o := range opts {
		o(&cfg)
	}
	s, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.c.backoff = 0
	s.pollEvery = time.Millisecond
	return s
}
