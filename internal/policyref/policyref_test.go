// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package policyref

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	longHost := "https://" + strings.Repeat("a", 64) + ".example.com/" + strings.Repeat("p", 2048)
	tests := []struct {
		name string
		c    Contact
		ok   bool
	}{
		{"empty", Contact{}, true},
		{"full", Contact{Owner: "Platform security", Email: "sec.team+a@example.com", RequestURL: "https://help.example.com/access?team=a&x=1", RequestText: "Ask in the queue."}, true},
		{"mailto url", Contact{RequestURL: "mailto:sec@example.com"}, true},
		{"mixed case domain", Contact{Email: "Sec@Example.com"}, true},

		{"javascript url", Contact{RequestURL: "javascript:alert(1)"}, false},
		{"http url", Contact{RequestURL: "http://help.example.com"}, false},
		{"data url", Contact{RequestURL: "data:text/html,x"}, false},
		{"userinfo", Contact{RequestURL: "https://user:pw@help.example.com/"}, false},
		{"userinfo lookalike", Contact{RequestURL: "https://help.example.com@evil.example.net/"}, false},
		{"non-ascii url", Contact{RequestURL: "https://help.example.com/ä"}, false},
		{"space in url", Contact{RequestURL: "https://help.example.com/a b"}, false},
		{"bare host", Contact{RequestURL: "https://localhost/"}, false},
		{"mailto two addresses", Contact{RequestURL: "mailto:a@example.com,b@example.com"}, false},
		{"mailto query", Contact{RequestURL: "mailto:a@example.com?cc=evil@example.com"}, false},
		{"mailto fragment", Contact{RequestURL: "mailto:a@example.com#x"}, false},
		{"mailto empty", Contact{RequestURL: "mailto:"}, false},
		{"url over cap", Contact{RequestURL: longHost}, false},

		{"display name email", Contact{Email: "Ann <ann@example.com>"}, false},
		{"query email", Contact{Email: "x?cc=evil@example.com"}, false},
		{"fragment email", Contact{Email: "x#frag@example.com"}, false},
		{"ampersand email", Contact{Email: "a&b@example.com"}, false},
		{"quoted email", Contact{Email: `"a b"@example.com`}, false},
		{"two emails", Contact{Email: "a@example.com, b@example.com"}, false},
		{"no domain dot", Contact{Email: "a@localhost"}, false},
		{"email over cap", Contact{Email: strings.Repeat("a", 250) + "@example.com"}, false},

		{"owner bidi", Contact{Owner: "Team \u202eevil"}, false},
		{"owner newline", Contact{Owner: "Team\nB"}, false},
		{"owner over cap", Contact{Owner: strings.Repeat("o", 201)}, false},
		{"owner at cap", Contact{Owner: strings.Repeat("ö", 200)}, true},
		{"text bidi", Contact{RequestText: "ask \u202e"}, false},
		{"text newline", Contact{RequestText: "a\r\nb"}, false},
		{"text over cap", Contact{RequestText: strings.Repeat("t", 1001)}, false},
		{"text at cap", Contact{RequestText: strings.Repeat("ö", 1000)}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.c); (err == nil) != tc.ok {
				t.Errorf("Validate(%+v) = %v, want ok=%v", tc.c, err, tc.ok)
			}
		})
	}
}

func TestProjectDropsOnlyTheBadFields(t *testing.T) {
	got := Project(SourceProfile, "Team A", &Contact{
		Owner:       "Platform",
		Email:       "x?cc=evil@example.com",
		RequestURL:  "javascript:alert(1)",
		RequestText: "ask\nnow",
	})
	want := Ref{Source: SourceProfile, Name: "Team A", Owner: "Platform"}
	if got == nil || *got != want {
		t.Fatalf("Project = %+v, want %+v", got, want)
	}
	good := Contact{Owner: "O", Email: "a@example.com", RequestURL: "https://h.example.com/x", RequestText: "t"}
	if r := Project(SourceDeployment, "ignored", &good); r == nil || r.Name != "" || r.Email != good.Email || r.RequestURL != good.RequestURL {
		t.Fatalf("deployment projection = %+v", r)
	}
	if r := Project(SourceDeployment, "", nil); r == nil || *r != (Ref{Source: SourceDeployment}) {
		t.Fatalf("nil contact projection = %+v", r)
	}
	if Project("other", "n", &good) != nil {
		t.Fatal("unknown source must project to nil")
	}
}

func TestRequestRoute(t *testing.T) {
	tests := []struct {
		name string
		ref  Ref
		want string
	}{
		{"url wins", Ref{RequestURL: "https://h.example.com/a", Email: "a@example.com"}, "https://h.example.com/a"},
		{"email fallback", Ref{Email: "a@example.com"}, "mailto:a@example.com"},
		{"bad url falls to email", Ref{RequestURL: "http://h.example.com", Email: "a@example.com"}, "mailto:a@example.com"},
		{"hand-built bad email", Ref{Email: "x?cc=evil@example.com"}, ""},
		{"hand-built bad email and url", Ref{RequestURL: "javascript:x", Email: "a&b@example.com"}, ""},
		{"nothing", Ref{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.RequestRoute(); got != tc.want {
				t.Errorf("RequestRoute = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHeaderValues(t *testing.T) {
	p, r := Ref{Source: SourceProfile, Name: `Équipe "A"`, Email: "a@example.com"}.HeaderValues()
	if p != `profile; name="%C3%89quipe%20%22A%22"` || r != "mailto:a@example.com" {
		t.Errorf("profile headers = %q, %q", p, r)
	}
	if p, r := (Ref{Source: SourceDeployment}).HeaderValues(); p != "deployment" || r != "" {
		t.Errorf("deployment headers = %q, %q", p, r)
	}
}

func TestFields(t *testing.T) {
	if got := (Contact{Email: "a@example.com", Owner: "O"}).Fields(); strings.Join(got, ",") != "owner,email" {
		t.Errorf("Fields = %v", got)
	}
	if !(Contact{}).IsZero() || (Contact{Owner: "x"}).IsZero() {
		t.Error("IsZero wrong")
	}
}
