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
		{"github https", "github", "https://github.com/Acme/widgets.git", "github.com", "Acme", false},
		{"github scp-like ssh", "github", "git@github.com:acme/widgets.git", "github.com", "acme", false},
		{"github ssh url", "github", "ssh://git@github.com/acme/widgets", "github.com", "acme", false},
		{"GHES keeps its host", "github", "https://ghes.corp.example/acme/widgets", "ghes.corp.example", "acme", false},
		{"ado https", "azure_devops", "https://dev.azure.com/contoso/proj/_git/repo", "dev.azure.com", "contoso", false},
		{"ado https with a user prefix", "azure_devops", "https://contoso@dev.azure.com/contoso/proj/_git/repo", "dev.azure.com", "contoso", false},
		{"ado visualstudio alias", "azure_devops", "https://contoso.visualstudio.com/proj/_git/repo", "dev.azure.com", "contoso", false},
		{"ado ssh", "azure_devops", "git@ssh.dev.azure.com:v3/contoso/proj/repo", "dev.azure.com", "contoso", false},
		{"ado legacy ssh", "azure_devops", "contoso@vs-ssh.visualstudio.com:v3/contoso/proj/repo", "dev.azure.com", "contoso", false},
		{"ado server collection", "azure_devops", "https://tfs.corp.example/DefaultCollection/proj/_git/repo", "tfs.corp.example", "DefaultCollection", false},
		{"no path", "github", "https://github.com/", "", "", true},
		{"neither url nor scp", "github", "not a remote", "", "", true},
		{"an owner that is not canonical", "github", "https://github.com/ac%20me/repo", "", "", true},
		{"legacy ssh without v3", "azure_devops", "git@vs-ssh.visualstudio.com:contoso/repo", "", "", true},
	} {
		host, org, err := PushRuleEntryOfRemote(tc.provider, tc.remote)
		if (err != nil) != tc.err || host != tc.host || org != tc.org {
			t.Errorf("%s: = %q %q %v, want %q %q error=%v", tc.name, host, org, err, tc.host, tc.org, tc.err)
		}
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
