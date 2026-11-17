// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func channelsHealthz(channels string) string {
	return `{"ssh":{"enabled":true,"advertise_addr":"gw.example.com:2222","host_key_fingerprint":"SHA256:x"` + channels + `}}`
}

// TestSSHCommands_RefuseAKindTheGatewayDoesNotServe: each ssh-backed command is refused with one line, before
// ssh(1) starts and before the account's keys are read, when /healthz's channels list leaves out a kind it opens.
func TestSSHCommands_RefuseAKindTheGatewayDoesNotServe(t *testing.T) {
	for _, tc := range []struct {
		verb     string
		channels string
		off      string
	}{
		{"exec", `["forward","sftp","shell"]`, "commands (exec)"},
		{"forward", `["exec","sftp","shell"]`, "port forwarding"},
		{"editor", `["exec","shell"]`, "file transfer (sftp), port forwarding"},
		{"ssh", `["exec","forward","sftp"]`, "a terminal"},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			homeWithKey(t, false)
			log := fakeSSH(t, "0")
			f := newGatewayFixture(t, channelsHealthz(`,"channels":`+tc.channels), oneKey)
			var err error
			if tc.verb == "ssh" {
				_, err = runGateway(t, sshCmd, f, "", testRunID)
			} else {
				_, err = resolveSSHAccess(context.Background(), &sdk.Client{BaseURL: f.url}, tc.verb, testRunID, false)
			}
			want := tc.verb + ": this deployment's SSH gateway does not serve " + tc.off + " (WARDYN_SSH_CHANNELS); ask your Wardyn admin"
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
			neverRan(t, log)
			if f.keyGET.Load() != 0 {
				t.Errorf("read the account's keys before the channel refusal")
			}
		})
	}
}

// TestSSHCommands_ServedKindsAndOlderServersGoOn: a list that holds every kind the command opens, and a server
// that publishes no list at all (older than WARDYN_SSH_CHANNELS), are not refused.
func TestSSHCommands_ServedKindsAndOlderServersGoOn(t *testing.T) {
	for _, channels := range []string{``, `,"channels":["exec","forward","sftp","shell"]`} {
		for _, verb := range []string{"exec", "forward", "editor"} {
			f := newGatewayFixture(t, channelsHealthz(channels), oneKey)
			if _, err := resolveSSHAccess(context.Background(), &sdk.Client{BaseURL: f.url}, verb, testRunID, false); err != nil {
				t.Errorf("%s with channels %q: %v", verb, channels, err)
			}
		}
	}
	// A list that holds only what exec needs still serves exec.
	f := newGatewayFixture(t, channelsHealthz(`,"channels":["exec"]`), oneKey)
	if _, err := resolveSSHAccess(context.Background(), &sdk.Client{BaseURL: f.url}, "exec", testRunID, false); err != nil {
		t.Errorf("exec with only exec served: %v", err)
	}
	homeWithKey(t, false)
	log := fakeSSH(t, "0")
	f = newGatewayFixture(t, channelsHealthz(`,"channels":["shell"]`), oneKey)
	if _, err := runGateway(t, sshCmd, f, "", testRunID); err != nil {
		t.Errorf("ssh with shell served: %v", err)
	}
	if args := readArgs(t, log); !strings.Contains(strings.Join(args, " "), testRunID+"@gw.example.com") {
		t.Errorf("ssh argv = %q, want it to dial the gateway", args)
	}
}
