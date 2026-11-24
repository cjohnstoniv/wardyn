// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The Azure DevOps arm reads the default branch's root, then only the blobs the fixed list names, in the list's
// order: a folder of the same name, a nested path and a name off the list are never read, and a file past the cap
// is named as too large with no text.
func TestAssistantADORootFiles(t *testing.T) {
	var asked []string
	get := func(_ context.Context, target, _ string, out any) string {
		asked = append(asked, target)
		body := `{"defaultBranch":"refs/heads/trunk"}`
		if strings.Contains(target, "/items?") {
			body = `{"value":[
				{"path":"/","isFolder":true,"gitObjectType":"tree"},
				{"path":"/package.json","gitObjectType":"blob"},
				{"path":"/go.mod","gitObjectType":"blob"},
				{"path":"/Dockerfile","isFolder":true,"gitObjectType":"tree"},
				{"path":"/.npmrc","gitObjectType":"blob"},
				{"path":"/src/package.json","gitObjectType":"blob"}]}`
		}
		_ = json.Unmarshal([]byte(body), out)
		return ""
	}
	getFile := func(_ context.Context, target, _ string, out any) string {
		asked = append(asked, target)
		text := `{"content":"module widgets\n"}`
		if strings.Contains(target, "package.json") {
			text = `{"content":"` + strings.Repeat("a", 20) + `"}`
		}
		_ = json.Unmarshal([]byte(text), out)
		return ""
	}
	branch, files, reason := adoReadRootFiles(context.Background(), get, getFile, "https://dev.azure.com/contoso/proj/_apis/git/repositories/app", "x",
		[]string{"Dockerfile", "package.json", "go.mod"}, 16) // go.mod's 15 bytes fit, package.json's 20 do not
	if reason != "" || branch != "trunk" || len(files) != 2 {
		t.Fatalf("read = %q %v %q, want trunk and two files", branch, files, reason)
	}
	if files[0].Name != "package.json" || !files[0].TooLarge || files[0].Text != "" {
		t.Errorf("package.json = %+v, want too large and no text", files[0])
	}
	if files[1].Name != "go.mod" || files[1].Text != "module widgets\n" || files[1].TooLarge {
		t.Errorf("go.mod = %+v, want its text", files[1])
	}
	for _, target := range asked {
		if strings.Contains(target, ".npmrc") || strings.Contains(target, "src%2Fpackage") || strings.Contains(target, "Dockerfile") {
			t.Errorf("asked for %s, which is not a listed blob at the root", target)
		}
	}

	failing := func(context.Context, string, string, any) string { return branchReasonRefused }
	if _, files, reason := adoReadRootFiles(context.Background(), failing, getFile, "u", "x", []string{"go.mod"}, 10); reason != branchReasonRefused || files != nil {
		t.Errorf("a refused default-branch read = %v %q, want forge_refused and no files", files, reason)
	}
}
