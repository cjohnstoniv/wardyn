// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adoscope

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// permittedNames are project and repository names Azure DevOps' naming rules
// allow (#485): a space and each other printable character neither list
// forbids, the apostrophe (repositories only), a literal "%", non-ASCII
// letters, and a no-break space.
var permittedNames = []string{
	"Payments Platform", "Card Auth (v2).Service", "R&D Ops!", "Bob's @Tools~1",
	"Café Équipe", "Ünïcode Repo", "100% Done", "a^b`c", "no\u00a0break", "日本語 リポ",
}

func TestEscapeName_RoundTripsEveryPermittedName(t *testing.T) {
	for _, name := range permittedNames {
		esc := EscapeName(name)
		if strings.ContainsAny(esc, " \t\u00a0") {
			t.Errorf("EscapeName(%q) = %q still holds whitespace", name, esc)
		}
		got, err := UnescapeName(esc)
		if err != nil || got != name {
			t.Errorf("UnescapeName(EscapeName(%q)) = %q, %v", name, got, err)
		}
		// Every other spelling of the name decodes to the same name.
		for _, other := range []string{url.PathEscape(name), strings.ReplaceAll(url.QueryEscape(name), "+", "%20")} {
			if got, err := UnescapeName(other); err != nil || got != name {
				t.Errorf("UnescapeName(%q) = %q, %v; want %q", other, got, err, name)
			}
		}
	}
}

// The ONE spelling escapes only what a stored field or a URL segment cannot
// carry, and leaves every character an existing row already held literal.
func TestEscapeName_Spelling(t *testing.T) {
	for name, want := range map[string]string{
		"Card Auth (v2).Service": "Card%20Auth%20(v2).Service",
		"R&D Ops!":               "R&D%20Ops!",
		"Bob's @Tools~1":         "Bob's%20@Tools~1",
		"Café":                   "Café",
		"100% Done":              "100%25%20Done",
		"a#b?c/d\\e":             "a%23b%3Fc%2Fd%5Ce",
		"no\u00a0break":          "no%C2%A0break",
		"a+b":                    "a+b",
	} {
		if got := EscapeName(name); got != want {
			t.Errorf("EscapeName(%q) = %q, want %q", name, got, want)
		}
	}
}

// "+" IS A LITERAL in path grammar — Azure DevOps routes it as "+" — so it
// never decodes to a space and a space never encodes to it.
func TestUnescapeName_PlusIsLiteral(t *testing.T) {
	if got, err := UnescapeName("a+b"); err != nil || got != "a+b" {
		t.Errorf(`UnescapeName("a+b") = %q, %v`, got, err)
	}
	if NameKey("Card+Auth") == NameKey("Card%20Auth") {
		t.Error(`"Card+Auth" and "Card%20Auth" share a key — "+" was read as a space`)
	}
}

// Every spelling that the service would route differently from how it reads is
// refused, whichever door asks.
func TestUnescapeName_RefusesStructure(t *testing.T) {
	for _, raw := range []string{
		"a%2Fb", "a%2fb", "a%5Cb", "a%252Fb", "a%25252Fb", "%2E%2E", "%2e", "..",
		"p%20", "%20p", "p.", "a%0Ab", "a%00", "a%7F", "%zz", "a%4", "a%",
	} {
		if got, err := UnescapeName(raw); err == nil {
			t.Errorf("UnescapeName(%q) = %q, want a refusal", raw, got)
		}
		if k := NameKey(raw); k != "" {
			t.Errorf("NameKey(%q) = %q, want none", raw, k)
		}
	}
}

// One repository is one key, however it was spelled or cased.
func TestNameKey_OneKeyPerName(t *testing.T) {
	want := "card auth (v2).service"
	for _, raw := range []string{"Card%20Auth%20(v2).Service", "Card%20Auth%20%28v2%29.Service", "CARD%20AUTH%20(V2).SERVICE", "Card Auth (v2).Service"} {
		if got := NameKey(raw); got != want {
			t.Errorf("NameKey(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestCanonicalRepoURL(t *testing.T) {
	const canon = "https://dev.azure.com/contoso/Payments%20Platform/_git/Card%20Auth%20(v2).Service"
	for in, want := range map[string]string{
		"https://dev.azure.com/contoso/Payments Platform/_git/Card Auth (v2).Service":           canon,
		"https://dev.azure.com/contoso/Payments%20Platform/_git/Card%20Auth%20%28v2%29.Service": canon,
		canon: canon,
		"https://contoso@dev.azure.com/contoso/Payments Platform/_git/Card Auth (v2).Service":       "https://contoso@dev.azure.com/contoso/Payments%20Platform/_git/Card%20Auth%20(v2).Service",
		"https://contoso.visualstudio.com/Payments Platform/_git/Café":                              "https://contoso.visualstudio.com/Payments%20Platform/_git/Café",
		"git@ssh.dev.azure.com:v3/contoso/Payments Platform/Card Auth (v2).Service":                 "git@ssh.dev.azure.com:v3/contoso/Payments%20Platform/Card%20Auth%20(v2).Service",
		"ssh://git@ssh.dev.azure.com/v3/contoso/Payments%20Platform/Card%20Auth%20%28v2%29.Service": "ssh://git@ssh.dev.azure.com/v3/contoso/Payments%20Platform/Card%20Auth%20(v2).Service",
		"https://tfs.corp.example/tfs/DefaultCollection/Payments Platform/_git/app":                 "https://tfs.corp.example/tfs/DefaultCollection/Payments%20Platform/_git/app",
		"https://dev.azure.com/contoso/100%25%20Done/_git/app":                                      "https://dev.azure.com/contoso/100%25%20Done/_git/app",
	} {
		got, ok := CanonicalRepoURL(in)
		if !ok || got != want {
			t.Errorf("CanonicalRepoURL(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"https://github.com/acme/app",                    // another forge: untouched
		"https://git.corp.example/group/app",             // no _git: not an Azure DevOps address
		"acme/app",                                       // a bare slug
		"https://dev.azure.com/contoso/p/_git/r?path=/x", // a query
		"https://dev.azure.com/contoso/a%2Fb/_git/r",     // an encoded separator
		"https://dev.azure.com/contoso/p/_git/a%252Fb",   // a doubly-encoded one
		"https://dev.azure.com/contoso/100% Done/_git/r", // a bare "%" is not an escape
		" https://dev.azure.com/contoso/p/_git/r",        // not a URL as written
	} {
		if got, ok := CanonicalRepoURL(in); ok {
			t.Errorf("CanonicalRepoURL(%q) = %q, want not canonicalised", in, got)
		}
	}
}

// THE GATE reads the same names: a spaced project and repository classify on
// their route like any other, and an encoded separator in either is refused.
func TestClassify_SpacedNames(t *testing.T) {
	runCases(t, []caseT{
		{name: "read of a spaced repository", req: adoReq(http.MethodGet,
			"/acme/Payments%20Platform/_apis/git/repositories/Card%20Auth%20(v2).Service/items", ""), want: CapRead},
		{name: "read with every character escaped", req: adoReq(http.MethodGet,
			"/acme/Payments%20Platform/_apis/git/repositories/Card%20Auth%20%28v2%29.Service/items", ""), want: CapRead},
		{name: "a repository named like a resource is still positional", req: adoReq(http.MethodDelete,
			"/acme/Payments%20Platform/_apis/git/repositories/items%20x", ""), want: CapRepoAdmin},
		{name: "an encoded separator in the project", req: adoReq(http.MethodGet,
			"/acme/Payments%2FPlatform/_apis/git/repositories/r/items", ""), wantErr: true},
		{name: "a control character in the repository", req: adoReq(http.MethodGet,
			"/acme/p/_apis/git/repositories/r%0A/items", ""), wantErr: true},
	})
}
