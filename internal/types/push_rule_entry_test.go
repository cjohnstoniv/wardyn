// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestPushRuleKeyMatchesTheSharedTable holds the key to the table the console's
// pushRuleKey is held to: canonical spellings meet one key, and what cannot name
// an entry is an error, in both languages.
func TestPushRuleKeyMatchesTheSharedTable(t *testing.T) {
	raw, err := os.ReadFile("../../ui/src/app/lib/push-rule-key.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name, Provider, Host, Org, Key string
		Error                          bool
	}
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) < 15 {
		t.Fatalf("golden table: %v (%d cases)", err, len(cases))
	}
	for _, tc := range cases {
		key, err := PushRuleKey(tc.Provider, tc.Host, tc.Org)
		if (err != nil) != tc.Error || key != tc.Key {
			t.Errorf("%s: key %q err %v, want %q error=%v", tc.Name, key, err, tc.Key, tc.Error)
		}
	}
}

func TestPushRuleEntryOfRemote(t *testing.T) {
	for _, tc := range []struct {
		name, provider, remote string
		host, org              string
		err                    bool
	}{
		{"github https", "github", "https://github.com/Acme/widgets.git", "github.com", "acme", false},
		{"github scp-like ssh", "github", "git@github.com:acme/widgets.git", "github.com", "acme", false},
		{"github ssh url", "github", "ssh://git@github.com/acme/widgets", "github.com", "acme", false},
		{"GHES keeps its host", "github", "https://ghes.corp.example/acme/widgets", "ghes.corp.example", "acme", false},
		{"ado https", "azure_devops", "https://dev.azure.com/contoso/proj/_git/repo", "dev.azure.com", "contoso", false},
		{"ado https with a user prefix", "azure_devops", "https://contoso@dev.azure.com/contoso/proj/_git/repo", "dev.azure.com", "contoso", false},
		{"ado visualstudio alias", "azure_devops", "https://contoso.visualstudio.com/proj/_git/repo", "dev.azure.com", "contoso", false},
		{"ado ssh", "azure_devops", "git@ssh.dev.azure.com:v3/contoso/proj/repo", "dev.azure.com", "contoso", false},
		{"ado legacy ssh", "azure_devops", "contoso@vs-ssh.visualstudio.com:v3/contoso/proj/repo", "dev.azure.com", "contoso", false},
		{"ado server collection", "azure_devops", "https://tfs.corp.example/DefaultCollection/proj/_git/repo", "tfs.corp.example", "defaultcollection", false},
		{"no path", "github", "https://github.com/", "", "", true},
		{"neither url nor scp", "github", "not a remote", "", "", true},
		{"an owner that is not canonical", "github", "https://github.com/ac%20me/repo", "", "", true},
		{"legacy ssh without v3", "azure_devops", "git@vs-ssh.visualstudio.com:contoso/repo", "", "", true},
		// C-B1 / F2: segments git and curl would resolve are refused, in every spelling.
		{"dot-dot segment (url)", "github", "https://github.com/lax/../strict/repo.git", "", "", true},
		{"dot-dot segment (scp)", "github", "git@github.com:lax/../strict/repo.git", "", "", true},
		{"dot segment", "github", "https://github.com/./lax/repo", "", "", true},
		{"empty segment (url)", "github", "https://github.com//evil/repo", "", "", true},
		{"empty segment (scp)", "github", "git@github.com:acme//repo", "", "", true},
		{"empty segment (scp, doubled leading slash)", "github", "git@github.com://evil/repo", "", "", true},
		{"encoded dot", "github", "https://github.com/%2e/lax/repo", "", "", true},
		{"encoded dot, upper", "github", "https://github.com/lax/%2E/repo", "", "", true},
		{"encoded dot-dot", "github", "https://github.com/lax/%2e%2e/strict/repo", "", "", true},
		{"half-encoded dot-dot", "github", "git@github.com:lax/.%2e/strict/repo", "", "", true},
		{"encoded slash", "github", "https://github.com/lax%2fstrict/repo", "", "", true},
		{"encoded slash in the repo", "github", "https://github.com/lax/re%2fpo", "", "", true},
		{"ado dot-dot", "azure_devops", "https://dev.azure.com/lax/../strict/_git/repo", "", "", true},
		{"one leading and one trailing slash are the path's own", "github", "https://github.com/acme/repo/", "github.com", "acme", false},
		{"scp-like absolute path", "github", "git@github.com:/acme/repo.git", "github.com", "acme", false},
		// C-R3-1: git sends an ssh:// or git:// path literally, "?" and "#" included.
		{"query hides a dot-dot (ssh)", "github", "ssh://git@github.com/lax?/../strict/repo.git", "", "", true},
		{"fragment hides a dot-dot (ssh)", "github", "ssh://git@github.com/lax#/../strict/repo.git", "", "", true},
		{"query hides a dot-dot (ssh with a port)", "github", "ssh://git@github.com:22/lax?/../strict/repo.git", "", "", true},
		{"fragment hides a dot-dot (git)", "github", "git://github.com/lax#/../strict/repo.git", "", "", true},
		{"query hides a dot-dot (ado ssh)", "azure_devops", "ssh://git@ssh.dev.azure.com/v3/lax?/../strict/p/r", "", "", true},
		{"query on https", "github", "https://github.com/acme/repo?x=1", "", "", true},
		{"empty query on https", "github", "https://github.com/acme/repo?", "", "", true},
		// C-F1: v3 only on the SSH hosts; scp host is split at the first ':'; server collections.
		{"v3 on the https host is an organisation named v3", "azure_devops", "https://dev.azure.com/v3/proj/_git/repo", "dev.azure.com", "v3", false},
		{"v3 on the https legacy host is not folded", "azure_devops", "https://contoso.visualstudio.com/v3/proj/_git/repo", "dev.azure.com", "contoso", false},
		{"second host smuggled into an scp path", "github", "git@github.com:x@evil.example:lax/repo", "", "", true},
		{"colon in an scp path", "github", "git@github.com:lax:x/repo", "", "", true},
		{"colon later in an scp path", "github", "git@github.com:acme/re:po", "", "", true},
		{"user with an at sign still splits at the first colon", "github", "u@x@github.com:acme/repo", "github.com", "acme", false},
		{"server under a prefix without a base path", "azure_devops", "https://tfs.corp.example/tfs/DefaultCollection/proj/_git/repo", "", "", true},
		{"server project-less collection without a base path", "azure_devops", "https://tfs.corp.example/Coll/_git/repo", "", "", true},
		{"server without _git", "azure_devops", "https://tfs.corp.example/DefaultCollection/proj", "", "", true},
		// C-F2: non-ASCII hosts are refused before folding.
		{"dotted capital I host (url)", "github", "https://gİthub.com/acme/repo", "", "", true},
		{"dotted capital I host (scp)", "github", "git@gİthub.com:acme/repo", "", "", true},
		{"fullwidth host", "github", "https://ｇｉｔｈｕｂ.com/acme/repo", "", "", true},
		// C-F3: ssh. is folded for the two public SSH hosts only.
		{"ssh.github.com", "github", "git@ssh.github.com:acme/repo", "github.com", "acme", false},
		{"ssh. on a server is its own host", "github", "git@ssh.ghes.example:acme/repo", "ssh.ghes.example", "acme", false},
		// F1: the legacy hosts fold to dev.azure.com.
		{"legacy https subdomain, upper case", "azure_devops", "https://Contoso.VisualStudio.com/DefaultCollection/proj/_git/repo", "dev.azure.com", "contoso", false},
		{"ado v3 on a non-ssh azure host", "azure_devops", "git@dev.azure.com:v3/contoso/proj/repo", "dev.azure.com", "v3", false},
	} {
		host, org, err := PushRuleEntryOfRemote(tc.provider, tc.remote)
		if (err != nil) != tc.err || host != tc.host || org != tc.org {
			t.Errorf("%s: = %q %q %v, want %q %q error=%v", tc.name, host, org, err, tc.host, tc.org, tc.err)
		}
	}
	// C-F1: an Azure DevOps Server under a prefix is read with the provider row's base path, and each collection is its own entry.
	for _, tc := range []struct {
		name, base, remote string
		org                string
		err                bool
	}{
		{"collection under /tfs", "/tfs", "https://tfs.corp.example/tfs/DefaultCollection/proj/_git/repo", "defaultcollection", false},
		{"another collection under /tfs", "tfs/", "https://tfs.corp.example/TFS/Other/proj/_git/repo", "other", false},
		{"project-less collection under /tfs", "/tfs", "https://tfs.corp.example/tfs/Coll/_git/repo", "coll", false},
		{"no base path, no prefix", "", "https://tfs.corp.example/Coll/proj/_git/repo", "coll", false},
		{"remote outside the base path", "/tfs", "https://tfs.corp.example/other/Coll/proj/_git/repo", "", true},
		{"base path with a remote that has none", "/tfs", "https://tfs.corp.example/Coll/proj/_git/repo", "", true},
		{"no base path and a prefix", "", "https://tfs.corp.example/tfs/Coll/proj/_git/repo", "", true},
	} {
		host, org, err := PushRuleEntryOfRemoteAt("azure_devops", tc.remote, tc.base)
		if (err != nil) != tc.err || org != tc.org || (!tc.err && host != "tfs.corp.example") {
			t.Errorf("%s: = %q %q %v, want org %q error=%v", tc.name, host, org, err, tc.org, tc.err)
		}
	}
	if _, _, err := PushRuleEntryOfRemoteAt("azure_devops", "ssh://tfs.corp/tfs/Coll/Proj/_git/r?/../../../Other/P/_git/r", "/tfs"); err == nil {
		t.Error("a query hiding dot segments under a base path must be an error")
	}
	tfs := []PushRuleSet{{Provider: "azure_devops", Host: "tfs.corp.example", Org: "Coll", Rules: PushRulesSpec{DenyPaths: []string{"a/"}}}}
	if _, found, err := PushRulesForRemoteAt(tfs, "azure_devops", "https://tfs.corp.example/tfs/Coll/p/_git/r", "/tfs"); err != nil || !found {
		t.Errorf("the collection's set was not found: %v %v", found, err)
	}
	if _, found, err := PushRulesForRemoteAt(tfs, "azure_devops", "https://tfs.corp.example/tfs/Other/p/_git/r", "/tfs"); err != nil || found {
		t.Errorf("another collection borrowed the set: %v %v", found, err)
	}
	if _, _, err := PushRulesForRemote(tfs, "azure_devops", "https://tfs.corp.example/tfs/Coll/p/_git/r"); err == nil {
		t.Error("a server remote under a prefix with no base path must be an error")
	}
	// F1: a rule authored for the legacy host keys the same as the remote derivation.
	legacy := []PushRuleSet{{Provider: "azure_devops", Host: "contoso.visualstudio.com", Org: "contoso", Rules: PushRulesSpec{DenyPaths: []string{"a/"}}}}
	if _, found, err := PushRulesForRemote(legacy, "azure_devops", "https://contoso.visualstudio.com/proj/_git/repo"); err != nil || !found {
		t.Errorf("a set authored for contoso.visualstudio.com was not found: %v %v", found, err)
	}
	// Two spellings of one remote meet the one set; another forge's same-named org does not.
	sets := []PushRuleSet{{Provider: "github", Org: "Acme", Rules: PushRulesSpec{DenyPaths: []string{"a/"}}}}
	for _, remote := range []string{"https://GitHub.com/ACME/x.git", "git@github.com:acme/x.git"} {
		if spec, found, err := PushRulesForRemote(sets, "github", remote); err != nil || !found || len(spec.DenyPaths) != 1 {
			t.Errorf("%s: found %v %v %v, want the acme set", remote, found, spec, err)
		}
	}
	if _, found, err := PushRulesForRemote(sets, "github", "https://ghes.corp.example/acme/x"); err != nil || found {
		t.Errorf("a GHES acme borrowed github.com's set: found %v err %v", found, err)
	}
	if _, _, err := PushRulesForRemote(sets, "github", "https://github.com/"); err == nil || !strings.Contains(err.Error(), "no host or path") {
		t.Errorf("an underivable remote must be an error the caller refuses, got %v", err)
	}
}
