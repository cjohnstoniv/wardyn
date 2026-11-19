// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"context"
	"strings"
	"testing"
)

const syncPassUnfinished = "the session ended before its sync pass finished"

// The ssh.sync.transfer row by the reports the gateway took. sftp has no end
// marker, so a connection cut mid-pass can reach sftp-server as a plain EOF and
// a clean exit: these sessions end that way (stdin EOF, exit 0). A pass started
// and never accounted for is a failure; a client that sends no start (an older
// wardyn sync) keeps the exit's outcome.
func TestSyncGateway_TheRowReadsThePassReports(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reports []syncPassReport
		want    string
		wantErr string
	}{
		{"no reports, as a 0.9 client", nil, "success", ""},
		{"an account only, as a 0.10.0 client", []syncPassReport{{Complete: true}}, "success", ""},
		{"started then complete", []syncPassReport{{Started: true}, {Complete: true}}, "success", ""},
		{"started then failed", []syncPassReport{{Started: true}, {Error: "disk full"}}, "success", ""},
		{"started and never accounted for", []syncPassReport{{Started: true}}, "failure", syncPassUnfinished},
		{"a later pass started and never accounted for", []syncPassReport{{Started: true}, {Complete: true}, {Started: true}}, "failure", syncPassUnfinished},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newSyncRig(t)
			g.mkSandboxDir(syncRemote)
			_, sess, _ := g.syncer(syncRemote, true)
			for _, r := range tc.reports {
				if !sess.report(r) {
					t.Fatalf("the gateway refused %+v on a started sync channel", r)
				}
			}
			if err := sess.close(); err != nil {
				t.Fatal(err)
			}
			outcome, data := g.audit.row(t)
			if got, _ := data["error"].(string); outcome != tc.want || got != tc.wantErr {
				t.Errorf("row = %s, error %q; want %s, error %q", outcome, got, tc.want, tc.wantErr)
			}
		})
	}
}

// Ctrl-C in the middle of a pass ends the loop with no account of that pass, so
// the start the loop sent first makes the row a failure.
func TestSyncGateway_AnInterruptedPassRecordsAFailure(t *testing.T) {
	g := newSyncRig(t)
	g.mkSandboxDir(syncRemote)
	sy, sess, _ := g.syncer(syncRemote, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runSyncLoop(ctx, syncCmd(nil), sy, sess, syncFlags{}, nil); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v, want interrupted", err)
	}
	if err := sess.close(); err != nil {
		t.Fatal(err)
	}
	if outcome, data := g.audit.row(t); outcome != "failure" || data["error"] != syncPassUnfinished {
		t.Errorf("row = %s, error %v; want failure, %q", outcome, data["error"], syncPassUnfinished)
	}
}
