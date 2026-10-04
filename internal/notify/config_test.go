// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"strings"
	"testing"
)

func TestParse_UnsetMeansOff(t *testing.T) {
	for _, raw := range []string{"", "  \n"} {
		c, err := Parse(raw)
		if c != nil || err != nil {
			t.Fatalf("Parse(%q) = %v, %v; want nil, nil", raw, c, err)
		}
	}
}

func TestParse_AcceptsAWebhookChannel(t *testing.T) {
	c, err := Parse(`{"console_url":"https://wardyn.example.com","channels":[
		{"id":"sec-hook","type":"webhook","url":"https://hooks.example.com/a","hmac_secret":"k","bearer_token":"b"},
		{"id":"plain","type":"webhook","url":"http://hooks.example.com/b"}]}`)
	if err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	if len(c.Channels) != 2 || c.ConsoleURL != "https://wardyn.example.com" {
		t.Fatalf("parsed %+v", c)
	}
}

// TestParse_RefusalsNameTheRuleAndNeverAValue is the boot-refusal contract: each broken config is
// refused, the error names the channel id (where there is one) and the rule, and contains none of the
// secret-bearing substrings the operator typed.
func TestParse_RefusalsNameTheRuleAndNeverAValue(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		mustHave []string
		leaks    []string
	}{
		{"unimplemented type", `{"channels":[{"id":"chat","type":"smtp","url":"https://h.example.com/SECRETPATH"}]}`,
			[]string{`"chat"`, `"smtp"`}, []string{"SECRETPATH", "h.example.com"}},
		{"type that could be a secret is not echoed", `{"channels":[{"id":"chat","type":"TYPE-SECRET-VALUE","url":"https://h.example.com/x"}]}`,
			[]string{`"chat"`, "redacted"}, []string{"TYPE-SECRET-VALUE"}},
		{"bad url", `{"channels":[{"id":"hook","type":"webhook","url":"::not a url SECRETURL"}]}`,
			[]string{`"hook"`, "url"}, []string{"SECRETURL"}},
		{"non-http scheme", `{"channels":[{"id":"hook","type":"webhook","url":"ftp://h.example.com/SECRETPATH"}]}`,
			[]string{`"hook"`}, []string{"SECRETPATH", "h.example.com"}},
		{"plain http with hmac secret", `{"channels":[{"id":"hook","type":"webhook","url":"http://h.example.com/p","hmac_secret":"HMACSECRETVALUE"}]}`,
			[]string{`"hook"`, "https"}, []string{"HMACSECRETVALUE", "h.example.com"}},
		{"plain http with bearer token", `{"channels":[{"id":"hook","type":"webhook","url":"http://h.example.com/p","bearer_token":"BEARERSECRETVALUE"}]}`,
			[]string{`"hook"`, "https"}, []string{"BEARERSECRETVALUE", "h.example.com"}},
		{"plain http with userinfo", `{"channels":[{"id":"hook","type":"webhook","url":"http://user:PASSWORDVALUE@h.example.com/p"}]}`,
			[]string{`"hook"`, "https"}, []string{"PASSWORDVALUE", "h.example.com"}},
		{"plain http with query", `{"channels":[{"id":"hook","type":"webhook","url":"http://h.example.com/p?sig=QUERYSECRET"}]}`,
			[]string{`"hook"`, "https"}, []string{"QUERYSECRET", "h.example.com"}},
		{"malformed JSON", `{"channels":[{"id":"hook","type":"webhook","url":"https://h.example.com/JSONSECRET"`,
			[]string{"malformed JSON"}, []string{"JSONSECRET", "h.example.com"}},
		{"unknown field", `{"channels":[{"id":"hook","type":"webhook","url":"https://h.example.com/p","hmac_secrt":"TYPOSECRETVALUE"}]}`,
			[]string{"schema"}, []string{"TYPOSECRETVALUE", "hmac_secrt"}},
		{"wrong type", `{"channels":[{"id":"hook","type":"webhook","url":["https://h.example.com/ARRAYSECRET"]}]}`,
			[]string{"schema"}, []string{"ARRAYSECRET"}},
		{"trailing data", `{"channels":[{"id":"hook","type":"webhook","url":"https://h.example.com/p"}]} TRAILINGSECRET`,
			[]string{"trailing"}, []string{"TRAILINGSECRET"}},
		{"duplicate id", `{"channels":[{"id":"hook","type":"webhook","url":"https://a.example.com/p"},{"id":"hook","type":"webhook","url":"https://b.example.com/p"}]}`,
			[]string{`"hook"`, "twice"}, []string{"a.example.com", "b.example.com"}},
		{"bad id", `{"channels":[{"id":"Bad Id","type":"webhook","url":"https://h.example.com/p"}]}`,
			[]string{"#1", "id must match"}, []string{"Bad Id"}},
		{"no channels", `{"channels":[]}`, []string{"at least one channel"}, nil},
		{"console_url http", `{"console_url":"http://console.example.com","channels":[{"id":"hook","type":"webhook","url":"https://h.example.com/p"}]}`,
			[]string{"console_url"}, []string{"console.example.com"}},
		{"console_url userinfo", `{"console_url":"https://u:CONSOLEPASS@console.example.com","channels":[{"id":"hook","type":"webhook","url":"https://h.example.com/p"}]}`,
			[]string{"console_url"}, []string{"CONSOLEPASS", "console.example.com"}},
		{"console_url query", `{"console_url":"https://console.example.com/?t=CONSOLEQUERY","channels":[{"id":"hook","type":"webhook","url":"https://h.example.com/p"}]}`,
			[]string{"console_url"}, []string{"CONSOLEQUERY"}},
		{"console_url fragment", `{"console_url":"https://console.example.com/#CONSOLEFRAG","channels":[{"id":"hook","type":"webhook","url":"https://h.example.com/p"}]}`,
			[]string{"console_url"}, []string{"CONSOLEFRAG"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Parse(c.raw)
			if err == nil {
				t.Fatalf("config accepted: %+v", cfg)
			}
			for _, want := range c.mustHave {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			for _, leak := range c.leaks {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error %q leaks %q", err, leak)
				}
			}
		})
	}
}
