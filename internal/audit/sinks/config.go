// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Config is the top-level JSON configuration block for the sinks subsystem.
// Example:
//
//	{
//	  "syslog":  {"network": "udp", "addr": "localhost:514"},
//	  "webhook": {"url": "https://siem.example.com/ingest", "bearer_token": "...", "batch_size": 50},
//	  "file":    {"path": "/var/log/wardyn/audit.log", "max_bytes": 52428800, "keep": 3}
//	}
//
// Omitting a top-level key disables that sink entirely.
type Config struct {
	Syslog  *SyslogConfig  `json:"syslog,omitempty"`
	Webhook *WebhookConfig `json:"webhook,omitempty"`
	File    *FileConfig    `json:"file,omitempty"`
}

// Source (WARDYN_AUDIT_SOURCE), when set, is stamped as an extra "source"
// field on every event a sink serializes, so one SIEM index can tell apart
// ingests from several wardynd instances. Set once at boot; never mutated.
var Source string

// marshalEvent serializes ev like json.Marshal, merging in Source as a
// "source" field when set; every sink calls this instead of json.Marshal directly.
func marshalEvent(ev types.AuditEvent) ([]byte, error) {
	if Source == "" {
		return json.Marshal(ev)
	}
	return json.Marshal(struct {
		types.AuditEvent
		Source string `json:"source"`
	}{ev, Source})
}

// SyslogConfig is the JSON-serialisable counterpart of SyslogSink fields.
type SyslogConfig struct {
	// Network is the transport: "tcp", "udp", or "" for local socket.
	Network string `json:"network,omitempty"`
	// Addr is the remote endpoint, e.g. "host:514". Empty = local socket.
	Addr string `json:"addr,omitempty"`
}

// ParseSinks parses cfgJSON into the enabled sinks, for wrapping in a Fanout;
// the caller must Close() any Closer sinks (SyslogSink, FileSink) on
// shutdown, including any sinks returned alongside an error.
func ParseSinks(cfgJSON []byte) ([]audit.Sink, error) {
	var cfg Config
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		return nil, fmt.Errorf("sinks.ParseSinks: invalid JSON: %w", err)
	}
	var out []audit.Sink

	if cfg.Syslog != nil {
		s, err := NewSyslogSink(cfg.Syslog.Network, cfg.Syslog.Addr)
		if err != nil {
			return out, fmt.Errorf("sinks.ParseSinks: syslog: %w", err)
		}
		out = append(out, s)
	}

	if cfg.Webhook != nil {
		if cfg.Webhook.Delivery == DeliveryAcknowledged {
			// Acknowledged delivery is not a fan-out sink: the leader-only loop
			// reads the stored trail from the durable checkpoint instead
			// (AckedWebhook). The config is still validated exactly as the
			// sink would have, so a bad one refuses boot here rather than at
			// first delivery.
			c := cfg.Webhook.withDefaults()
			if _, _, _, err := c.validate(); err != nil {
				return out, fmt.Errorf("sinks.ParseSinks: webhook: %w", err)
			}
		} else {
			s, err := NewWebhookSink(*cfg.Webhook)
			if err != nil {
				return out, fmt.Errorf("sinks.ParseSinks: webhook: %w", err)
			}
			out = append(out, s)
		}
	}

	if cfg.File != nil {
		s, err := NewFileSink(*cfg.File)
		if err != nil {
			return out, fmt.Errorf("sinks.ParseSinks: file: %w", err)
		}
		out = append(out, s)
	}

	return out, nil
}

// AcknowledgedWebhook returns the defaulted webhook config when the sinks
// config asks for acknowledged delivery, and nil otherwise — so a caller that
// would start the leader-only loop asks one question instead of re-parsing the
// JSON. An empty or blank cfgJSON (no sinks configured at all) is nil, nil.
// A config that fails validation is an error, exactly as ParseSinks reports
// it: boot has already refused it by the time this runs.
func AcknowledgedWebhook(cfgJSON []byte) (*WebhookConfig, error) {
	if len(bytes.TrimSpace(cfgJSON)) == 0 {
		return nil, nil
	}
	var cfg Config
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		return nil, fmt.Errorf("sinks.AcknowledgedWebhook: invalid JSON: %w", err)
	}
	if cfg.Webhook == nil || cfg.Webhook.Delivery != DeliveryAcknowledged {
		return nil, nil
	}
	c := cfg.Webhook.withDefaults()
	if _, _, _, err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
