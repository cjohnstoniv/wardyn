// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"unicode"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gitEmailLocal makes a git-safe email local-part from a principal (e.g.
// "local:cjohn" -> "local_cjohn"): keep alphanumerics and a few safe symbols,
// map everything else to '_', so the synthesized GIT_AUTHOR_EMAIL is well-formed.
func gitEmailLocal(principal string) string {
	if principal == "" {
		return "operator"
	}
	b := make([]byte, 0, len(principal))
	for i := 0; i < len(principal); i++ {
		c := principal[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}

// repoFieldSafe rejects a repo slug/URL that contains any ASCII control
// character or whitespace. run.Repo is attacker-influenceable run-request text
// and flows verbatim into an in-sandbox `git clone "$WARDYN_REPO_URL"`; the
// agent-run scripts always double-quote it, but we still refuse control/space
// bytes so a slug can never smuggle a newline, NUL, or argument break into the
// sandbox env or the clone command. Fail closed: anything unexpected means the
// repo env is simply not surfaced and the agent runs in an empty workspace.
// stricter than the old hand-rolled scan — unicode.IsSpace also
// rejects Unicode space separators (U+2000-200A, U+3000, ...) the old fixed
// list let through; approved as a hardening, not a regression, for this
// trust-boundary check.
func repoFieldSafe(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	})
}

// repoCloneURL derives a git clone URL from a (already sanitized) repo slug.
//   - If the slug is already a URL (contains "://"), it is passed through as-is.
//   - Otherwise, if it matches a bare <org>/<name> GitHub slug, an https GitHub
//     clone URL is built.
//   - Anything else yields "" (no clone URL; the slug is still surfaced as
//     audit metadata and the agent runs in an empty workspace).
//
// LIMITATION (v0.1): non-URL slugs are assumed to be GitHub. The brokered git
// credential helper (wardyn-git-helper) and the demo egress allowlist are
// GitHub-scoped, so cross-host cloning of a bare slug is out of scope for now;
// pass a full https:// URL (and allowlist its host) to clone elsewhere.
//
// SSH: an ssh://[user@]host/… or scp-form user@host:path clone URL is accepted
// ONLY when the host is a supported SSH-over-443 provider (sshOver443Endpoint:
// GitHub / Azure DevOps). The URL passes through VERBATIM — the agent-run sandbox
// (not this URL) supplies the minted key, known_hosts and the :443 ProxyCommand;
// the run's ssh_key grant (maybeSSHKeyGrant) authorizes it. Any other transport
// (file://, git's ext::/fd:: helpers, an unsupported SSH host, or an explicit
// non-443 SSH port) fails closed.
func repoCloneURL(slug string) string {
	if strings.Contains(slug, "://") {
		if strings.HasPrefix(slug, "https://") || strings.HasPrefix(slug, "http://") {
			return slug
		}
		if strings.HasPrefix(slug, "ssh://") {
			if host, ok := sshCloneHost(slug); ok {
				if _, ok := sshOver443Endpoint(host); ok {
					return slug
				}
			}
		}
		return ""
	}
	// scp-form user@host:path — has '@' and ':' but no scheme.
	if strings.ContainsRune(slug, '@') && strings.ContainsRune(slug, ':') {
		if host, ok := sshCloneHost(slug); ok {
			if _, ok := sshOver443Endpoint(host); ok {
				return slug
			}
		}
		return ""
	}
	// Bare <org>/<name>: exactly two non-empty path segments, no extra slashes.
	parts := strings.Split(slug, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return "https://github.com/" + slug + ".git"
	}
	return ""
}

// buildRepoRecords assembles the WARDYN_REPOS env value: newline-delimited,
// tab-separated <url>\t<dest>\t<slug> records the agent-run entrypoint iterates to
// clone each repo. Sources are the legacy single run.Repo (first, keeping its
// default ~/work/<name> dest) plus each onboarded WorkspaceRepo (already
// onboarding-gated). Every field is repoFieldSafe (no whitespace/control chars) so
// the tab/newline framing cannot be smuggled past; every dest is a validated
// allowed-prefix target, deduped so two repos never target one directory. A slug
// with no derivable clone URL, an unsafe/out-of-prefix dest, or a duplicate dest is
// skipped. Returns "" when there is nothing to clone.
func buildRepoRecords(legacyRepo string, repos []types.WorkspaceRepo) string {
	const workRoot = "/home/agent/work"
	seenDest := map[string]bool{}
	var b strings.Builder
	add := func(slug, dest string) {
		slug = strings.TrimSpace(slug)
		if slug == "" || !repoFieldSafe(slug) {
			return
		}
		url := repoCloneURL(slug)
		if url == "" {
			return
		}
		if dest == "" {
			name := strings.TrimSuffix(url[strings.LastIndex(url, "/")+1:], ".git")
			if name == "" {
				name = "repo"
			}
			dest = workRoot + "/" + name
		}
		if !repoFieldSafe(dest) || runner.ValidateTarget(dest) != nil || seenDest[dest] {
			return
		}
		seenDest[dest] = true
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(url)
		b.WriteByte('\t')
		b.WriteString(dest)
		b.WriteByte('\t')
		b.WriteString(slug)
	}
	add(legacyRepo, "") // legacy single repo → default dest
	for _, wr := range repos {
		add(wr.Repo, wr.Target)
	}
	return b.String()
}

// injectionRuleFromScope decodes an api_key grant scope into its proxy-side
// injection rule. Mirrors the broker's apiKeyScope shape (host, header,
// format, secret_name) with the same defaults.
func injectionRuleFromScope(scope json.RawMessage) (egress.InjectionRule, error) {
	var sc struct {
		Host       string `json:"host"`
		Header     string `json:"header"`
		Format     string `json:"format"`
		SecretName string `json:"secret_name"`
	}
	if err := json.Unmarshal(scope, &sc); err != nil {
		return egress.InjectionRule{}, err
	}
	if sc.Host == "" || sc.SecretName == "" {
		return egress.InjectionRule{}, errors.New("api_key scope requires host and secret_name")
	}
	if sc.Header == "" {
		sc.Header = "Authorization"
	}
	if sc.Format == "" {
		sc.Format = "Bearer %s"
	}
	return egress.InjectionRule{Host: sc.Host, Header: sc.Header, SecretName: sc.SecretName, Format: sc.Format}, nil
}

// githubScopeRepos decodes a github_token grant scope {"repos":[...]} and returns
// the "<org>/<repo>" entries (mirrors the broker's githubScope shape). Best-effort:
// a malformed scope or a repo that isn't exactly "<org>/<repo>" is skipped, since
// this feeds the git-broker allowlist where a missing key simply 403s (fail closed).
func githubScopeRepos(scope json.RawMessage) []string {
	var sc struct {
		Repos []string `json:"repos"`
	}
	if err := json.Unmarshal(scope, &sc); err != nil {
		return nil
	}
	out := make([]string, 0, len(sc.Repos))
	for _, r := range sc.Repos {
		r = strings.TrimSpace(r)
		if parts := strings.Split(r, "/"); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			out = append(out, r)
		}
	}
	return out
}

// gitPATScopeFields decodes a git_pat grant scope {host, secret_name, username?}.
// host and secret_name are REQUIRED (fail closed); mirrors the broker's
// gitPATScope shape. Used by policy validation, inline-secret checks, compose
// grounding, and the sandbox env wiring so all agree on the scope contract.
func gitPATScopeFields(scope json.RawMessage) (host, secretName, username string, err error) {
	var sc struct {
		Host       string `json:"host"`
		SecretName string `json:"secret_name"`
		Username   string `json:"username"`
	}
	if err = json.Unmarshal(scope, &sc); err != nil {
		return "", "", "", err
	}
	if sc.Host == "" || sc.SecretName == "" {
		return "", "", "", errors.New("git_pat scope requires host and secret_name")
	}
	return sc.Host, sc.SecretName, sc.Username, nil
}

// sshKeyScopeFields decodes an ssh_key grant scope
// {host, key_secret_ref, username?, known_hosts_secret_ref?}. host and
// key_secret_ref are REQUIRED (fail closed); mirrors the broker's sshKeyScope
// shape. Used by policy validation, inline-secret checks, and the sandbox env
// wiring so all agree on the scope contract.
func sshKeyScopeFields(scope json.RawMessage) (host, keySecretRef, username, knownHostsSecretRef string, err error) {
	var sc struct {
		Host                string `json:"host"`
		KeySecretRef        string `json:"key_secret_ref"`
		Username            string `json:"username"`
		KnownHostsSecretRef string `json:"known_hosts_secret_ref"`
	}
	if err = json.Unmarshal(scope, &sc); err != nil {
		return "", "", "", "", err
	}
	if sc.Host == "" || sc.KeySecretRef == "" {
		return "", "", "", "", errors.New("ssh_key scope requires host and key_secret_ref")
	}
	return sc.Host, sc.KeySecretRef, sc.Username, sc.KnownHostsSecretRef, nil
}

// sshOver443Endpoint maps a supported SCM host to its SSH-over-443 endpoint
// (host:443) so git-over-SSH reuses the existing CONNECT-443 egress lane with NO
// port-policy change. Returns ok=false for an unsupported host — the SSH lane is
// deliberately limited to the two providers that publish an :443 SSH endpoint
// (GitHub, Azure DevOps); a custom GHES/ADO-Server host would need port-22 egress
// and is out of scope for the SSH lane. The returned endpoint is PORT-QUALIFIED
// (":443") on purpose: it is added to the egress allowlist so it matches ONLY
// :443, closing the bare-entry "matches any port" permissiveness for SSH hosts.
func sshOver443Endpoint(host string) (endpoint string, ok bool) {
	switch strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), ".")) {
	case "github.com", "ssh.github.com":
		return "ssh.github.com:443", true
	case "dev.azure.com", "ssh.dev.azure.com":
		return "ssh.dev.azure.com:443", true
	}
	return "", false
}

// sshCloneHost extracts the host git will dial from an SSH clone URL — either
// ssh://[user@]host[:port]/path or scp-form [user@]host:path. It does NOT validate
// the host is a supported provider (callers gate on sshOver443Endpoint). ok=false
// for a non-SSH string or an explicit non-443 ssh:// port (a port-22 URL would
// override the sandbox's Port-443 ssh_config and defeat the SSH-over-443 egress
// lane, so it fails closed here).
func sshCloneHost(raw string) (host string, ok bool) {
	if strings.HasPrefix(raw, "ssh://") {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return "", false
		}
		if p := u.Port(); p != "" && p != "443" {
			return "", false
		}
		return strings.ToLower(u.Hostname()), true
	}
	if strings.Contains(raw, "://") {
		return "", false // some other scheme, not scp-form
	}
	// scp-form: [user@]host:path — exactly one host, then ':' then a non-empty path.
	s := raw
	if at := strings.IndexByte(s, '@'); at >= 0 {
		s = s[at+1:]
	}
	colon := strings.IndexByte(s, ':')
	if colon <= 0 || colon == len(s)-1 {
		return "", false
	}
	host = strings.ToLower(s[:colon])
	if strings.ContainsAny(host, "/@") {
		return "", false
	}
	return host, true
}

// canonicalSSHKeySecret maps a supported SSH host (either the primary or its
// ssh.<host> form) to the canonical ssh-key-<host-slug> secret name — the SAME
// convention setup.sh's SCM import writes and setup.go documents. Keying the
// secret off the canonical provider (not the raw URL host) is what lets an
// operator store ONE `ssh-key-github-com` and clone either github.com or
// ssh.github.com URL forms.
func canonicalSSHKeySecret(host string) (secretName string, ok bool) {
	switch strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), ".")) {
	case "github.com", "ssh.github.com":
		return "ssh-key-github-com", true
	case "dev.azure.com", "ssh.dev.azure.com":
		return "ssh-key-dev-azure-com", true
	}
	return "", false
}

// adoEgressDomains returns the Azure DevOps egress bundle {dev.azure.com,
// *.visualstudio.com} when host is an ADO host (either the modern
// dev.azure.com or a legacy org.visualstudio.com), else nil. Unlike GitHub
// (whose egress is baked into the example policies' static AllowedDomains),
// nothing today adds ADO's hosts for a git_pat grant, so dev.azure.com /
// *.visualstudio.com are in NO example policy — a plain ADO PAT grant would
// mint a credential the sandbox then has no egress to use it with. Both hosts
// are returned together (not just the matched one) because an org may clone
// via one host while ADO's REST/API surface uses the other.
func adoEgressDomains(host string) []string {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h == "dev.azure.com" || strings.HasSuffix(h, ".visualstudio.com") {
		return []string{"dev.azure.com", "*.visualstudio.com"}
	}
	return nil
}
