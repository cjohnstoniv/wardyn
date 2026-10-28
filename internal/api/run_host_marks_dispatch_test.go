// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assertMarksAreThoseOfTheConfigSent decodes the run's recorded run.policy.resolve envelope and
// checks its host marks are what hostMarksOf says of the proxy config the runner received, which is
// the config the proxy then enforces.
func assertMarksAreThoseOfTheConfigSent(t *testing.T, events []types.AuditEvent, runID uuid.UUID, sent runner.SandboxSpec) (*effectivePolicyDatum, *hostMarks) {
	t.Helper()
	ev := findAudit(events, runID, "run.policy.resolve", "success")
	if ev == nil {
		t.Fatalf("dispatch recorded no run.policy.resolve envelope; events=%s", auditDump(events, runID))
	}
	var env effectivePolicyDatum
	if err := json.Unmarshal(ev.Data, &env); err != nil || env.HostMarks == nil {
		t.Fatalf("the envelope carries no host marks (%v): %s", err, ev.Data)
	}
	want := hostMarksOf(sent.ProxyConfig, env.AllowedDomains)
	if !slices.Equal(env.HostMarks.Inspected, want.Inspected) {
		t.Errorf("recorded inspected = %v, the config the runner received marks %v", env.HostMarks.Inspected, want.Inspected)
	}
	if !slices.Equal(env.HostMarks.Terminated, want.Terminated) {
		t.Errorf("recorded terminated = %v, the config the runner received terminates %v", env.HostMarks.Terminated, want.Terminated)
	}
	if !slices.Equal(env.HostMarks.Outside, want.Outside) {
		t.Errorf("recorded outside = %v, the config the runner received leaves %v outside the allowlist", env.HostMarks.Outside, want.Outside)
	}
	return &env, env.HostMarks
}

// terminatesHost reports whether a terminated list names host, on any port or scoped to one.
func terminatesHost(terminated []string, host string) bool {
	return slices.ContainsFunc(terminated, func(e string) bool { return e == host || strings.HasPrefix(e, host+":") })
}

// The marks are recorded from the proxy config dispatch sends, after the artifact redirect widened
// the run: the corp mirror the redirect substituted is Inspected, and a host nothing terminates is
// Tunnel only.
func TestDispatchRecordsTheMarksOfTheConfigSent_ArtifactMirror(t *testing.T) {
	_, sent, events, runID := runWalledDispatch(t, walledDispatch{
		site: govMirrorSite(),
		policy: types.RunPolicySpec{
			AllowedDomains:      []string{"registry.npmjs.org", "pypi.org"},
			MinConfinementClass: types.CC1,
		},
	})
	env, marks := assertMarksAreThoseOfTheConfigSent(t, events, runID, sent)
	if !slices.Contains(env.AllowedDomains, govCorpMirrorEntry) {
		t.Fatalf("the artifact redirect did not widen this run (allowed=%v); the fixture is broken", env.AllowedDomains)
	}
	if !slices.Contains(marks.Inspected, govCorpMirrorEntry) {
		t.Errorf("inspected = %v, want the corp mirror %s", marks.Inspected, govCorpMirrorEntry)
	}
	if slices.Contains(marks.Inspected, "pypi.org") {
		t.Errorf("inspected = %v, pypi.org is Tunnel only", marks.Inspected)
	}
	if !terminatesHost(marks.Terminated, govCorpMirror) {
		t.Errorf("terminated = %v, want the corp mirror", marks.Terminated)
	}
}

// A file_secret placeholder adds its host to MITMHosts after the policy is snapshotted, so the host
// is Inspected only if the marks are computed from the config dispatch finally sends. With the
// switch off the same grant is delivered as a file value and no host is terminated.
func TestDispatchRecordsTheMarksOfTheConfigSent_FilePlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
		want []string
	}{
		{"placeholders on", true, []string{placeholderHost}},
		{"placeholders off", false, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, sent, events, runID := runWalledDispatch(t, walledDispatch{
				policy: types.RunPolicySpec{
					AllowedDomains:      []string{placeholderHost, "pypi.org"},
					MinConfinementClass: types.CC1,
					EligibleGrants:      []types.GrantSpec{hostScoped(t, fileSecretGrant("api-token", "walled-token", false))},
				},
				mutate: func(srv *Server) {
					// A one-line value: fileSecretServer's operator rows end in a line break, which
					// cannot ride a request header, so no placeholder would be authored.
					_, sec, _ := fileSecretServer(t)
					if err := sec.For("").Put(context.Background(), "walled-token", []byte("walled-own-value-0001")); err != nil {
						t.Fatal(err)
					}
					srv.cfg.Secrets, srv.cfg.MaskRegistry = sec, secretmask.NewRegistry()
					srv.cfg.FileSecretPlaceholders = tc.on
				},
			})
			_, marks := assertMarksAreThoseOfTheConfigSent(t, events, runID, sent)
			if !slices.Equal(marks.Inspected, tc.want) {
				t.Errorf("inspected = %v, want %v", marks.Inspected, tc.want)
			}
			if tc.on != terminatesHost(marks.Terminated, placeholderHost) {
				t.Errorf("terminated = %v, placeholders on = %v", marks.Terminated, tc.on)
			}
		})
	}
}
