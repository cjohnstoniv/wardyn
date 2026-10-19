// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"net/http"
	"strings"
	"testing"
)

// A structural path segment spelled with U+017F (long s) or U+212A (Kelvin
// sign) is classified as the ASCII segment a case-insensitive router can match
// it to, never as a lower arm's fallback. A name in an id position keeps its
// own spelling.
func TestClassifyFoldsStructuralSegmentsLikeTheirASCIIForm(t *testing.T) {
	const longS, kelvin = "%C5%BF", "%E2%84%AA"
	batch := `{"data":null,"operation":"delete","packages":[{"id":"p","version":"1.0.0"}]}`
	for _, c := range []struct {
		name, method, host, ascii, folded, body string
	}{
		{"a work item delete", http.MethodDelete, "dev.azure.com", "/acme/proj/_apis/wit/workitems/5", "/acme/proj/_apis/wit/workitem" + longS + "/5", ""},
		{"an organisation-wide work resource", http.MethodPost, "dev.azure.com", "/acme/_apis/wit/fields", "/acme/_apis/wit/field" + longS, `{}`},
		{"a package batch", http.MethodPost, "pkgs.dev.azure.com", "/acme/proj/_apis/packaging/feeds/f/npm/packagesbatch", "/acme/proj/_apis/packaging/feeds/f/npm/pac" + kelvin + "agesbatch", batch},
		{"a feed admin", http.MethodPatch, "pkgs.dev.azure.com", "/acme/_apis/packaging/feeds/f", "/acme/_apis/packaging/feed" + longS + "/f", `{}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := func(path string) Request {
				r := adoReq(c.method, path, c.body)
				r.Host = c.host
				return r
			}
			want, werr := Classify(req(c.ascii))
			got, gerr := Classify(req(c.folded))
			if (werr == nil) != (gerr == nil) || got.Capability != want.Capability {
				t.Fatalf("Classify(%s) = %v, %v; want %v, %v as for %s", c.folded, got.Capability, gerr, want.Capability, werr, c.ascii)
			}
		})
	}
	r, err := parseRoute("dev.azure.com", "/acme/proj/_apis/git/repositories/Repo%C5%BF/items", "acme")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.id(3); got != strings.ToLower("Repo\u017f") {
		t.Errorf("repository id = %q, want its own spelling lowercased", got)
	}
}
