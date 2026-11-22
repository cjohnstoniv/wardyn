// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	grantGatewayHost   = "gateway.corp.example"
	grantTokenHost     = "idp.internal.example"
	grantFormTokenHost = "form-idp.elsewhere.example"
	grantFormBaseHost  = "form-gateway.elsewhere.example"
	grantAudienceHost  = "llm-api.internal.example"
)

// grantHostFixture is a service grant's mint, a refused secret write and an admin's secret delete, their data
// as serviceGrantMintEvent and auditServiceGrantSecret record it, naming four distinct hosts.
func grantHostFixture() []types.AuditEvent {
	data := func(extra map[string]any) json.RawMessage {
		d := map[string]any{"provider": "corp-llm", "provider_uid": "uid-1", "client_id": "wardyn", "token_host": grantTokenHost}
		for k, v := range extra {
			d[k] = v
		}
		return mustJSON(d)
	}
	return []types.AuditEvent{
		{ID: uuid.New(), Action: "model_provider.service_grant.mint", Outcome: "success",
			Data: data(map[string]any{"scope": "llm", "audience": "https://" + grantAudienceHost, "trigger": "refresh", "replica": "r1", "egress": "direct"})},
		{ID: uuid.New(), Action: "model_provider.service_grant.secret_write", Outcome: "failure",
			Data: data(map[string]any{"base_host": grantGatewayHost, "reason": reasonServiceGrantDestinationChanged,
				"body_token_host": grantFormTokenHost, "body_base_host": grantFormBaseHost})},
		{ID: uuid.New(), Action: "model_provider.service_grant.secret_delete", Outcome: "success",
			Data: data(map[string]any{"base_host": grantGatewayHost})},
	}
}

// A security_admin reads a service grant's rows without the token endpoint's host, a mint's audience and scope,
// or a refused form's hosts, each named in redacted_fields, and keeps the configured gateway's base_host. A super
// admin reads every host as recorded. Both feed reads.
func TestAuditHostDetail_ServiceGrantTokenAndFormHostsBelowSuperAdmin(t *testing.T) {
	const mint, write = "model_provider.service_grant.mint", "model_provider.service_grant.secret_write"
	withheld := map[string][]string{
		mint:  {"audience", "scope", "token_host"},
		write: {"body_base_host", "body_token_host", "token_host"},
		"model_provider.service_grant.secret_delete": {"token_host"},
	}
	srv := hostAuditServer(t, grantHostFixture())
	sec := ssoSession(t, "sub-sec", "sec@corp.example", oidc.RoleSecurityAdmin)
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	for _, path := range []string{"/api/v1/audit", "/api/v1/audit/export"} {
		rows := hostAuditRows(t, srv, path, sec)
		if len(rows) != 3 {
			t.Fatalf("%s: security_admin read %d rows, want 3", path, len(rows))
		}
		for _, ev := range rows {
			data := string(ev.Data)
			for _, h := range []string{grantTokenHost, grantFormTokenHost, grantFormBaseHost, grantAudienceHost} {
				if strings.Contains(data, h) {
					t.Errorf("%s: security_admin read %s %s, want no %s", path, ev.Action, data, h)
				}
			}
			if served, want := strings.Contains(data, grantGatewayHost), ev.Action != mint; served != want {
				t.Errorf("%s: security_admin read %s %s: base_host served = %v, want %v", path, ev.Action, data, served, want)
			}
			if got := hostRedactedFields(t, data); !slices.Equal(got, withheld[ev.Action]) {
				t.Errorf("%s: security_admin read %s redacted_fields %v, want %v", path, ev.Action, got, withheld[ev.Action])
			}
		}
		rows = hostAuditRows(t, srv, path, admin)
		if len(rows) != 3 {
			t.Fatalf("%s: super admin read %d rows, want 3", path, len(rows))
		}
		for _, ev := range rows {
			data := string(ev.Data)
			want := []string{grantTokenHost}
			if ev.Action != mint {
				want = append(want, grantGatewayHost)
			} else {
				want = append(want, grantAudienceHost)
			}
			if ev.Action == write {
				want = append(want, grantFormTokenHost, grantFormBaseHost)
			}
			for _, h := range want {
				if !strings.Contains(data, h) {
					t.Errorf("%s: super admin read %s %s, want %s as recorded", path, ev.Action, data, h)
				}
			}
			if strings.Contains(data, auditRedactedFieldsKey) {
				t.Errorf("%s: super admin read %s %s, want no %s", path, ev.Action, data, auditRedactedFieldsKey)
			}
		}
	}
}
