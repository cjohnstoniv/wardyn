// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRemoteSinkActivity_OnlyAllowedDecisions: the Activity flag a record carries to the runner is what moves
// the run's idle clock, so a refused request, a hold, a token-count row and the approvals poll must not set it.
func TestRemoteSinkActivity_OnlyAllowedDecisions(t *testing.T) {
	cases := []struct {
		name string
		d    egress.DecisionLog
		want bool
	}{
		{"policy allow", egress.DecisionLog{Decision: egress.Allow, RuleSource: "policy:allowlist"}, true},
		{"brokered mint allow", egress.DecisionLog{Decision: egress.Allow, RuleSource: "brokered:mint"}, true},
		{"policy deny", egress.DecisionLog{Decision: egress.Deny, RuleSource: "policy:denied"}, false},
		{"builtin deny", egress.DecisionLog{Decision: egress.Deny, RuleSource: "builtin:private-ip"}, false},
		{"approvals poll allow", egress.DecisionLog{Decision: egress.Allow, RuleSource: ruleSourceApprovals}, false},
		{"reauth timeout deny", egress.DecisionLog{Decision: egress.Deny, RuleSource: ruleSourceCredentialReauthTimeout}, false},
		{"model usage row", egress.DecisionLog{Decision: egress.Allow, RuleSource: ruleSourceLLMUsage, Usage: &egress.ModelUsage{}}, false},
	}
	rs, f := startedSink(t)
	c := accept(t, rs, f.next(), 1, 0, int64(len(cases)), 1<<20)
	for _, tc := range cases {
		id, err := rs.reserve(t.Context(), 1024)
		if err != nil {
			t.Fatalf("%s: reserve: %v", tc.name, err)
		}
		tc.d.Request.AuditReservation = id
		tc.d.Request.Host = "h.test"
		rs.emit(tc.d)
		fr := readUntil(t, c, FrameRecord)
		// readUntil acks records before the wanted type, never the wanted one itself.
		writeFrame(t, c, SinkFrame{T: FrameAck, Gen: fr.Gen, Seq: fr.Seq, Disposition: "appended"})
		var body RecordBody
		if err := json.Unmarshal(fr.Body, &body); err != nil {
			t.Fatalf("%s: decode record: %v", tc.name, err)
		}
		if !body.Admitted {
			t.Fatalf("%s: record not admitted; the test needs an admitted record", tc.name)
		}
		if body.Activity != tc.want {
			t.Errorf("%s: Activity = %v, want %v", tc.name, body.Activity, tc.want)
		}
	}
}

// TestRemoteSinkActivity_IgnoredHosts: an allowed request to a host on the run's ignore list does not move the
// idle clock, an empty list counts it as before, and the list reaches the sink from the proxy's Config.
func TestRemoteSinkActivity_IgnoredHosts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ignore []string
		host   string
		want   bool
	}{
		{"listed host", []string{"mask.icloud.com"}, "mask.icloud.com", false},
		{"unlisted host", []string{"mask.icloud.com"}, "github.com", true},
		{"empty list", nil, "mask.icloud.com", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs, f := startedSink(t)
			rs.activityIgnore = tc.ignore
			c := accept(t, rs, f.next(), 1, 0, 1, 1<<20)
			id, err := rs.reserve(t.Context(), 1024)
			if err != nil {
				t.Fatal(err)
			}
			rs.emit(egress.DecisionLog{Request: egress.Request{AuditReservation: id, Host: tc.host}, Decision: egress.Allow, RuleSource: "policy:allowlist"})
			fr := readUntil(t, c, FrameRecord)
			var body RecordBody
			if err := json.Unmarshal(fr.Body, &body); err != nil {
				t.Fatal(err)
			}
			if body.Activity != tc.want {
				t.Errorf("Activity = %v, want %v", body.Activity, tc.want)
			}
		})
	}
}

// An allow-all run's CONNECT to an ignored host is allowed and recorded, and is not activity; the same host
// with no list counts. This is a macOS guest's boot traffic on a run that denies nothing.
func TestConnect_IgnoredHostAllowedIsNotActivity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ignore []string
		want   bool
	}{
		{"ignored", []string{"mask.icloud.com"}, false},
		{"empty list", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs, f := startedSink(t)
			rs.activityIgnore = tc.ignore
			c := accept(t, rs, f.next(), 1, 0, 4, 1<<20)
			p := newProxy(Options{
				RunID:    uuid.New(),
				Policy:   CompilePolicy(types.RunPolicySpec{AllowAllEgress: true}),
				Sink:     &decisionSink{remote: rs, out: &bytes.Buffer{}},
				Resolver: publicResolver{},
				Dial:     redirectDial(startEcho(t)),
			})
			srv := httptest.NewServer(p)
			defer srv.Close()
			conn, status := connectThrough(t, srv.URL, "mask.icloud.com:443")
			defer conn.Close()
			if !strings.Contains(status, "200") {
				t.Fatalf("CONNECT = %q, want 200", status)
			}
			fr := readUntil(t, c, FrameRecord)
			var body RecordBody
			if err := json.Unmarshal(fr.Body, &body); err != nil {
				t.Fatal(err)
			}
			if body.Decision.Request.Host != "mask.icloud.com" || body.Decision.Decision != egress.Allow || !body.Admitted {
				t.Fatalf("record = %+v, want the admitted allow of mask.icloud.com", body)
			}
			if body.Activity != tc.want {
				t.Errorf("Activity = %v, want %v", body.Activity, tc.want)
			}
		})
	}
}

// The proxy's Config list is what the runner sink judges by.
func TestConfiguredDecisionSink_CarriesActivityIgnoreHosts(t *testing.T) {
	hex32 := strings.Repeat("ab", 32)
	cfg := &Config{RunID: uuid.New(), ActivityIgnoreHosts: []string{"mask.icloud.com"},
		DecisionSink: &DecisionSinkConfig{URL: "tls://127.0.0.1:1", SPKISHA256: hex32, Key: hex32}}
	s, err := configuredDecisionSink(cfg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.remote.close(short(t)) })
	if !slices.Equal(s.remote.activityIgnore, cfg.ActivityIgnoreHosts) {
		t.Errorf("remote sink ignore list = %v, want %v", s.remote.activityIgnore, cfg.ActivityIgnoreHosts)
	}
}
