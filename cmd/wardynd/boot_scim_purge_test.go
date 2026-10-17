// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"
)

// The purge settings: 720h and reassign by default, zero disables the automatic purge, keep leaves the
// workspaces, and a negative delay or an unknown mode refuses boot.
func TestSCIMConfigPurgeSettings(t *testing.T) {
	served := tlsPosture{tlsEnabled: true, secureCookies: true}
	with := func(after time.Duration, workspaces string) *bootFlags {
		f := scimFlags(scimTestToken, "", scimTestIssuer, scimTestAdmin)
		f.scimPurgeAfter, f.scimLeaverWorkspaces = &after, &workspaces
		return f
	}
	cfg, err := scimConfig(scimFlags(scimTestToken, "", scimTestIssuer, scimTestAdmin), served)
	if err != nil || cfg.PurgeAfter != 720*time.Hour || cfg.KeepWorkspaces {
		t.Fatalf("defaults = %+v, %v", cfg, err)
	}
	if cfg, err = scimConfig(with(0, "keep"), served); err != nil || cfg.PurgeAfter != 0 || !cfg.KeepWorkspaces {
		t.Errorf("zero and keep = %+v, %v", cfg, err)
	}
	if cfg, err = scimConfig(with(48*time.Hour, "reassign"), served); err != nil || cfg.PurgeAfter != 48*time.Hour || cfg.KeepWorkspaces {
		t.Errorf("48h and reassign = %+v, %v", cfg, err)
	}
	if _, err = scimConfig(with(-time.Hour, "reassign"), served); err == nil || !strings.Contains(err.Error(), "WARDYN_SCIM_PURGE_AFTER") {
		t.Errorf("a negative delay: %v", err)
	}
	if _, err = scimConfig(with(time.Hour, "delete"), served); err == nil || !strings.Contains(err.Error(), "WARDYN_SCIM_LEAVER_WORKSPACES") {
		t.Errorf("an unknown workspaces mode: %v", err)
	}
}
