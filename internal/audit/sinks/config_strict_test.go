// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks_test

import (
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
)

func TestParseSinks_RefusesATypoedConfig(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]string{
		"top-level typo":   `{"syslg":{"network":"tcp","addr":"siem:514"}}`,
		"webhook typo":     `{"web_hook":{"url":"https://siem.example.com/ingest"}}`,
		"nested typo":      `{"webhook":{"url":"https://siem.example.com/ingest","batchSize":50}}`,
		"trailing object":  `{"syslog":{"network":"udp","addr":"localhost:514"}} {}`,
		"trailing garbage": `{} x`,
	} {
		if _, err := sinks.ParseSinks([]byte(cfg)); err == nil {
			t.Errorf("%s: ParseSinks accepted %s", name, cfg)
		}
		if _, err := sinks.AcknowledgedWebhook([]byte(cfg)); err == nil {
			t.Errorf("%s: AcknowledgedWebhook accepted %s", name, cfg)
		}
	}
}

func TestNewFileSink_RefusesNegativeLimits(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "audit.log")
	for name, cfg := range map[string]sinks.FileConfig{
		"keep":      {Path: path, Keep: -1},
		"max_bytes": {Path: path, MaxBytes: -1},
	} {
		if s, err := sinks.NewFileSink(cfg); err == nil {
			_ = s.Close()
			t.Errorf("%s: NewFileSink accepted a negative value", name)
		}
	}
}
