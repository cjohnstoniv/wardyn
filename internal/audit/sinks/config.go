// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
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
		s, err := NewWebhookSink(*cfg.Webhook)
		if err != nil {
			return out, fmt.Errorf("sinks.ParseSinks: webhook: %w", err)
		}
		out = append(out, s)
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
