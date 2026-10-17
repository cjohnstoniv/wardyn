// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var update = flag.Bool("update", false, "rewrite the golden chat bodies in testdata/")

const consoleURL = "https://wardyn.example.com/"

func fixedFacts(kind types.ApprovalKind) approvalFacts {
	f := facts()
	f.Kind = kind
	f.RunID = uuid.MustParse("a1b2c3d4-0000-4000-8000-000000000002")
	f.RequestedAt = time.Date(2026, 10, 3, 14, 5, 59, 0, time.UTC)
	return f
}

func chatChannel(typ string) Channel {
	return Channel{ID: "chat", Type: typ, URL: "https://h.example.com/p"}
}

// TestRender_Golden pins the Teams and Slack bodies, byte for byte, for every approval kind at tier 0
// and tier 1. Regenerate with `go test ./internal/notify -run TestRender_Golden -update` and review
// the diff: the title text is fixed by the design record.
func TestRender_Golden(t *testing.T) {
	for _, typ := range []string{TypeTeams, TypeSlack} {
		for _, kind := range types.ApprovalKinds {
			for _, tier := range []int16{0, 1} {
				name := fmt.Sprintf("%s_%s_tier%d.json", typ, kind, tier)
				t.Run(name, func(t *testing.T) {
					if _, ok := kindLabels[kind]; !ok {
						t.Fatalf("kind %q has no chat label", kind)
					}
					got, err := chatChannel(typ).render(uuid.New(), tier, fixedFacts(kind), consoleURL, secretmask.Masker{})
					if err != nil {
						t.Fatal(err)
					}
					var pretty strings.Builder
					var v any
					if err := json.Unmarshal(got, &v); err != nil {
						t.Fatalf("body is not JSON: %v", err)
					}
					enc := json.NewEncoder(&pretty)
					enc.SetIndent("", "  ")
					_ = enc.Encode(v)
					path := filepath.Join("testdata", name)
					if *update {
						if err := os.MkdirAll("testdata", 0o755); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte(pretty.String()), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					want, err := os.ReadFile(path)
					if err != nil {
						t.Fatalf("golden missing (run with -update): %v", err)
					}
					if pretty.String() != string(want) {
						t.Fatalf("%s drifted\n got %s\nwant %s", name, pretty.String(), want)
					}
				})
			}
		}
	}
}

// hostile is requester and profile text that would be live in Slack mrkdwn or Adaptive Card Markdown.
const hostile = "*bold* _it_ `code` [click](https://evil.example.com) <a href=\"https://evil.example.com\">x</a> " +
	"<!channel> <!here> @here <@U123> <https://evil.example.com|safe> &amp; ![i](https://evil.example.com/i.png)\x00\x1b[31m\r\n"

// TestRender_HostileTextIsInert: markup in the requester and profile names does not become live. In
// Slack every one of those strings sits in a plain_text object and no mrkdwn object exists; in the
// Adaptive Card every control character is escaped and the only url in the body is console_url.
func TestRender_HostileTextIsInert(t *testing.T) {
	f := fixedFacts(types.ApprovalPushContent)
	f.Principal, f.ProfileName = hostile, hostile

	t.Run("slack", func(t *testing.T) {
		got, err := chatChannel(TypeSlack).render(uuid.New(), 0, f, consoleURL, secretmask.Masker{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), "mrkdwn") {
			t.Fatalf("a mrkdwn object or flag is present: %s", got)
		}
		var body struct {
			Text   string `json:"text"`
			Blocks []struct {
				Type     string                       `json:"type"`
				Text     map[string]any               `json:"text"`
				Elements []map[string]json.RawMessage `json:"elements"`
			} `json:"blocks"`
		}
		if err := json.Unmarshal(got, &body); err != nil {
			t.Fatal(err)
		}
		if strings.ContainsAny(body.Text, "<>*_`[") || body.Text != "Approval waiting: Push review" {
			t.Fatalf("top-level text (parsed as mrkdwn) is not the fixed title: %q", body.Text)
		}
		sawHostile := false
		for _, b := range body.Blocks {
			if b.Type == "actions" {
				continue
			}
			if b.Text["type"] != "plain_text" || b.Text["emoji"] != false {
				t.Fatalf("block %q text is %v, want plain_text with emoji off", b.Type, b.Text)
			}
			if s, _ := b.Text["text"].(string); strings.Contains(s, "<!channel>") {
				sawHostile = true
				if strings.ContainsAny(s, "\x00\x1b\r") {
					t.Fatalf("control characters survived: %q", s)
				}
			}
		}
		if !sawHostile {
			t.Fatalf("the hostile text was not rendered at all, so the test proves nothing: %s", got)
		}
		// The only url anywhere is the console button.
		if strings.Count(string(got), `"url"`) != 1 || !strings.Contains(string(got), `"url":"https://wardyn.example.com/approvals"`) {
			t.Fatalf("body carries a url other than console_url: %s", got)
		}
	})

	t.Run("teams", func(t *testing.T) {
		got, err := chatChannel(TypeTeams).render(uuid.New(), 0, f, consoleURL, secretmask.Masker{})
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Attachments []struct {
				ContentType string `json:"contentType"`
				Content     struct {
					Body []struct {
						Text string `json:"text"`
					} `json:"body"`
					Actions []struct {
						URL string `json:"url"`
					} `json:"actions"`
				} `json:"content"`
			} `json:"attachments"`
		}
		if err := json.Unmarshal(got, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Attachments) != 1 || len(body.Attachments[0].Content.Actions) != 1 || body.Attachments[0].Content.Actions[0].URL != "https://wardyn.example.com/approvals" {
			t.Fatalf("unexpected card shape: %s", got)
		}
		for _, tb := range body.Attachments[0].Content.Body[1:] {
			text := tb.Text
			if strings.HasPrefix(text, "Run: ") || strings.HasPrefix(text, "Requested: ") {
				continue // fixed text and server-minted values, not escaped
			}
			text = strings.SplitN(text, ": ", 2)[1] // the row label is fixed text
			if strings.ContainsAny(text, "\x00\x1b\r\n<>") {
				t.Fatalf("raw control character or angle bracket in %q", text)
			}
			// Every markdown-significant character must be preceded by a backslash or be part of an entity.
			for i, r := range text {
				if strings.ContainsRune("*_`[]()!:", r) && (i == 0 || text[i-1] != '\\') {
					t.Fatalf("unescaped %q at %d in %q", r, i, text)
				}
			}
		}
		joined := ""
		for _, tb := range body.Attachments[0].Content.Body {
			joined += tb.Text + "\n"
		}
		if !strings.Contains(joined, `&lt;\!channel&gt;`) || !strings.Contains(joined, `\[click\]\(https\:\/\/evil\.example\.com\)`) {
			t.Fatalf("hostile text missing or not escaped: %q", joined)
		}
		if strings.Count(string(got), "://") != 2 { // the $schema and console_url
			t.Fatalf("body carries a url other than the schema and console_url: %s", got)
		}
	})
}

// TestRender_RegisteredSecretNeverReachesAChatBody: the facts carry no scope or reason, so a secret
// written into both cannot be rendered; the masker also covers an identifier that equals one.
func TestRender_RegisteredSecretNeverReachesAChatBody(t *testing.T) {
	const secret = "ghp_REGISTEREDSECRETVALUE0123456789"
	reg := secretmask.NewRegistry()
	f := fixedFacts(types.ApprovalEgressDomain)
	reg.Add(f.RunID, []byte(secret))
	approval := types.ApprovalRequest{
		RequestedScope: json.RawMessage(`{"host":"evil.example.com","note":"` + secret + `"}`),
		Reason:         "please approve, token " + secret,
	}
	f.ProfileName = secret // an admin-authored name that equals a secret is masked too
	for _, typ := range []string{TypeTeams, TypeSlack} {
		got, err := chatChannel(typ).render(uuid.New(), 1, f, consoleURL, reg.Masker(f.RunID))
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{secret, "evil.example.com", approval.Reason, string(approval.RequestedScope)} {
			if strings.Contains(string(got), leak) {
				t.Fatalf("%s body carries %q: %s", typ, leak, got)
			}
		}
	}
}

func TestRender_RedactRequesterDropsTheOwnerOnEveryType(t *testing.T) {
	f := fixedFacts(types.ApprovalToolCall)
	for _, typ := range []string{TypeTeams, TypeSlack, TypeWebhook} {
		ch := chatChannel(typ)
		with, err := ch.render(uuid.New(), 0, f, consoleURL, secretmask.Masker{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(with), "alice") {
			t.Fatalf("%s: the requester is missing without redaction, so the test proves nothing: %s", typ, with)
		}
		ch.RedactRequester = true
		without, err := ch.render(uuid.New(), 0, f, consoleURL, secretmask.Masker{})
		if err != nil {
			t.Fatal(err)
		}
		for _, owner := range []string{"alice", "requester"} {
			if strings.Contains(string(without), owner) {
				t.Fatalf("%s: redact_requester left %q in the body: %s", typ, owner, without)
			}
		}
		if !json.Valid(without) {
			t.Fatalf("%s: body is not JSON: %s", typ, without)
		}
	}
}

func TestRender_CapsAndStripsLongNames(t *testing.T) {
	f := fixedFacts(types.ApprovalCredential)
	f.ProfileName = "pay\x00ments" + strings.Repeat("é", 300)
	for _, typ := range []string{TypeTeams, TypeSlack} {
		got, err := chatChannel(typ).render(uuid.New(), 0, f, consoleURL, secretmask.Masker{})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(got), `\u0000`) || strings.Contains(string(got), strings.Repeat("é", 200)) {
			t.Fatalf("%s: name was not stripped and capped: %s", typ, got)
		}
	}
}

func TestParse_ChatChannelsRequireHTTPSAndNeverEchoTheURL(t *testing.T) {
	for _, typ := range []string{TypeTeams, TypeSlack} {
		_, err := Parse(`{"channels":[{"id":"chat","type":"` + typ + `","url":"http://h.example.com/SECRETPATH?sig=SECRETSIG"}]}`)
		if err == nil {
			t.Fatalf("%s accepted a plain http URL", typ)
		}
		for _, want := range []string{`"chat"`, "https"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q lacks %q", typ, err, want)
			}
		}
		for _, leak := range []string{"SECRETPATH", "SECRETSIG", "h.example.com"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("%s: error %q leaks %q", typ, err, leak)
			}
		}
		c, err := Parse(`{"channels":[{"id":"chat","type":"` + typ + `","url":"https://h.example.com/p?sig=x","redact_requester":true}]}`)
		if err != nil || !c.Channels[0].RedactRequester {
			t.Fatalf("%s: valid https config refused or redact_requester dropped: %v", typ, err)
		}
	}
}
