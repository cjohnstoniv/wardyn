// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package armscope

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

const (
	pinned = "1111aaaa-2222-3333-4444-5555bbbbcccc"
	other  = "99999999-8888-7777-6666-555555555555"
	keys   = "/subscriptions/" + pinned + "/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts/acct/listKeys"
)

func scope(write bool) Scope { return Scope{Subscriptions: []string{pinned}, Write: write} }

// A row that says read gets GET and HEAD only: the POST that returns a storage
// account's keys is refused with every other change, and admitted under write.
func TestARMScope_ReadRefusesPost(t *testing.T) {
	group := "/subscriptions/" + pinned + "/resourceGroups/rg"
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, keys},
		{http.MethodPut, group},
		{http.MethodPatch, group},
		{http.MethodDelete, group},
	} {
		if err := scope(false).Admit(tc.method, tc.path, nil); !errors.Is(err, ErrReadOnly) {
			t.Errorf("read: %s %s = %v, want ErrReadOnly", tc.method, tc.path, err)
		}
		if err := scope(true).Admit(tc.method, tc.path, nil); err != nil {
			t.Errorf("write: %s %s = %v, want admitted", tc.method, tc.path, err)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		if err := scope(false).Admit(method, group, nil); err != nil {
			t.Errorf("read: %s = %v, want admitted", method, err)
		}
	}
}

// Every path is under one of the row's subscriptions or is one of the three
// tenant-level reads, whatever the row's access.
func TestARMScope_SubscriptionPin(t *testing.T) {
	for _, write := range []bool{false, true} {
		s := scope(write)
		for _, path := range []string{
			"/subscriptions/" + pinned,
			"/subscriptions/" + pinned + "/resourcegroups",
			"/SUBSCRIPTIONS/" + strings.ToUpper(pinned) + "/resourceGroups/rg/",
			"/subscriptions", "/providers", "/tenants", "/Subscriptions/",
		} {
			if err := s.Admit(http.MethodGet, path, nil); err != nil {
				t.Errorf("write=%v GET %s = %v, want admitted", write, path, err)
			}
		}
		for _, path := range []string{
			"/subscriptions/" + other,
			"/subscriptions/" + other + "/resourcegroups",
			"/subscriptions/" + pinned + "x/resourcegroups",
			"/", "/batch", "/providers/Microsoft.Management/managementGroups",
			"/providers/Microsoft.ResourceGraph/resources", "/tenants/x", "/subscriptions/",
			"/resourceGroups/" + pinned,
		} {
			if path == "/subscriptions/" {
				continue // the tenant-level list, with its tolerated trailing slash
			}
			if err := s.Admit(http.MethodGet, path, nil); !errors.Is(err, ErrOutside) {
				t.Errorf("write=%v GET %s = %v, want ErrOutside", write, path, err)
			}
		}
		// A change outside any subscription is outside, never merely read-only.
		for _, path := range []string{"/subscriptions", "/providers", "/batch", "/subscriptions/" + other + "/resourceGroups/rg"} {
			if err := s.Admit(http.MethodPost, path, nil); !errors.Is(err, ErrOutside) {
				t.Errorf("write=%v POST %s = %v, want ErrOutside", write, path, err)
			}
		}
	}
	if err := (Scope{Write: true}).Admit(http.MethodGet, "/subscriptions/"+pinned, nil); !errors.Is(err, ErrOutside) {
		t.Errorf("a scope with no subscriptions admitted one: %v", err)
	}
}

// A request whose method or path Azure might read differently is refused
// before the pin is asked.
func TestARMScope_UnreadableIsRefused(t *testing.T) {
	base := "/subscriptions/" + pinned
	for _, path := range []string{
		"", "subscriptions/" + pinned, base + "//resourcegroups", "//subscriptions/" + pinned,
		base + "/../" + other, base + "/%2e%2e/" + other, base + "/%252e%252e/" + other,
		base + "/a%2Fb", base + "/a%5Cb", base + `\resourcegroups`, base + "/a%zz", base + "/rg%00",
		base + ";x/resourcegroups", base + "/..;/" + other, base + "?api-version=1", base + "#x",
		// The same marks percent-encoded: a server that decodes, drops ";…" and
		// then collapses ".." would route these into the other subscription.
		base + "/..%3b/" + other, base + "/..%3B/" + other, base + "/.%3b/" + other, base + "/rg%3bx=1",
		"/subscriptions%3bx/" + pinned, "/subscriptions/" + pinned + "%3b/resourcegroups",
		base + "/..%253b/" + other, base + "/rg%2541", base + "/rg%25", base + "/rg%3Fx", base + "/rg%23x",
		base + "/%2E%2E/" + other, base + "/rg%0a", base + "/rg%7f", base + "/%20rg", base + "/rg.",
		// A dot or lookalike with no letter or digit: fullwidth dots, ".." with a zero-width character, "..+".
		base + "/%EF%BC%8E%EF%BC%8E/" + other, base + "/..%E2%80%8B/" + other, base + "/..+/" + other, base + "/%E2%80%A4/" + other,
	} {
		if err := scope(true).Admit(http.MethodGet, path, nil); !errors.Is(err, ErrUnreadable) {
			t.Errorf("GET %q = %v, want ErrUnreadable", path, err)
		}
	}
	for _, method := range []string{"", "get", http.MethodOptions, http.MethodTrace, http.MethodConnect, "PROPFIND"} {
		if err := scope(true).Admit(method, base, nil); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%q = %v, want ErrUnreadable", method, err)
		}
	}
	for _, name := range []string{
		"X-HTTP-Method-Override", "x-http-method", "X-Method-Override", "X_HTTP_Method_Override", "x_http_method",
		"X-Ms-Http-Method", "X_MS_HTTP_METHOD", "X-Http-Method_Override", "X-MS-Method-Override",
	} {
		if err := scope(false).Admit(http.MethodGet, base, http.Header{name: {"DELETE"}}); !errors.Is(err, ErrUnreadable) {
			t.Errorf("%s = %v, want ErrUnreadable", name, err)
		}
	}
	// Headers the CLI really sends are not overrides.
	sent := http.Header{"X-Ms-Client-Request-Id": {"1"}, "Commandname": {"group list"}, "Access-Control-Request-Method": {"GET"}, "Accept": {"application/json"}}
	if err := scope(false).Admit(http.MethodGet, base, sent); err != nil {
		t.Errorf("ordinary headers = %v, want admitted", err)
	}
	// A name with an encoded space or parenthesis is one segment that reads as itself.
	for _, path := range []string{base + "/resourceGroups/my%20rg", base + "/resourceGroups/rg%28a%29"} {
		if err := scope(false).Admit(http.MethodGet, path, nil); err != nil {
			t.Errorf("GET %q = %v, want admitted", path, err)
		}
	}
}

func TestSubscription(t *testing.T) {
	if got, ok := Subscription(strings.ToUpper(pinned)); !ok || got != pinned {
		t.Errorf("uppercase = %q, %v; want %q", got, ok, pinned)
	}
	for _, bad := range []string{"", "nope", pinned + "0", pinned[:35] + "g", strings.ReplaceAll(pinned, "-", "0"), "{" + pinned[2:] + "}"} {
		if got, ok := Subscription(bad); ok {
			t.Errorf("Subscription(%q) = %q, want refused", bad, got)
		}
	}
}
