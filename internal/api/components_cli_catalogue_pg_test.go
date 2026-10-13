// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	cliGitHubDef = `{"hosts":["api.github.com"],"cli":{"kind":"github","github":{"capabilities":["issues_read"]}}}`
	cliAWSDef    = `{"hosts":[],"cli":{"kind":"aws","aws":{"start_url":"https://portal-secret.awsapps.com/start","sso_region":"eu-west-1","account_id":"123456789012","role_name":"ReadOnly","region":"eu-west-2"}}}`
	cliAzureDef  = `{"hosts":[],"cli":{"kind":"azure","azure":{"subscriptions":["11111111-2222-3333-4444-555555555555"],"access":"read"}}}`
)

func (e componentsPG) putOrgDef(t *testing.T, name, def string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	w := e.do(t, http.MethodPut, "/api/v1/components/"+id.String(), e.admin, saveComponentBody(name, def))
	if w.Code != http.StatusCreated {
		t.Fatalf("admin creates %q = %d: %s", name, w.Code, w.Body.String())
	}
	return id
}

func (e componentsPG) memberBody(t *testing.T) string {
	t.Helper()
	w := e.do(t, http.MethodGet, "/api/v1/me/components", e.member, "")
	if w.Code != http.StatusOK {
		t.Fatalf("member list = %d: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// TestMyComponents_CLIStateNeverRevealsUngrantedRow: a member's catalogue is the
// same bytes whether or not ungranted CLI rows exist; a grant turns exactly that
// kind to available, lists the row, and shows facts but never the portal URL.
func TestMyComponents_CLIStateNeverRevealsUngrantedRow(t *testing.T) {
	e := newComponentsPG(t)
	before := e.memberBody(t)

	gh := e.putOrgDef(t, "GitHub CLI", cliGitHubDef)
	e.putOrgDef(t, "AWS CLI", cliAWSDef)
	e.putOrgDef(t, "Azure CLI", cliAzureDef)
	if after := e.memberBody(t); after != before {
		t.Fatalf("ungranted CLI rows changed the member's bytes:\nbefore %s\nafter  %s", before, after)
	}

	got := e.memberList(t).CLIs
	kinds := []string{"azure", "github", "aws", "gcloud", "kubectl"}
	if len(got) != len(kinds) {
		t.Fatalf("clis = %d entries, want %d", len(got), len(kinds))
	}
	for i, c := range got {
		want := client.CLIStateNotGranted
		if i >= 3 {
			want = client.CLIStateComingSoon
		}
		if c.Kind != kinds[i] || c.State != want || c.Rows == nil || len(c.Rows) != 0 {
			t.Errorf("clis[%d] = %+v, want %s %s with no rows", i, c, kinds[i], want)
		}
	}

	e.grant(t, "user", "sub-member", capComponent, gh.String(), "allow")
	list := e.memberList(t)
	if c := list.CLIs[1]; c.State != client.CLIStateAvailable || len(c.Rows) != 1 || c.Rows[0] != gh {
		t.Errorf("granted github card = %+v", c)
	}
	if c := list.CLIs[2]; c.State != client.CLIStateNotGranted || len(c.Rows) != 0 {
		t.Errorf("aws card changed by a github grant: %+v", c)
	}
	if len(list.Org) != 1 || list.Org[0].Category != "cli" || list.Org[0].CLI == nil || list.Org[0].CLI.Kind != "github" || list.Org[0].MCP != nil {
		t.Errorf("granted row view = %+v", list.Org)
	}
}

// TestMyComponents_CLIViewHoldsNoPortalURL: the non-secret view of an AWS or
// Azure row names the account, role and region, never the portal or the ids.
func TestMyComponents_CLIViewHoldsNoPortalURL(t *testing.T) {
	e := newComponentsPG(t)
	aws := e.putOrgDef(t, "AWS CLI", cliAWSDef)
	az := e.putOrgDef(t, "Azure CLI", cliAzureDef)
	e.grant(t, "all", "", capComponent, aws.String(), "allow")
	e.grant(t, "all", "", capComponent, az.String(), "allow")
	body := e.memberBody(t)
	for _, leak := range []string{"portal-secret", "11111111-2222"} {
		if strings.Contains(body, leak) {
			t.Errorf("member list carries %q: %s", leak, body)
		}
	}
	for _, want := range []string{`"account_id":"123456789012"`, `"role_name":"ReadOnly"`, `"subscriptions":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("member list lacks %s: %s", want, body)
		}
	}
}
