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
	if err := startApprovalNotify(context.Background(), "", nil, nil, nil, 0); err != nil {
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
		// route refusals: unknown channel reference, unknown kind, non-ascending tiers, too many tiers,
		// and run_owner on a channel that redacts the requester
		`{"channels":[{"id":"hook","type":"webhook","url":"http://h.example.com/SECRETPATH"}],"routes":[{"tiers":[{"after":"0s","channels":["ghost"]}]}]}`,
		`{"channels":[{"id":"hook","type":"webhook","url":"http://h.example.com/SECRETPATH"}],"routes":[{"kinds":["ghost"],"tiers":[{"after":"0s","channels":["hook"]}]}]}`,
		`{"channels":[{"id":"hook","type":"webhook","url":"http://h.example.com/SECRETPATH"}],"routes":[{"tiers":[{"after":"5m","channels":["hook"]},{"after":"1m","channels":["hook"]}]}]}`,
		`{"channels":[{"id":"hook","type":"webhook","url":"http://h.example.com/SECRETPATH"}],"routes":[{"tiers":[` +
			`{"after":"0s","channels":["hook"]},{"after":"1s","channels":["hook"]},{"after":"2s","channels":["hook"]},` +
			`{"after":"3s","channels":["hook"]},{"after":"4s","channels":["hook"]},{"after":"5s","channels":["hook"]}]}]}`,
		`{"channels":[{"id":"hook","type":"webhook","url":"http://h.example.com/SECRETPATH","redact_requester":true}],"routes":[{"tiers":[{"after":"0s","channels":["hook"],"notify":["run_owner"]}]}]}`,
		// smtp: a CR or LF in the host, from, to, username or password, and a non-address
		`{"channels":[{"id":"mail","type":"smtp","host":"h.example.com\r\nSECRETPATH","port":587,"from":"w@example.com"}]}`,
		`{"channels":[{"id":"mail","type":"smtp","host":"h.example.com","port":587,"from":"w@example.com\nBcc: SECRETPATH@example.com"}]}`,
		`{"channels":[{"id":"mail","type":"smtp","host":"h.example.com","port":587,"from":"w@example.com","to":["a@example.com\rSECRETPATH"]}]}`,
		`{"channels":[{"id":"mail","type":"smtp","host":"h.example.com","port":587,"from":"w@example.com","username":"u\nSECRETPATH","password":"HMACVALUE"}]}`,
		`{"channels":[{"id":"mail","type":"smtp","host":"h.example.com","port":587,"from":"w@example.com","username":"u","password":"HMACVALUE\r"}]}`,
		`{"channels":[{"id":"mail","type":"smtp","host":"h.example.com","port":587,"from":"SECRETPATH <w@example.com>"}]}`,
	} {
		err := startApprovalNotify(context.Background(), raw, nil, nil, nil, 0)
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
