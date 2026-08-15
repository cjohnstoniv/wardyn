// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// W6-S1-6: --kind and --locator are cobra-required, so a missing one fails
// locally with cobra's own "required flag(s)" message rather than
// round-tripping to the server and surfacing its misleading 400.
func TestSourceCreateCmd_RequiresKindAndLocator(t *testing.T) {
	srv := newCmdServer(t, http.StatusOK, sdk.Source{})

	err := execCmd(t, "source", "create", "--locator", "acme/widgets", "--url", srv.URL, "--token", "tok")
	if err == nil {
		t.Fatal("expected error when --kind is missing, got nil")
	}
	if !strings.Contains(err.Error(), `required flag(s) "kind" not set`) {
		t.Errorf("error = %q, want the required-flag message", err)
	}

	err = execCmd(t, "source", "create", "--kind", "repo", "--url", srv.URL, "--token", "tok")
	if err == nil {
		t.Fatal("expected error when --locator is missing, got nil")
	}
	if !strings.Contains(err.Error(), `required flag(s) "locator" not set`) {
		t.Errorf("error = %q, want the required-flag message", err)
	}

	srv.mu.Lock()
	n := len(srv.reqs)
	srv.mu.Unlock()
	if n != 0 {
		t.Errorf("a required-flag failure must never round-trip to the server, got %d request(s)", n)
	}
}

func TestParseAttachArg(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		in           string
		wantTarget   string
		wantWritable bool
		wantErr      bool
	}{
		{in: id.String()},
		{in: id.String() + "@/work/api", wantTarget: "/work/api"},
		{in: id.String() + ":rw", wantWritable: true},
		{in: id.String() + ":ro"},
		{in: id.String() + "@/work/api:rw", wantTarget: "/work/api", wantWritable: true},
		{in: "not-a-uuid", wantErr: true},
		{in: "not-a-uuid@/t:rw", wantErr: true},
	}
	for _, c := range cases {
		gotID, target, writable, err := parseAttachArg(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseAttachArg(%q): want error, got id=%s", c.in, gotID)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAttachArg(%q): unexpected error: %v", c.in, err)
			continue
		}
		if gotID != id || target != c.wantTarget || writable != c.wantWritable {
			t.Errorf("parseAttachArg(%q) = (%s, %q, %v), want (%s, %q, %v)",
				c.in, gotID, target, writable, id, c.wantTarget, c.wantWritable)
		}
	}
}
