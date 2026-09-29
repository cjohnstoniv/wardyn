// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// integration_fold_test.go is the migration proof for the base-component
// reshape (track B, B1): a SiteConfig document carrying EVERY pre-fold row
// shape — azure_openai, github_app, git_host, one of each generic
// category, artifact_mirror and host_proxy — decodes into the new shape
// (read-time fold), the two topology categories come out ABSENT, old stored
// wire data stays readable, and a write emits the new shape only.

// legacySiteConfigFixture is a stored pre-base-component SiteConfig document,
// verbatim in the OLD wire shape ({category, type, hosts, header, format,
// credentials, config}).
const legacySiteConfigFixture = `{
  "scm_hosts": ["github.example.com"],
  "integrations": [
    {"id": "acme-azure", "name": "Azure OpenAI",
     "category": "ai_provider", "type": "azure_openai",
     "credentials": {"api_key": "azure-key"}},
    {"id": "github_app", "name": "GitHub App",
     "category": "scm_host", "type": "github_app",
     "credentials": {"app_id": "github-app-id", "app_key": "github-app-key"},
     "config": {"host": "github.com"}},
    {"id": "git_host:github.example.com", "name": "github.example.com",
     "category": "scm_host", "type": "git_host",
     "credentials": {"pat": "git-pat-github-example-com", "ssh_key": "ssh-key-github-example-com"}},

    {"id": "corp-artifactory", "name": "Corp Artifactory",
     "category": "package_feed", "type": "artifactory",
     "hosts": ["artifactory.corp.internal"],
     "header": "X-JFrog-Art-Api",
     "credentials": {"token": "artifactory-token"},
     "docs": "https://wiki.corp.internal/artifactory"},
    {"id": "corp-registry", "name": "Corp Registry",
     "category": "container_registry", "type": "harbor",
     "hosts": ["registry.corp.internal"],
     "header": "Authorization", "format": "Bearer %s",
     "credentials": {"token": "registry-token"}},
    {"id": "corp-aws", "name": "Corp AWS",
     "category": "cloud_provider", "type": "aws",
     "hosts": ["*.amazonaws.com"]},
    {"id": "corp-postgres", "name": "Prod Postgres",
     "category": "data_store", "type": "postgres",
     "hosts": ["db.corp.internal:5432"]},
    {"id": "corp-mcp", "name": "Corp MCP",
     "category": "mcp_server", "type": "corp-mcp",
     "hosts": ["mcp.corp.internal"],
     "header": "Authorization", "format": "Bearer %s",
     "credentials": {"token": "mcp-token"}},
    {"id": "corp-jira", "name": "Jira",
     "category": "work_tracking", "type": "jira",
     "hosts": ["jira.corp.internal"],
     "header": "Authorization", "format": "Bearer %s",
     "credentials": {"token": "jira-token"},
     "disabled": true},
    {"id": "corp-datadog", "name": "Datadog",
     "category": "observability", "type": "datadog",
     "hosts": ["api.datadoghq.com"],
     "header": "DD-API-KEY",
     "credentials": {"token": "dd-api-key"},
     "disabled_capabilities": ["credential"]},
    {"id": "corp-tool", "name": "Corp Tool",
     "category": "other_service", "type": "corp-tool",
     "hosts": ["tool.corp.internal"],
     "default_for": []},

    {"id": "artifact_mirror:artifactory.corp", "name": "artifactory.corp",
     "category": "artifact_mirror", "type": "artifact_mirror",
     "credentials": {"token": "npm-token"},
     "config": {"ecosystems": ["npm", "pip"]}},
    {"id": "host_proxy", "name": "Corporate upstream proxy",
     "category": "host_proxy", "type": "host_proxy",
     "credentials": {"secret": "corp-proxy-url"}}
  ]
}`

func foldFixture(t *testing.T) SiteConfig {
	t.Helper()
	var sc SiteConfig
	if err := json.Unmarshal([]byte(legacySiteConfigFixture), &sc); err != nil {
		t.Fatalf("legacy SiteConfig no longer decodes: %v", err)
	}
	return sc
}

func fixtureRow(t *testing.T, sc SiteConfig, id string) Integration {
	t.Helper()
	for _, in := range sc.Integrations {
		if in.ID == id {
			return in
		}
	}
	t.Fatalf("row %q missing after fold; got %d rows", id, len(sc.Integrations))
	return Integration{}
}

// TestIntegrationFold_TopologyRowsAbsent: the two legacy topology categories
// are DROPPED from this surface by the fold — their config lives (and stays)
// under Corporate network.
func TestIntegrationFold_TopologyRowsAbsent(t *testing.T) {
	sc := foldFixture(t)
	if want, got := 11, len(sc.Integrations); got != want {
		t.Errorf("rows after fold = %d, want %d (13 legacy rows minus the 2 topology rows)", got, want)
	}
	for _, in := range sc.Integrations {
		if in.ID == "artifact_mirror:artifactory.corp" || in.ID == "host_proxy" {
			t.Errorf("topology row %q survived the fold — it must be dropped from this surface", in.ID)
		}
	}
	// The rest of the document is untouched by the integrations fold.
	if len(sc.ScmHosts) != 1 || sc.ScmHosts[0] != "github.example.com" {
		t.Errorf("scm_hosts = %v, want carried through unchanged", sc.ScmHosts)
	}
}

// TestIntegrationFold_UnknownKindPassesThrough: a kind Wardyn stopped honoring
// (azure_openai in 0.5) still DESERIALIZES with its kind and credential
// intact — the read-time fold is a passthrough, so nobody's config is silently
// rewritten — and validateIntegrationWrite refuses it like any other unknown
// kind on the next write.
func TestIntegrationFold_UnknownKindPassesThrough(t *testing.T) {
	azure := fixtureRow(t, foldFixture(t), "acme-azure")
	if azure.Kind != "azure_openai" || azure.RoleSecret("api_key") != "azure-key" {
		t.Errorf("azure row = %+v", azure)
	}
	if ClosedIntegrationKinds[azure.Kind] {
		t.Errorf("azure_openai is still a closed kind — it was removed in 0.5")
	}
}

// TestIntegrationFold_SCMKinds: github_app/git_host credentials fold to secret
// rows with NO delivery — their lane is the SCM broker's own bespoke
// transport, which cannot be declared as one of the three modes.
func TestIntegrationFold_SCMKinds(t *testing.T) {
	sc := foldFixture(t)

	gh := fixtureRow(t, sc, "github_app")
	if gh.Kind != IntegrationKindGitHubApp {
		t.Errorf("kind = %q", gh.Kind)
	}
	if gh.RoleSecret("app_id") != "github-app-id" || gh.RoleSecret("app_key") != "github-app-key" {
		t.Errorf("secrets = %+v", gh.Secrets)
	}
	for _, s := range gh.Secrets {
		if s.Delivery != nil {
			t.Errorf("github_app %s delivery = %+v, want nil (brokered lane)", s.Role, s.Delivery)
		}
	}
	if gh.Config["host"] != "github.com" {
		t.Errorf("config = %+v, want the legacy host marker preserved", gh.Config)
	}

	git := fixtureRow(t, sc, "git_host:github.example.com")
	if git.Kind != IntegrationKindGitHost {
		t.Errorf("kind = %q", git.Kind)
	}
	if git.RoleSecret("pat") != "git-pat-github-example-com" || git.RoleSecret("ssh_key") != "ssh-key-github-example-com" {
		t.Errorf("secrets = %+v", git.Secrets)
	}
	for _, s := range git.Secrets {
		if s.Delivery != nil {
			t.Errorf("git_host %s delivery = %+v, want nil (brokered lane)", s.Role, s.Delivery)
		}
	}
}

// TestIntegrationFold_GenericKinds: every generic-category row's kind = its
// old open type slug (the category dies), hosts become egress, and the
// header/format pair folds onto the token secret as proxy_header delivery —
// with an EMPTY legacy format materialized as "%s" (the raw secret IS the
// value; leaving it empty would read back as "Bearer %s" downstream).
func TestIntegrationFold_GenericKinds(t *testing.T) {
	sc := foldFixture(t)

	art := fixtureRow(t, sc, "corp-artifactory")
	if art.Kind != "artifactory" {
		t.Errorf("kind = %q, want the old open type slug", art.Kind)
	}
	if len(art.Egress) != 1 || art.Egress[0] != "artifactory.corp.internal" {
		t.Errorf("egress = %v, want the legacy hosts", art.Egress)
	}
	if d := art.Secrets[0].Delivery; d == nil || d.Mode != DeliveryProxyHeader || d.Header != "X-JFrog-Art-Api" || d.Format != "%s" {
		t.Errorf("delivery = %+v, want proxy_header X-JFrog-Art-Api with the EMPTY legacy format materialized as %%s", art.Secrets[0].Delivery)
	}
	if art.Docs != "https://wiki.corp.internal/artifactory" {
		t.Errorf("docs = %q, want preserved", art.Docs)
	}

	reg := fixtureRow(t, sc, "corp-registry")
	if secret, header, format, ok := reg.HeaderSecret(); !ok || secret != "registry-token" || header != "Authorization" || format != "Bearer %s" {
		t.Errorf("registry HeaderSecret = %q/%q/%q ok=%v", secret, header, format, ok)
	}

	// Egress-only rows (no header): the secret-less shapes systems that
	// authenticate outside HTTP take — wildcard and port entries stay legal.
	aws := fixtureRow(t, sc, "corp-aws")
	if aws.Kind != "aws" || len(aws.Secrets) != 0 || aws.Egress[0] != "*.amazonaws.com" {
		t.Errorf("aws row = %+v", aws)
	}
	pg := fixtureRow(t, sc, "corp-postgres")
	if pg.Kind != "postgres" || len(pg.Secrets) != 0 || pg.Egress[0] != "db.corp.internal:5432" {
		t.Errorf("postgres row = %+v", pg)
	}

	mcp := fixtureRow(t, sc, "corp-mcp")
	if mcp.Kind != "corp-mcp" || mcp.RoleSecret("token") != "mcp-token" {
		t.Errorf("mcp row = %+v", mcp)
	}

	// Cross-cutting flags survive the fold verbatim.
	jira := fixtureRow(t, sc, "corp-jira")
	if jira.Kind != "jira" || !jira.Disabled {
		t.Errorf("jira row = %+v, want Disabled preserved", jira)
	}
	dd := fixtureRow(t, sc, "corp-datadog")
	if dd.Kind != "datadog" || len(dd.DisabledCapabilities) != 1 || dd.DisabledCapabilities[0] != "credential" {
		t.Errorf("datadog row = %+v, want DisabledCapabilities preserved", dd)
	}
	if _, header, _, ok := dd.HeaderSecret(); !ok || header != "DD-API-KEY" {
		t.Errorf("datadog HeaderSecret header = %q ok=%v, want the custom credential header", header, ok)
	}

	tool := fixtureRow(t, sc, "corp-tool")
	if tool.Kind != "corp-tool" || len(tool.Secrets) != 0 {
		t.Errorf("other_service row = %+v", tool)
	}
}

// TestIntegrationFold_WriteEmitsNewShapeOnly: one-way, write-new. Marshaling
// the folded document emits no legacy key, and the emitted document decodes
// back byte-equal (a NEW-shape doc is a fixed point of the fold).
func TestIntegrationFold_WriteEmitsNewShapeOnly(t *testing.T) {
	sc := foldFixture(t)
	out, err := json.Marshal(sc)
	if err != nil {
		t.Fatalf("marshal folded config: %v", err)
	}
	// header/format legitimately reappear INSIDE delivery objects; the
	// row-level legacy markers below exist only in the old shape.
	for _, legacyKey := range []string{`"category"`, `"type"`, `"hosts"`, `"credentials"`} {
		if strings.Contains(string(out), legacyKey) {
			t.Errorf("marshal emitted legacy key %s — writes must be new-shape only", legacyKey)
		}
	}
	var again SiteConfig
	if err := json.Unmarshal(out, &again); err != nil {
		t.Fatalf("re-decode emitted document: %v", err)
	}
	if len(again.Integrations) != len(sc.Integrations) {
		t.Fatalf("round-trip changed the row count: %d -> %d", len(sc.Integrations), len(again.Integrations))
	}
	for i := range sc.Integrations {
		a, _ := json.Marshal(sc.Integrations[i])
		b, _ := json.Marshal(again.Integrations[i])
		if string(a) != string(b) {
			t.Errorf("row %s changed across the write round-trip:\n  %s\n  %s", sc.Integrations[i].ID, a, b)
		}
	}
}

// TestIntegrationFold_NewShapeRowsPassThrough: a base-component row decodes
// verbatim — including one whose generic slug collides with a legacy topology
// name, which must NOT be dropped (only legacy-SHAPED topology rows are).
func TestIntegrationFold_NewShapeRowsPassThrough(t *testing.T) {
	doc := `{"integrations": [
	  {"id": "my-mirror", "name": "My Mirror", "kind": "artifact_mirror",
	   "egress": ["mirror.corp.internal"],
	   "secrets": [{"role": "token", "secret_name": "mirror-token",
	                "delivery": {"mode": "proxy_header", "header": "Authorization", "format": "Bearer %s"}}],
	   "probe": {"method": "GET", "url": "https://mirror.corp.internal/health"}},
	  {"id": "corp-vault", "kind": "vault",
	   "secrets": [{"role": "token", "secret_name": "vault-token",
	                "delivery": {"mode": "resident_env", "var": "VAULT_TOKEN"}},
	               {"role": "ca", "secret_name": "vault-ca",
	                "delivery": {"mode": "resident_file", "path": "/etc/vault/ca.pem"}}]}
	]}`
	var sc SiteConfig
	if err := json.Unmarshal([]byte(doc), &sc); err != nil {
		t.Fatalf("decode new-shape doc: %v", err)
	}
	if len(sc.Integrations) != 2 {
		t.Fatalf("rows = %d, want 2 (a new-shape row is never dropped, whatever its slug)", len(sc.Integrations))
	}
	// The `probe` key in the fixture below is simply ignored: there is no
	// verification-probe framework. An unknown key must not fail the decode — a
	// row stored by an older release still loads.
	mirror := fixtureRow(t, sc, "my-mirror")
	if mirror.Kind != "artifact_mirror" {
		t.Errorf("mirror row = %+v", mirror)
	}
	vault := fixtureRow(t, sc, "corp-vault")
	if d := vault.Secrets[0].Delivery; d == nil || d.Mode != DeliveryResidentEnv || d.Var != "VAULT_TOKEN" {
		t.Errorf("vault token delivery = %+v", vault.Secrets[0].Delivery)
	}
	if d := vault.Secrets[1].Delivery; d == nil || d.Mode != DeliveryResidentFile || d.Path != "/etc/vault/ca.pem" {
		t.Errorf("vault ca delivery = %+v", vault.Secrets[1].Delivery)
	}
}

// TestIntegrationFold_SecretsOrderDeterministic: the fold sorts credential
// roles, so repeated reads of the same stored document produce the same
// secrets order (the map had none).
func TestIntegrationFold_SecretsOrderDeterministic(t *testing.T) {
	row := `{"id": "github_app", "category": "scm_host", "type": "github_app",
	         "credentials": {"app_key": "k", "app_id": "i"}}`
	for range 8 {
		var in Integration
		if err := json.Unmarshal([]byte(row), &in); err != nil {
			t.Fatal(err)
		}
		if in.Secrets[0].Role != "app_id" || in.Secrets[1].Role != "app_key" {
			t.Fatalf("secrets order = [%s %s], want sorted [app_id app_key]", in.Secrets[0].Role, in.Secrets[1].Role)
		}
	}
}
