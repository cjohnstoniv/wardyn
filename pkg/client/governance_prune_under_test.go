// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// pruneUnderFake is sectionsFake with every write's path and query recorded, ids as unit names.
func pruneUnderFake(t *testing.T) (*sectionsFake, *client.Client, func() []string) {
	t.Helper()
	f := &sectionsFake{rules: map[uuid.UUID][]client.DeciderRule{}, held: map[uuid.UUID]bool{}, refuse: map[uuid.UUID][2]string{}}
	var mu sync.Mutex
	var writes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mu.Lock()
			writes = append(writes, r.Method+" "+r.URL.RequestURI())
			mu.Unlock()
		}
		f.handler(t)(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, newTestClient(srv), func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(writes))
		for i, w := range writes {
			out[i] = f.label(w)
		}
		return out
	}
}

// Every prune write of an --under apply names the subtree's root as under, so the server can refuse a
// write whose unit was moved out after the SDK's last read; an apply without --under names none.
func TestPruneUnderNamesTheRootOnEveryWrite(t *testing.T) {
	f, c, writes := pruneUnderFake(t)
	_, platform, _, _ := platformTree(f)
	doc := client.GovernanceDocument{Units: []client.GovernanceUnit{platform}, Memberships: []client.GovernanceMembership{},
		Deciders: map[string][]client.DeciderRule{}}
	if _, err := c.ApplyGovernanceResultUnder(context.Background(), doc, "platform", true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"DELETE /api/v1/governance/units/<platform>/admins/<admin pat@corp.com>?under=<platform>",
		"PUT /api/v1/governance/units/<team>/deciders?under=<platform>",
		"DELETE /api/v1/governance/memberships/<ann@corp.com>?under=<platform>",
		"DELETE /api/v1/governance/units/<team>?under=<platform>",
	}
	if got := writes(); !slices.Equal(got, want) {
		t.Errorf("prune under platform wrote\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}

	f, c, writes = pruneUnderFake(t)
	def, platform, _, other := platformTree(f)
	doc = client.GovernanceDocument{Units: []client.GovernanceUnit{def, platform, other}, Memberships: []client.GovernanceMembership{},
		Deciders: map[string][]client.DeciderRule{}}
	if _, err := c.ApplyGovernanceResult(context.Background(), doc, true); err != nil {
		t.Fatal(err)
	}
	if got := writes(); len(got) == 0 || slices.ContainsFunc(got, func(w string) bool { return strings.Contains(w, "?") }) {
		t.Errorf("a prune without --under wrote %v, want deletes with no query", got)
	}
}
