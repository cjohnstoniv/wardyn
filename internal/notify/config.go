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
)

// channelIDRe is the channel id grammar. It is also the metric label and the outbox column value, so
// the label set is bounded by config and the same pattern backs the table's CHECK.
var channelIDRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// TypeWebhook is the only channel type this build implements. A new type is added here and in
// Channel.send, nowhere else; any other value is refused at boot.
const TypeWebhook = "webhook"

var implementedTypes = []string{TypeWebhook}

// Config is the parsed WARDYN_APPROVAL_NOTIFY value.
type Config struct {
	ConsoleURL string    `json:"console_url"`
	Channels   []Channel `json:"channels"`
}

// Channel is one named destination. The id, never the URL, is what the outbox stores, so rotating a
// URL under the same id keeps pending rows deliverable.
type Channel struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	HMACSecret  string `json:"hmac_secret"`
	BearerToken string `json:"bearer_token"`
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
	if u.Scheme != "https" && (ch.HMACSecret != "" || ch.BearerToken != "" || u.User != nil || u.RawQuery != "" || strings.Contains(ch.URL, "?")) {
		return fmt.Errorf("approval notify config: channel %q: https is required when hmac_secret or bearer_token is set or the url carries userinfo or a query", ch.ID)
	}
	return nil
}
