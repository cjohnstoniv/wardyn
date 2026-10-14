// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

// An Azure DevOps Server remote is read from "_git": the project is the segment
// before it, and exactly one collection segment may sit between the base path
// and the project when the base path names no collection, none when it does.
func TestPushRuleEntryOfServerRemote(t *testing.T) {
	const server = "https://tfs.corp.example/"
	for _, tc := range []struct {
		name, remote, base string
		want               string // host/org, "" for a refusal
	}{
		{"host-only row, collection and project", server + "Coll/Proj/_git/app", "", "tfs.corp.example/coll"},
		{"prefix row, collection and project", server + "tfs/Coll/Proj/_git/app", "/tfs", "tfs.corp.example/coll"},
		{"collection-scoped row, project only", server + "tfs/Coll/Proj/_git/app", "/tfs/Coll", "tfs.corp.example/coll"},
		{"collection-only row, project only", server + "Coll/Proj/_git/app", "/Coll", "tfs.corp.example/coll"},
		{"the verb after the repository is not a segment of it", server + "tfs/Coll/Proj/_git/app/git-receive-pack", "/tfs", "tfs.corp.example/coll"},
		{"base path case is not significant", server + "TFS/Coll/Proj/_git/app", "/tfs", "tfs.corp.example/coll"},

		{"a collection name with a space", server + "tfs/Fabrikam%20Collection/Proj/_git/app", "/tfs", "tfs.corp.example/fabrikam collection"},
		{"a collection name with a letter outside ASCII", server + "tfs/%C3%89quipe/Proj/_git/app", "/tfs", "tfs.corp.example/équipe"},
		{"a collection-scoped row names its collection as written", server + "tfs/Fabrikam%20Collection/Proj/_git/app", "/tfs/Fabrikam Collection", "tfs.corp.example/fabrikam collection"},

		{"a collection with an encoded separator", server + "tfs/Fabrikam%5CCollection/Proj/_git/app", "/tfs", ""},
		{"a collection-scoped row, a remote in another collection", server + "tfs/Other%20Collection/Proj/_git/app", "/tfs/Fabrikam Collection", ""},
		{"host-only row, a prefix segment too many", server + "tfs/Coll/Proj/_git/app", "", ""},
		{"prefix row, one segment too many", server + "tfs/Coll/Extra/Proj/_git/app", "/tfs", ""},
		{"collection-scoped row, a collection segment too many", server + "tfs/Coll/Other/Proj/_git/app", "/tfs/Coll", ""},
		{"host-only row, no collection", server + "Proj/_git/app", "", ""},
		{"no project before _git", server + "_git/app", "", ""},
		{"no repository after _git", server + "Coll/Proj/_git", "", ""},
		{"outside the base path", server + "other/Coll/Proj/_git/app", "/tfs", ""},
		{"an empty segment", server + "Coll//Proj/_git/app", "", ""},
	} {
		host, org, err := PushRuleEntryOfServerRemote(string(GitProviderAzureDevOps), tc.remote, tc.base)
		got := ""
		if err == nil {
			got = host + "/" + org
		}
		if got != tc.want {
			t.Errorf("%s: got %q (%v), want %q", tc.name, got, err, tc.want)
		}
	}
	// Everything but an Azure DevOps Server remote is the older derivation's.
	for _, tc := range []struct{ provider, remote, want string }{
		{"github", "https://github.com/Acme/api.git", "github.com/acme"},
		{"azure_devops", "https://dev.azure.com/acme/proj/_git/app", "dev.azure.com/acme"},
		{"azure_devops", "https://acme.visualstudio.com/proj/_git/app", "dev.azure.com/acme"},
	} {
		host, org, err := PushRuleEntryOfServerRemote(tc.provider, tc.remote, "")
		if got := host + "/" + org; err != nil || got != tc.want {
			t.Errorf("%s: got %q (%v), want %q", tc.remote, got, err, tc.want)
		}
	}
}
