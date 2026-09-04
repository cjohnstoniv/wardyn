// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// TestSDKCensusNamesEveryRouteFamily is the COMPLEMENT guard for the public
// SDK's route census (pkg/client/client.go's "# Coverage" block).
//
// docs/sdk.md calls that block "the exact list of what it wraps and what it does
// not", and pkg/client's own two tests pin it in both directions against the
// SDK's METHOD SET — doc->method and method->doc. Neither can see a route family
// the SDK never wrapped, so the not-covered half named three items while seven
// whole families 0.7 added (drives, governance, permissions, access, tokens,
// integrations, base-images) appeared in neither half. "Exact" was guarded
// against the wrong set.
//
// The registry is routeMatrix, not a second chi.Walk: TestAuthzMatrix already
// fails when routeMatrix and the live router disagree in either direction, so
// this reads an already-pinned truth instead of re-deriving it (and cannot pass
// by walking a differently-configured server, which is how /api/v1/secrets and
// /api/v1/sessions go missing from a bare New(Config{})).
func TestSDKCensusNamesEveryRouteFamily(t *testing.T) {
	raw, err := os.ReadFile("../../pkg/client/client.go")
	if err != nil {
		t.Fatalf("read pkg/client/client.go: %v", err)
	}
	doc := string(raw)
	start := strings.Index(doc, "// # Coverage")
	end := strings.Index(doc, "// # Pagination")
	if start < 0 || end < 0 || end <= start {
		t.Fatal(`pkg/client/client.go has no "# Coverage" ... "# Pagination" package-doc block — this guard reads the census between them`)
	}
	census := doc[start:end]

	// Every route family the router registers, as the string the census must
	// name it by: the /api/v1 prefix plus its first segment, or the exact path
	// for the handful mounted at the top level.
	//
	// The SPA root is the one family with no stable literal — it is "GET /"
	// without a UIDir and "GET /*" with one — so it is named by a sentence the
	// census carries instead.
	want := map[string]string{} // literal the census must contain -> an example route
	for route := range routeMatrix {
		_, path, ok := strings.Cut(route, " ")
		if !ok {
			t.Fatalf("routeMatrix key %q is not \"METHOD /path\"", route)
		}
		switch {
		case path == "/" || path == "/*":
			want["the console SPA at /"] = path
		case strings.HasPrefix(path, "/api/v1/"):
			seg, _, _ := strings.Cut(strings.TrimPrefix(path, "/api/v1/"), "/")
			want["/api/v1/"+seg] = path
		default:
			want[path] = path
		}
	}
	if len(want) < 20 {
		t.Fatalf("derived only %d route families from routeMatrix (%d routes) — the parser, not the census, is what changed", len(want), len(routeMatrix))
	}

	var missing []string
	for lit, example := range want {
		if !strings.Contains(census, lit) {
			missing = append(missing, lit+" (e.g. "+example+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("pkg/client/client.go's route census names neither wraps-it nor does-not-wrap-it for %d registered route famil(ies):\n  %s\n"+
			"docs/sdk.md calls that block \"the exact list of what it wraps and what it does not\" — add each to the covered list "+
			"(with its methods) or to the NOT-covered census below it",
			len(missing), strings.Join(missing, "\n  "))
	}
}
