// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SchemaV1 names the webhook body. The body is an ALLOWLIST: every field below is set from an
// admin-authored or server-minted value. requested_scope, the reason, the run title and any credential
// are written or influenced by the sandbox agent and are never sent, because a chat message under the
// deployment's name carrying agent-chosen text is a phishing channel into the approving team.
const SchemaV1 = "wardyn.approval.v1"

// maxFieldBytes caps a requester or profile string.
const maxFieldBytes = 256

type payload struct {
	Schema     string             `json:"schema"`
	DeliveryID string             `json:"delivery_id"`
	Event      string             `json:"event"`
	Tier       int16              `json:"tier"`
	Approval   payloadApproval    `json:"approval"`
	Run        payloadRun         `json:"run"`
	Profile    *payloadProfile    `json:"profile,omitempty"`
	Requester  *payloadRequester  `json:"requester,omitempty"`
	Recipients []payloadRecipient `json:"recipients,omitempty"`
	ConsoleURL string             `json:"console_url,omitempty"`
}

type payloadRecipient struct {
	Role  string `json:"role"`
	Email string `json:"email"`
}

type payloadApproval struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	RequestedAt string `json:"requested_at"`
}

type payloadRun struct {
	ID string `json:"id"`
}

type payloadProfile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type payloadRequester struct {
	Principal string `json:"principal"`
	Email     string `json:"email,omitempty"`
}

// approvalFacts is what the worker reads at send time. Nothing here is stored in the outbox.
type approvalFacts struct {
	ApprovalID  uuid.UUID
	State       types.ApprovalState
	Kind        types.ApprovalKind
	RequestedAt time.Time
	RunID       uuid.UUID
	Principal   string
	Email       string
	ProfileID   *uuid.UUID
	ProfileName string
	// Recipients are the addresses the tier's notify targets resolved to at send time; RedactRequester
	// is the sending channel's setting.
	Recipients      []recipient
	RedactRequester bool
}

// recipient is one resolved notify target: its role (a Target* name) and address.
type recipient struct{ Role, Email string }

// buildPayload renders the body for one outbox row. Every string field is control-stripped, capped and
// passed through the run's masker BEFORE encoding, so the JSON stays well formed and a signature
// covers the final bytes.
func buildPayload(deliveryID uuid.UUID, tier int16, f approvalFacts, consoleURL string, m secretmask.Masker) ([]byte, error) {
	clean := func(s string) string {
		return string(m.Mask([]byte(capBytes(stripControl(s)))))
	}
	event := "raised"
	if tier > 0 {
		event = "escalated"
	}
	p := payload{
		Schema:     SchemaV1,
		DeliveryID: clean(deliveryID.String()),
		Event:      event,
		Tier:       tier,
		Approval: payloadApproval{
			ID:          clean(f.ApprovalID.String()),
			Kind:        clean(string(f.Kind)),
			RequestedAt: clean(f.RequestedAt.UTC().Format(time.RFC3339)),
		},
		Run: payloadRun{ID: clean(f.RunID.String())},
	}
	if f.ProfileID != nil {
		p.Profile = &payloadProfile{ID: clean(f.ProfileID.String()), Name: clean(f.ProfileName)}
	}
	if f.Principal != "" && !f.RedactRequester {
		p.Requester = &payloadRequester{Principal: clean(f.Principal), Email: clean(f.Email)}
	}
	for _, r := range f.Recipients {
		p.Recipients = append(p.Recipients, payloadRecipient{Role: r.Role, Email: clean(r.Email)})
	}
	if consoleURL != "" {
		p.ConsoleURL = clean(strings.TrimRight(consoleURL, "/") + "/approvals")
	}
	return json.Marshal(p)
}

// Sign returns the X-Wardyn-Signature value: t=<unix seconds>,v1=hex(HMAC-SHA256(secret, t+"."+body)).
// The timestamp lets a receiver bound replay; one more than five minutes old should be refused.
func Sign(secret string, t int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	ts := strconv.FormatInt(t, 10)
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// capBytes cuts s to maxFieldBytes without splitting a rune.
func capBytes(s string) string {
	if len(s) <= maxFieldBytes {
		return s
	}
	cut := maxFieldBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
