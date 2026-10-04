// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// kindLabels is the fixed, human-readable name of each approval kind in a chat title.
var kindLabels = map[types.ApprovalKind]string{
	types.ApprovalCredential:       "Credential",
	types.ApprovalEgressDomain:     "Network access",
	types.ApprovalToolCall:         "Tool call",
	types.ApprovalCredentialReauth: "Sign-in needed",
	types.ApprovalPushContent:      "Push review",
}

// message is the chat view of one outbox row, built from the same allowlisted facts as the webhook
// body. Title, RunShort and Requested come from fixed text and server-minted values and are rendered
// as is; Requester, Profile and ConsoleURL are cleaned (control-stripped, capped, masked) and the first
// two are escaped by each renderer for its markup.
type message struct {
	Title      string
	RunShort   string
	Profile    string
	Requester  string
	Requested  string
	ConsoleURL string
}

func newMessage(tier int16, f approvalFacts, consoleURL string, m secretmask.Masker) message {
	clean := func(s string) string {
		return string(m.Mask([]byte(capBytes(stripControl(s)))))
	}
	label, ok := kindLabels[f.Kind]
	if !ok {
		label = clean(string(f.Kind))
	}
	title := "Approval waiting: " + label
	if tier > 0 {
		title = fmt.Sprintf("Approval still waiting (escalation %d): %s", tier, label)
	}
	msg := message{
		Title:     title,
		RunShort:  strings.SplitN(f.RunID.String(), "-", 2)[0],
		Requested: f.RequestedAt.UTC().Format("2006-01-02 15:04") + " UTC",
	}
	if f.ProfileID != nil {
		msg.Profile = clean(f.ProfileName)
	}
	if f.Principal != "" {
		msg.Requester = clean(f.Principal)
		if f.Email != "" {
			msg.Requester += " (" + clean(f.Email) + ")"
		}
	}
	if consoleURL != "" {
		msg.ConsoleURL = clean(strings.TrimRight(consoleURL, "/") + "/approvals")
	}
	return msg
}

// render builds the body this channel's type expects. A channel with redact_requester never sees the
// run owner, whatever its type.
func (ch Channel) render(deliveryID uuid.UUID, tier int16, f approvalFacts, consoleURL string, m secretmask.Masker) ([]byte, error) {
	if ch.RedactRequester {
		f.Principal, f.Email = "", ""
	}
	switch ch.Type {
	case TypeTeams:
		return renderTeams(newMessage(tier, f, consoleURL, m))
	case TypeSlack:
		return renderSlack(newMessage(tier, f, consoleURL, m))
	}
	return buildPayload(deliveryID, tier, f, consoleURL, m)
}

// lines are the body rows under the title, each as label and text. escape is applied to the two
// sandbox-adjacent rows only.
func (msg message) lines(escape func(string) string) [][2]string {
	rows := [][2]string{{"Run", msg.RunShort}}
	if msg.Profile != "" {
		rows = append(rows, [2]string{"Profile", escape(msg.Profile)})
	}
	if msg.Requester != "" {
		rows = append(rows, [2]string{"Requester", escape(msg.Requester)})
	}
	return append(rows, [2]string{"Requested", msg.Requested})
}

// renderSlack builds an incoming-webhook body. Every sandbox-adjacent string sits in a plain_text
// object, which Slack never parses for <...|...> links, <!channel> or mentions; the only link is a
// button whose url is console_url. The top-level text (the notification fallback, which Slack does
// parse as mrkdwn) is the fixed title alone.
func renderSlack(msg message) ([]byte, error) {
	plain := func(s string) map[string]any { return map[string]any{"type": "plain_text", "text": s, "emoji": false} }
	var rows []string
	for _, r := range msg.lines(func(s string) string { return s }) {
		rows = append(rows, r[0]+": "+r[1])
	}
	blocks := []any{
		map[string]any{"type": "header", "text": plain(msg.Title)},
		map[string]any{"type": "section", "text": plain(strings.Join(rows, "\n"))},
	}
	if msg.ConsoleURL != "" {
		blocks = append(blocks, map[string]any{"type": "actions", "elements": []any{
			map[string]any{"type": "button", "text": plain("Open approvals"), "url": msg.ConsoleURL},
		}})
	}
	return json.Marshal(map[string]any{"text": msg.Title, "blocks": blocks})
}

// renderTeams builds a Teams Workflows message carrying one Adaptive Card. TextBlocks render Markdown,
// so requester and profile text is escaped by escapeAdaptive; the link is an Action.OpenUrl, never
// Markdown.
func renderTeams(msg message) ([]byte, error) {
	block := func(text string, extra map[string]any) map[string]any {
		b := map[string]any{"type": "TextBlock", "text": text, "wrap": true}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	body := []any{block(msg.Title, map[string]any{"weight": "Bolder", "size": "Medium"})}
	for _, r := range msg.lines(escapeAdaptive) {
		body = append(body, block(r[0]+": "+r[1], nil))
	}
	card := map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body":    body,
	}
	if msg.ConsoleURL != "" {
		card["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "title": "Open approvals", "url": msg.ConsoleURL}}
	}
	return json.Marshal(map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content":     card,
		}},
	})
}

// escapeAdaptive makes s inert in an Adaptive Card TextBlock: & < > become entities and every other
// ASCII punctuation character is backslash-escaped, which CommonMark defines as a literal. That covers
// emphasis, link and image syntax, and keeps a bare host or URL from auto-linking.
func escapeAdaptive(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r < 0x80 && strings.ContainsRune("!\"#$%'()*+,-./:;=?@[\\]^_`{|}~", r):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
