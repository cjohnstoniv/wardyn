// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/testutil"
)

// branchesFake is a stand-in api.github.com for ListBranches: n branches named
// b000..., served 100 a page with a Link header, and a record of the token
// request and the hand-back.
type branchesFake struct {
	n         int
	status    int  // non-zero: every repository read answers it
	noInstall bool // the installation lookup names no installation (id 0)

	mu        sync.Mutex
	perms     string
	revoked   bool
	listAuths []string
}

func (f *branchesFake) serve(t *testing.T) *githubMinter {
	t.Helper()
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/installation/token"):
			f.revoked = true
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/installation") && f.noInstall:
			_, _ = w.Write([]byte(`{"id": 0}`))
		case strings.HasSuffix(r.URL.Path, "/installation"):
			_, _ = w.Write([]byte(`{"id": 42}`))
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			b, _ := io.ReadAll(r.Body)
			f.perms = string(b)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_branches","expires_at":"` + testutil.FutureRFC3339(24) + `"}`))
		case f.status != 0:
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"message":"refused"}`))
		case strings.HasSuffix(r.URL.Path, "/repos/acme/widgets"):
			_, _ = w.Write([]byte(`{"name":"widgets","default_branch":"trunk"}`))
		case strings.HasSuffix(r.URL.Path, "/repos/acme/widgets/branches"):
			f.listAuths = append(f.listAuths, r.Header.Get("Authorization"))
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			page = max(page, 1)
			var names []string
			for i := (page - 1) * 100; i < min(page*100, f.n); i++ {
				names = append(names, fmt.Sprintf(`{"name":"b%03d"}`, i))
			}
			if page*100 < f.n {
				w.Header().Set("Link", fmt.Sprintf(`<%srepos/acme/widgets/branches?per_page=100&page=%d>; rel="next"`, base, page+1))
			}
			_, _ = w.Write([]byte("[" + strings.Join(names, ",") + "]"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	base = srv.URL + "/"
	return newMinterAgainst(t, base)
}

// The token is one repository, contents:read and nothing more, it is the one
// the listing reads with, and it is handed back afterwards.
func TestListBranches_OneRepoContentsReadAndRevoked(t *testing.T) {
	f := &branchesFake{n: 3}
	m := f.serve(t)
	names, def, truncated, err := m.ListBranches(context.Background(), "acme/widgets", 200)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if strings.Join(names, ",") != "b000,b001,b002" || def != "trunk" || truncated {
		t.Fatalf("got %v default %q truncated %v", names, def, truncated)
	}
	if !strings.Contains(f.perms, `"permissions":{"contents":"read"}`) || !strings.Contains(f.perms, `"repositories":["widgets"]`) {
		t.Fatalf("token request = %s, want one repository with contents:read only", f.perms)
	}
	if len(f.listAuths) != 1 || !strings.Contains(f.listAuths[0], "ghs_branches") {
		t.Fatalf("branch reads carried %v, want the minted token", f.listAuths)
	}
	if !f.revoked {
		t.Fatal("the listing token was not handed back")
	}
}

// 250 branches: 200 come back, and truncated says there are more, after two
// pages and no third.
func TestListBranches_TruncatesAtLimit(t *testing.T) {
	f := &branchesFake{n: 250}
	m := f.serve(t)
	names, _, truncated, err := m.ListBranches(context.Background(), "acme/widgets", 200)
	if err != nil {
		t.Fatalf("ListBranches: %v", err)
	}
	if len(names) != 200 || !truncated || names[199] != "b199" {
		t.Fatalf("got %d names (last %q), truncated %v; want 200 and truncated", len(names), names[len(names)-1], truncated)
	}
	if len(f.listAuths) != 2 {
		t.Fatalf("read %d pages, want 2", len(f.listAuths))
	}
}

// A GitHub refusal is ErrGitHubRefused, so the API answers forge_refused
// rather than forge_unreachable; the token is still handed back.
func TestListBranches_RefusalIsErrGitHubRefused(t *testing.T) {
	f := &branchesFake{status: http.StatusNotFound}
	m := f.serve(t)
	_, _, _, err := m.ListBranches(context.Background(), "acme/widgets", 200)
	if !errors.Is(err, ErrGitHubRefused) {
		t.Fatalf("err = %v, want ErrGitHubRefused", err)
	}
	if !f.revoked {
		t.Fatal("the listing token was not handed back after a refusal")
	}
}

// An App with no installation on the repository's owner is a refusal too
// (forge_refused), not an outage; nothing is minted, so nothing is revoked.
func TestListBranches_NoInstallationIsErrGitHubRefused(t *testing.T) {
	f := &branchesFake{noInstall: true}
	m := f.serve(t)
	_, _, _, err := m.ListBranches(context.Background(), "acme/widgets", 200)
	if !errors.Is(err, ErrGitHubRefused) {
		t.Fatalf("err = %v, want ErrGitHubRefused", err)
	}
	if f.perms != "" {
		t.Fatalf("a token was requested (%s) with no installation", f.perms)
	}
}
