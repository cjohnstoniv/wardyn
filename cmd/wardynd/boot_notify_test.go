// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/notify"
)

// With the config unset nothing is installed: no planner output (so no outbox rows) and, because the
// pool is never touched, no worker.
func TestStartApprovalNotify_UnsetInstallsNothing(t *testing.T) {
	notify.SetActive(nil, nil)
	if err := startApprovalNotify(context.Background(), "", nil, nil, nil); err != nil {
		t.Fatalf("unset config refused boot: %v", err)
	}
	if notify.Enabled() {
		t.Fatal("notifications are enabled with the config unset")
	}
}

// A config naming a channel type this build lacks refuses boot, and neither that refusal nor a bad-URL
// one echoes the URL or a secret.
func TestStartApprovalNotify_RefusesBadConfigWithoutEchoingIt(t *testing.T) {
	notify.SetActive(nil, nil)
	for _, raw := range []string{
		`{"channels":[{"id":"chat","type":"carrier-pigeon","url":"https://h.example.com/SECRETPATH","hmac_secret":"HMACVALUE"}]}`,
		`{"channels":[{"id":"hook","type":"webhook","url":"http://h.example.com/SECRETPATH","hmac_secret":"HMACVALUE"}]}`,
		`{"channels":[{"id":"hook","type":"webhook","url":"::SECRETPATH"`,
		`{"channels":[{"id":"chat","type":"teams","url":"http://h.example.com/SECRETPATH?sig=HMACVALUE"}]}`,
		`{"channels":[{"id":"chat","type":"slack","url":"http://h.example.com/SECRETPATH"}]}`,
	} {
		err := startApprovalNotify(context.Background(), raw, nil, nil, nil)
		if err == nil {
			t.Fatalf("config accepted: %s", raw)
		}
		for _, leak := range []string{"SECRETPATH", "HMACVALUE", "h.example.com"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("boot refusal %q leaks %q", err, leak)
			}
		}
		if notify.Enabled() {
			t.Fatal("a refused config left notifications enabled")
		}
	}
}
