// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package notify delivers approval notifications: a transactional outbox written beside every new
// approval, and a worker on every replica that sends each row at least once to an operator-named
// channel. The config is read once at boot from WARDYN_APPROVAL_NOTIFY.
package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// channelIDRe is the channel id grammar. It is also the metric label and the outbox column value, so
// the label set is bounded by config and the same pattern backs the table's CHECK.
var channelIDRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// The channel types this build implements. A new type is added here and in Channel.render; any other
// value is refused at boot.
const (
	TypeWebhook = "webhook"
	TypeTeams   = "teams"
	TypeSlack   = "slack"
)

var implementedTypes = []string{TypeWebhook, TypeTeams, TypeSlack}

// Config is the parsed WARDYN_APPROVAL_NOTIFY value.
type Config struct {
	ConsoleURL string    `json:"console_url"`
	Channels   []Channel `json:"channels"`
	// Routes choose channels by approval kind and leaf profile. Absent means every approval goes to
	// every channel at tier 0.
	Routes []Route `json:"routes"`
}

// MaxTiers is the most escalation tiers a route may have: the outbox column admits tiers 0 to 4.
const MaxTiers = 5

// Notify targets a tier may name.
const (
	TargetRunOwner       = "run_owner"
	TargetProfileContact = "profile_contact"
)

// Route is one routing rule. An absent Kinds or Profiles matches anything; the first matching route
// wins.
type Route struct {
	Kinds    []types.ApprovalKind `json:"kinds"`
	Profiles []string             `json:"profiles"` // governance profile ids, never names
	Tiers    []Tier               `json:"tiers"`
}

// Tier is one escalation step: its channels are sent After the approval was raised, while it is
// still pending.
type Tier struct {
	After    string   `json:"after"`
	Channels []string `json:"channels"`
	Notify   []string `json:"notify"`
	after    time.Duration
}

// Channel is one named destination. The id, never the URL, is what the outbox stores, so rotating a
// URL under the same id keeps pending rows deliverable.
type Channel struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	HMACSecret  string `json:"hmac_secret"`
	BearerToken string `json:"bearer_token"`
	// RedactRequester drops the run owner's principal and email from this channel's messages.
	RedactRequester bool `json:"redact_requester"`
}

// Parse validates raw and returns the config. An empty value means notifications are off and returns
// (nil, nil).
//
// Every error names the channel id and the rule it broke and nothing else: never the URL, a secret, a
// token, console_url or any other field value, because boot errors reach logs and process supervisors.
// The decoder's own errors are not wrapped for the same reason: they can quote input.
func Parse(raw string) (*Config, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return nil, fmt.Errorf("approval notify config: malformed JSON at byte %d (details withheld)", syn.Offset)
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errors.New("approval notify config: malformed JSON (truncated)")
		}
		return nil, errors.New("approval notify config: does not match the schema (unknown field or wrong type; details withheld)")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("approval notify config: trailing data after the JSON object")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	if c.ConsoleURL != "" {
		u, err := url.Parse(c.ConsoleURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Contains(c.ConsoleURL, "?") || strings.Contains(c.ConsoleURL, "#") {
			return errors.New("approval notify config: console_url must be an https URL with no userinfo, query or fragment")
		}
	}
	if len(c.Channels) == 0 {
		return errors.New("approval notify config: channels must name at least one channel")
	}
	seen := map[string]bool{}
	for i := range c.Channels {
		ch := &c.Channels[i]
		if !channelIDRe.MatchString(ch.ID) {
			return fmt.Errorf("approval notify config: channel #%d: id must match %s", i+1, channelIDRe)
		}
		if seen[ch.ID] {
			return fmt.Errorf("approval notify config: channel %q: id is listed twice", ch.ID)
		}
		seen[ch.ID] = true
		if err := ch.validate(); err != nil {
			return err
		}
	}
	for i := range c.Routes {
		if err := c.validateRoute(i); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) channel(id string) *Channel {
	for i := range c.Channels {
		if c.Channels[i].ID == id {
			return &c.Channels[i]
		}
	}
	return nil
}

// validateRoute refuses a route that names an unknown kind, profile or channel, has non-ascending or
// too many tiers, or notifies the run owner through a channel that redacts the requester. Errors name
// the route number and the rule, plus a channel id (an operator-chosen label), never a config value.
func (c *Config) validateRoute(i int) error {
	r := &c.Routes[i]
	for _, k := range r.Kinds {
		if !slices.Contains(types.ApprovalKinds, k) {
			return fmt.Errorf("approval notify config: route #%d: kinds names an unknown approval kind (value withheld)", i+1)
		}
	}
	for j, p := range r.Profiles {
		id, err := uuid.Parse(p)
		if err != nil {
			return fmt.Errorf("approval notify config: route #%d: profiles must be governance profile ids (uuids), not names (value withheld)", i+1)
		}
		r.Profiles[j] = id.String()
	}
	if len(r.Tiers) == 0 || len(r.Tiers) > MaxTiers {
		return fmt.Errorf("approval notify config: route #%d: tiers must hold 1 to %d tiers", i+1, MaxTiers)
	}
	prev := time.Duration(-1)
	for j := range r.Tiers {
		t := &r.Tiers[j]
		d, err := time.ParseDuration(t.After)
		if err != nil || d < 0 {
			return fmt.Errorf("approval notify config: route #%d tier #%d: after must be a duration of zero or more such as 30m", i+1, j+1)
		}
		if d <= prev {
			return fmt.Errorf("approval notify config: route #%d tier #%d: after must be strictly greater than the previous tier's", i+1, j+1)
		}
		prev, t.after = d, d
		if len(t.Channels) == 0 {
			return fmt.Errorf("approval notify config: route #%d tier #%d: channels must name at least one channel", i+1, j+1)
		}
		for k, id := range t.Channels {
			ch := c.channel(id)
			if ch == nil {
				return fmt.Errorf("approval notify config: route #%d tier #%d: channel reference #%d names no configured channel", i+1, j+1, k+1)
			}
			if slices.Contains(t.Channels[:k], id) {
				return fmt.Errorf("approval notify config: route #%d tier #%d: channel %q is listed twice", i+1, j+1, id)
			}
			if ch.RedactRequester && slices.Contains(t.Notify, TargetRunOwner) {
				return fmt.Errorf("approval notify config: route #%d tier #%d: notify %s on channel %q, which sets redact_requester (the owner is the requester)", i+1, j+1, TargetRunOwner, id)
			}
		}
		for _, n := range t.Notify {
			if n != TargetRunOwner && n != TargetProfileContact {
				return fmt.Errorf("approval notify config: route #%d tier #%d: notify must be %s or %s", i+1, j+1, TargetRunOwner, TargetProfileContact)
			}
		}
	}
	return nil
}

// ExpiryWarnings lists the tiers whose after is at or beyond the approval expiry: the approval will
// have expired first, so that tier never sends. A non-positive expiry means approvals never expire.
func (c *Config) ExpiryWarnings(expiry time.Duration) []string {
	if expiry <= 0 {
		return nil
	}
	var out []string
	for i, r := range c.Routes {
		for j, t := range r.Tiers {
			if t.after >= expiry {
				out = append(out, fmt.Sprintf("route #%d tier #%d: after is at or beyond WARDYN_APPROVAL_EXPIRY_AFTER, so the approval expires before it sends", i+1, j+1))
			}
		}
	}
	return out
}

// route returns the first route matching the approval, or nil.
func (c *Config) route(kind types.ApprovalKind, profileID *uuid.UUID) *Route {
	for i := range c.Routes {
		r := &c.Routes[i]
		if len(r.Kinds) > 0 && !slices.Contains(r.Kinds, kind) {
			continue
		}
		if len(r.Profiles) > 0 && (profileID == nil || !slices.Contains(r.Profiles, profileID.String())) {
			continue
		}
		return r
	}
	return nil
}

func (ch *Channel) validate() error {
	if !slices.Contains(implementedTypes, ch.Type) {
		shown := "(redacted)"
		if channelIDRe.MatchString(ch.Type) {
			shown = ch.Type
		}
		return fmt.Errorf("approval notify config: channel %q: type %q is not one this build implements (%s)", ch.ID, shown, strings.Join(implementedTypes, ", "))
	}
	u, err := url.Parse(ch.URL)
	if err != nil || ch.URL == "" || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("approval notify config: channel %q: url must be an absolute http or https URL (value withheld)", ch.ID)
	}
	// A Teams or Slack URL is itself the credential, so plain HTTP is never allowed for them.
	if ch.Type != TypeWebhook && u.Scheme != "https" {
		return fmt.Errorf("approval notify config: channel %q: a %s url must be https (value withheld)", ch.ID, ch.Type)
	}
	if u.Scheme != "https" && (ch.HMACSecret != "" || ch.BearerToken != "" || u.User != nil || u.RawQuery != "" || strings.Contains(ch.URL, "?")) {
		return fmt.Errorf("approval notify config: channel %q: https is required when hmac_secret or bearer_token is set or the url carries userinfo or a query", ch.ID)
	}
	return nil
}
