// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// A package batch is classified by its operation: promote, list and deprecate
// are packaging_write; delete, permanentDelete, unpublish and restoreToFeed are
// packaging_manage; anything unreadable, unknown or ambiguous is refused.
func TestClassifyPackageBatchByOperation(t *testing.T) {
	const nuget = "/acme/proj/_apis/packaging/feeds/f/nuget/packagesbatch"
	batch := func(path, body string) Request { return onHost("pkgs.dev.azure.com", http.MethodPost, path, body) }
	op := func(name string) string {
		return `{"data":null,"operation":"` + name + `","packages":[{"id":"p","version":"1.0.0"}]}`
	}
	oversized := batch(nuget, op("promote"))
	oversized.Header = hdr("Content-Length", "999999")
	runCases(t, []caseT{
		{name: "nuget promote", req: batch(nuget, op("promote")), want: CapPackagingWrite},
		{name: "nuget list", req: batch(nuget, op("list")), want: CapPackagingWrite},
		{name: "nuget delete", req: batch(nuget, op("delete")), want: CapPackagingManage},
		{name: "nuget permanentDelete", req: batch(nuget, op("permanentDelete")), want: CapPackagingManage},
		{name: "nuget restoreToFeed", req: batch(nuget, op("restoreToFeed")), want: CapPackagingManage},
		{name: "an operation in upper case", req: batch(nuget, op("DELETE")), want: CapPackagingManage},
		{name: "npm deprecate", req: batch("/acme/proj/_apis/packaging/feeds/f/npm/packagesbatch", op("deprecate")), want: CapPackagingWrite},
		{name: "npm unpublish", req: batch("/acme/proj/_apis/packaging/feeds/f/npm/packagesbatch", op("unpublish")), want: CapPackagingManage},
		{name: "an org-level feed", req: batch("/acme/_apis/packaging/feeds/f/pypi/packagesbatch", op("delete")), want: CapPackagingManage},
		{name: "the recycle bin form", req: batch("/acme/proj/_apis/packaging/feeds/f/nuget/RecycleBin/PackagesBatch", op("restoreToFeed")), want: CapPackagingManage},
		{name: "the recycle bin form, permanentDelete", req: batch("/acme/proj/_apis/packaging/feeds/f/maven/recyclebin/packagesbatch", op("permanentDelete")), want: CapPackagingManage},

		{name: "an unknown operation", req: batch(nuget, op("frobnicate")), wantErr: true},
		{name: "no operation", req: batch(nuget, `{"packages":[]}`), wantErr: true},
		{name: "a numeric operation", req: batch(nuget, `{"operation":2}`), wantErr: true},
		{name: "a repeated operation key", req: batch(nuget, `{"operation":"promote","operation":"delete"}`), wantErr: true},
		{name: "a repeated operation key in another case", req: batch(nuget, `{"operation":"promote","Operation":"delete"}`), wantErr: true},
		{name: "a body that is not JSON", req: batch(nuget, `operation=delete`), wantErr: true},
		{name: "no body", req: batch(nuget, ""), wantErr: true},
		{name: "a body longer than the peek", req: oversized, wantErr: true},
		{name: "an encoded body", req: withHeaders(batch(nuget, op("promote")), "Content-Encoding", "gzip"), wantErr: true},

		// Positional: only the batch route reads the body.
		{name: "a package named packagesbatch", req: onHost("pkgs.dev.azure.com", http.MethodPatch, "/acme/_apis/packaging/feeds/f/npm/packagesbatch/versions/1", `{}`), want: CapPackagingWrite},
		{name: "a package version PATCH", req: onHost("pkgs.dev.azure.com", http.MethodPatch, "/acme/_apis/packaging/feeds/f/nuget/packages/p/versions/1", `{}`), want: CapPackagingWrite},
		{name: "a package client publish", req: onHost("pkgs.dev.azure.com", http.MethodPut, "/acme/_packaging/feed/nuget/v2", `{}`), want: CapPackagingWrite},
	})

	withheld := batch(nuget, "")
	withheld.BodyWithheld = true
	if _, err := Classify(withheld); !errors.Is(err, ErrNeedsBody) {
		t.Fatalf("a withheld batch body = %v, want ErrNeedsBody", err)
	}
	withheld.BodyWithheld, withheld.BodyPeek = false, []byte(op("delete"))
	if v, err := Classify(withheld); err != nil || v.Capability != CapPackagingManage {
		t.Fatalf("the retry with the body = %v, %v", v.Capability, err)
	}
	if _, err := Classify(batch(nuget, `{"operation":"`+strings.Repeat("x", 64)+`"}`)); err == nil {
		t.Fatal("a long unknown operation was classified")
	}
}

func withHeaders(r Request, kv ...string) Request {
	r.Header = hdr(kv...)
	return r
}
