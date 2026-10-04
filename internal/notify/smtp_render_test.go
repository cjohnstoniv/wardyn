// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func mailChan() Channel {
	return Channel{ID: "mail", Type: TypeSMTP, Host: "smtp.example.com", Port: 587, From: "wardyn@example.com", To: []string{"secops@example.com"}}
}

var goldenDelivery = uuid.MustParse("00000000-0000-4000-8000-0000000000d1")

func renderMailFor(t *testing.T, ch Channel, f approvalFacts, tier int16, m secretmask.Masker) (string, error) {
	t.Helper()
	got, err := ch.render(goldenDelivery, tier, f, consoleURL, m)
	return strings.ReplaceAll(string(got), "\r\n", "\n"), err
}

// TestRenderMail_Golden pins the headers and body, byte for byte: subject, recipients, Message-ID and
// plain-text body for a tier-0 and an escalated message. Date is added at send time and pinned by the
// transport test. Regenerate with -update and review the diff.
func TestRenderMail_Golden(t *testing.T) {
	for _, c := range []struct {
		kind types.ApprovalKind
		tier int16
	}{{types.ApprovalEgressDomain, 0}, {types.ApprovalPushContent, 1}} {
		name := fmt.Sprintf("smtp_%s_tier%d.eml", c.kind, c.tier)
		t.Run(name, func(t *testing.T) {
			f := fixedFacts(c.kind)
			f.Recipients = []recipient{{TargetRunOwner, "alice@example.com"}, {TargetProfileContact, "payments-owner@example.com"}}
			got, err := renderMailFor(t, mailChan(), f, c.tier, secretmask.Masker{})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", name)
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("golden missing (run with -update): %v", err)
			}
			if got != string(want) {
				t.Fatalf("%s drifted\n got %s\nwant %s", name, got, want)
			}
		})
	}
}

// TestRenderMail_RegisteredSecretIsMasked: a name that equals a registered secret never reaches the
// message.
func TestRenderMail_RegisteredSecretIsMasked(t *testing.T) {
	const secret = "ghp_REGISTEREDSECRETVALUE0123456789"
	reg := secretmask.NewRegistry()
	f := fixedFacts(types.ApprovalEgressDomain)
	reg.Add(f.RunID, []byte(secret))
	f.ProfileName, f.Principal = secret, secret
	got, err := renderMailFor(t, mailChan(), f, 1, reg.Masker(f.RunID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, secret) {
		t.Fatalf("message carries a registered secret:\n%s", got)
	}
}

// TestRenderMail_BadRecipientsAreSkippedAndNeverReachAHeader: a stored email with a CR LF, a pair, a
// display name or a bracket form is dropped; the good static address still goes.
func TestRenderMail_BadRecipientsAreSkippedAndNeverReachAHeader(t *testing.T) {
	bad := []string{
		"victim@example.com\r\nBcc: attacker@example.com",
		"a@example.com, b@example.com",
		"a@example.com;b@example.com",
		"Alice <alice@example.com>",
		"<alice@example.com>",
		"alice@example.com (comment)",
		"alice @example.com",
		"alice@example.com\x00",
		strings.Repeat("a", 250) + "@example.com",
		"",
	}
	for _, e := range bad {
		f := fixedFacts(types.ApprovalToolCall)
		f.Recipients, f.Email = []recipient{{TargetRunOwner, e}}, e
		got, err := renderMailFor(t, mailChan(), f, 0, secretmask.Masker{})
		if err != nil {
			t.Fatalf("%q: %v", e, err)
		}
		if !strings.Contains(got, "\nRequester: alice\n") {
			t.Fatalf("%q reached the body:\n%s", e, got)
		}
		head, _, _ := strings.Cut(got, "\n\n")
		if !strings.Contains(head, "\nTo: secops@example.com\n") || strings.Contains(got, "attacker") || strings.Count(head, "@example.com") != 3 {
			t.Fatalf("%q reached the message:\n%s", e, got)
		}
	}
}

// TestRenderMail_NoRecipientIsAnError: with no static address and nothing valid resolved the row
// cannot send.
func TestRenderMail_NoRecipientIsAnError(t *testing.T) {
	ch := mailChan()
	ch.To = nil
	f := fixedFacts(types.ApprovalToolCall)
	f.Recipients = []recipient{{TargetRunOwner, "a@example.com, b@example.com"}}
	if _, err := ch.render(goldenDelivery, 0, f, consoleURL, secretmask.Masker{}); err != errNoRecipient {
		t.Fatalf("err = %v, want errNoRecipient", err)
	}
}

func TestParse_SMTPRefusals(t *testing.T) {
	pre := `{"channels":[{"id":"mail","type":"smtp",`
	good := `"host":"smtp.example.com","port":587,"from":"w@example.com"`
	if _, err := Parse(pre + good + `,"to":["a@example.com"],"username":"u","password":"p"}]}`); err != nil {
		t.Fatalf("a valid smtp channel was refused: %v", err)
	}
	for name, c := range map[string]struct{ cfg, leak string }{
		"CR in host":      {`"host":"smtp.example.com\r","port":587,"from":"w@example.com"`, "smtp.example"},
		"LF in from":      {`"host":"smtp.example.com","port":587,"from":"w@example.com\nBcc: x@example.com"`, "Bcc"},
		"LF in to":        {good + `,"to":["a@example.com\r\nBcc: x@example.com"]`, "Bcc"},
		"CR in username":  {good + `,"username":"u\r","password":"PASSWORDVALUE"`, "PASSWORDVALUE"},
		"LF in password":  {good + `,"username":"u","password":"PASSWORD\nVALUE"`, "PASSWORD"},
		"display-name to": {good + `,"to":["Bob <b@example.com>"]`, "Bob"},
		"bad from":        {`"host":"smtp.example.com","port":587,"from":"not an address"`, "not an address"},
		"no host":         {`"port":587,"from":"w@example.com"`, ""},
		"host with path":  {`"host":"smtp.example.com/x","port":587,"from":"w@example.com"`, "smtp.example"},
		"no port":         {`"host":"smtp.example.com","from":"w@example.com"`, ""},
		"password alone":  {good + `,"password":"PASSWORDVALUE"`, "PASSWORDVALUE"},
		"url on smtp":     {good + `,"url":"https://h.example.com/SECRETPATH"`, "SECRETPATH"},
	} {
		_, err := Parse(pre + c.cfg + `}]}`)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), `"mail"`) || (c.leak != "" && strings.Contains(err.Error(), c.leak)) {
			t.Errorf("%s: error %q must name the channel and not echo %q", name, err, c.leak)
		}
	}
}
