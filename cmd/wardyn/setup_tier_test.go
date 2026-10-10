// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetupStatusNamesTheTier(t *testing.T) {
	for tier, want := range map[string]string{
		"local-only": "Tier: local only — not governed by an organisation",
		"runner":     "Tier: client-mode runner for an organisation",
		"org":        "Tier: organisation",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ready":true,"tier":"` + tier + `","checks":[]}`))
		}))
		t.Setenv("WARDYN_URL", srv.URL)
		t.Setenv("WARDYN_TOKEN", "tok")
		root := rootCmd()
		out := &strings.Builder{}
		root.SetArgs([]string{"setup", "status"})
		root.SetOut(out)
		root.SetErr(&strings.Builder{})
		err := root.Execute()
		srv.Close()
		if err != nil || !strings.Contains(out.String(), want) {
			t.Errorf("tier %s: err=%v out=%q, want %q", tier, err, out.String(), want)
		}
	}
}
