// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"encoding/json"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/audit"
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

// SyslogConfig is the JSON-serialisable counterpart of SyslogSink fields.
type SyslogConfig struct {
	// Network is the transport: "tcp", "udp", or "" for local socket.
	Network string `json:"network,omitempty"`
	// Addr is the remote endpoint, e.g. "host:514". Empty = local socket.
	Addr string `json:"addr,omitempty"`
}

// ParseSinks parses cfgJSON and constructs the enabled sinks, returning them
// as a slice suitable for wrapping in a Fanout. The caller is responsible for
// calling Close() on any Closer sinks (SyslogSink, FileSink) on shutdown.
//
// ParseSinks never partially constructs: if any enabled sink fails to
// initialise it returns all previously-initialised sinks alongside the error
// so the caller can close them.
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
