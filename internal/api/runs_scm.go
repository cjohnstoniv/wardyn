// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"unicode"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/gitremote"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gitEmailLocal makes a git-safe email local-part from a principal (e.g.
// "local:alice" -> "local_alice"): keep alphanumerics and a few safe symbols,
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
// sandbox env or the clone command (Unicode space separators included). Fail
// closed: the repo env is not surfaced and the agent runs in an empty workspace.
// The RULE lives in gitremote.FieldSafe so the door that AUTHORS a repo field and
// the scanner that DETECTS one cannot answer differently about the same value.
func repoFieldSafe(s string) bool {
	return gitremote.FieldSafe(s)
}

// repoField400Charset — DRAFT (M2 canon pending). The write-door refusal for a
// repo-shaped field carrying a control character or whitespace — the charset
// repoFieldSafe guards, because every one of these values flows verbatim into
// an in-sandbox `git clone`/`git checkout` argument. %s is the field name, so
// the workspace door ("source", "ref", "base_image.image",
// "llm_cred.integration_ref") and the library door ("locator", "ref") answer
// the identical sentence about the identical rule instead of four spellings of
// it.
const repoField400Charset = "%s must not contain control characters or whitespace"

// repo400LocatorShape — DRAFT (M2 canon pending). The write-door refusal for a
// locator repoLocatorPathSafe rejects. It is a 400 ("you wrote this wrong"), not
// an admission 422/403: a dot-segment or percent-encoded repository address is
// never a legitimate one on any provider, in legacy open mode included, so the
// door that AUTHORS it refuses it rather than leaving it to the clone.
// %s is the field name — "locator" in the source library, "source" on a
// workspace spec — so one sentence serves both doors.
const repo400LocatorShape = "%s is not a repository address — a repository address carries " +
	`no percent-escapes, no backslash, and no "", "." or ".." path segment`

// repoLocatorPathSafe reports whether a repo locator's PATH is a plain
// repository address. git squashes "." and ".." client-side and sends `%2F` raw,
// so `https://github.com/acme/../evil/repo.git` passes an `https://github.com/acme`
// provider row's prefix match and is cloned, with that row's org credential, as
// `evil/repo`. No prefix match survives a path server and client read differently,
// so the traversable SHAPES are refused, at parseCloneTarget (where every
// admission door resolves a clone URL) and again at the write doors: any "%"
// (%2F sneaks a segment past a decoded compare), any "\" (a separator to some
// clients), any empty, "." or ".." segment. The ONE "%" exception is an Azure
// DevOps address already canonical (canonicalRepoAddress): names there carry
// escapes, and adoscope's name rule has already refused every escape that decodes
// to structure. A locator with no path is unclonable anyway and not handled here.
func repoLocatorPathSafe(raw string, adoServerHosts []string) bool {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, `\`) {
		return false
	}
	if strings.Contains(s, "%") {
		if c, ok := adoscope.CanonicalRepoURL(s, adoServerHosts); !ok || c != s {
			return false
		}
	}
	path := s
	if i := strings.Index(s, "://"); i >= 0 {
		path = ""
		if j := strings.IndexByte(s[i+3:], '/'); j >= 0 {
			path = s[i+3+j+1:]
		}
	} else if i := strings.IndexByte(s, ':'); i >= 0 {
		path = s[i+1:] // scp-form [user@]host:path
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return true
	}
	for _, seg := range strings.Split(path, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// canonicalRepoAddress is the ONE stored spelling of a repository address. An
// Azure DevOps address has each path segment rewritten by adoscope's name rule
// — so a project typed "Payments Platform", pasted "Payments%20Platform" or
// escaped wholesale by a client library is one string — and anything else is
// returned exactly as given. Every door that AUTHORS a repository address calls
// this before it validates, so every gate, the run row and WARDYN_REPOS read
// one spelling; a value it cannot canonicalise keeps its spelling and meets the
// same refusals it always did.
//
// adoServerHosts are the Azure DevOps Server hosts this install's provider rows
// name (adoServerHosts); the Azure DevOps service hosts need no listing.
func canonicalRepoAddress(s string, adoServerHosts []string) string {
	if c, ok := adoscope.CanonicalRepoURL(s, adoServerHosts); ok {
		return c
	}
	return s
}

// canonicalizeRunRepos puts every repository address a create-run body names
// into its stored spelling, before any gate reads one. Both run doors (launch
// and preflight) call it straight after decoding.
func canonicalizeRunRepos(req *createRunRequest, ado adoHostsLoader) {
	values := []string{req.Repo, req.DevcontainerRepo}
	if req.InlinePolicy != nil {
		for _, wr := range req.InlinePolicy.WorkspaceRepos {
			values = append(values, wr.Repo)
		}
	}
	adoServerHosts := ado.forAddresses(values...)
	req.Repo = canonicalRepoAddress(req.Repo, adoServerHosts)
	req.DevcontainerRepo = canonicalRepoAddress(req.DevcontainerRepo, adoServerHosts)
	if req.InlinePolicy != nil {
		canonicalizeWorkspaceRepos(req.InlinePolicy.WorkspaceRepos, adoServerHosts)
	}
}

// canonicalizeWorkspaceRepos is canonicalRepoAddress over a spec's
// workspace_repos, in place.
func canonicalizeWorkspaceRepos(repos []types.WorkspaceRepo, adoServerHosts []string) {
	for i := range repos {
		repos[i].Repo = canonicalRepoAddress(repos[i].Repo, adoServerHosts)
	}
}

// repoDirName is the directory name for a clone whose address ends in leaf:
// the leaf decoded by adoscope's name rule where it decodes (an Azure DevOps
// repository is named "Card Auth (v2).Service", not its escapes), with each run
// of whitespace made "-" so the name stays repoFieldSafe.
func repoDirName(leaf string) string {
	if name, err := adoscope.UnescapeName(leaf); err == nil {
		leaf = name
	}
	return strings.Join(strings.FieldsFunc(leaf, unicode.IsSpace), "-")
}

// repoCloneURL derives a git clone URL from a (already sanitized) repo slug.
//   - If the slug is already a URL (contains "://"), it is passed through as-is.
//   - Otherwise, if it matches a bare <org>/<name> GitHub slug, an https GitHub
//     clone URL is built.
//   - Anything else yields "" (no clone URL; the agent runs in an empty workspace).
//
// LIMITATION: bare slugs are assumed GitHub (the git helper and demo allowlist
// are GitHub-scoped); pass a full https:// URL and allowlist its host otherwise.
// SSH (ssh:// or scp-form) is accepted ONLY for an sshOver443Endpoint host and
// passes VERBATIM — the sandbox supplies key, known_hosts and the :443
// ProxyCommand; maybeSSHKeyGrant authorizes it. Any other transport (file://,
// ext::/fd::, other SSH hosts, a non-443 SSH port) fails closed.
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
// tab-separated <url>\t<dest>\t<slug>\t<ref> records the agent-run entrypoint
// iterates to clone each repo: the legacy run.Repo first (default dest, no
// ref), then each onboarded WorkspaceRepo. Every field is repoFieldSafe so the
// tab/newline framing cannot be smuggled past; every dest is a validated
// allowed-prefix target, deduped so two repos never share a directory. A slug
// with no clone URL, a bad dest or ref, or a duplicate dest is skipped. Ref is
// optional ("" = default branch); clone_one (agent-run-lib.sh) checks it out.
//
// The SECOND return is one sentence per repo dropped for a refused target or a
// taken directory. Run create appends them to the 201's warnings[]; dispatch
// discards them (the run already exists; the slog.Warn is the record there).
func buildRepoRecords(legacyRepo string, repos []types.WorkspaceRepo) (string, []string) {
	const workRoot = "/home/agent/work"
	seenDest := map[string]string{} // dest -> the slug that took it
	var warnings []string
	var b strings.Builder
	add := func(slug, dest, ref string) {
		slug = strings.TrimSpace(slug)
		if slug == "" || !repoFieldSafe(slug) {
			return
		}
		ref = strings.TrimSpace(ref)
		if ref != "" && !repoFieldSafe(ref) {
			return
		}
		url := repoCloneURL(slug)
		if url == "" {
			return
		}
		// A FULL github URL — any spelling the control plane accepts: http://,
		// trailing slash, .git suffix, :port, mixed case — collapses to the one
		// shape agent-run's url.<broker>.insteadOf rewrite prefix-matches
		// ("https://github.com/<org>/<repo>"); any other spelling would dial
		// github.com directly, a route a brokered run does not have. A BARE slug is
		// already this shape and is left untouched, casing included.
		// Deliberate side effect: the key is lowercased, so a full URL's default
		// dest is lowercased too (~/work/hello-world). That is what lets the dest
		// dedup below SEE two spellings of one repo as one; resolve_workdir finds
		// the repo by scanning for .git, never by name.
		if strings.Contains(slug, "://") {
			if key := gitBrokerKeyFromSlug(slug); key != "" {
				slug, url = key, "https://github.com/"+key+".git"
			}
		}
		if dest == "" {
			name := repoDirName(strings.TrimSuffix(url[strings.LastIndex(url, "/")+1:], ".git"))
			if name == "" {
				name = "repo"
			}
			dest = workRoot + "/" + name
		}
		if !repoFieldSafe(dest) {
			return
		}
		if terr := runner.ValidateAuthoredTarget(dest); terr != nil {
			// Loud, for the same reason the dest-collision skip below is, and one
			// the write door cannot cover: the reserved-target rule sits on POST
			// /policies only (validatePolicySpec → 400), and resolvePolicy hands
			// dispatch a STORED spec VERBATIM. A pre-0.7.2 workspace_repos row
			// targeting /home/agent/drive therefore still reaches here, where the
			// repo would otherwise be dropped in silence — a 201, an empty
			// WARDYN_REPOS, and an agent that starts looking for a repo nothing
			// ever cloned. Say it on the 201 (run create appends the returned
			// sentence) and in the log.
			slog.Warn("wardynd: repo clone target is not an allowed destination; dropping the repo",
				slog.String("slug", slug), slog.String("dest", dest), slog.String("err", terr.Error()))
			warnings = append(warnings, fmt.Sprintf(
				"repository %s was NOT cloned: its workspace_repos target %s is refused (%s) — fix the target in the policy and launch again",
				slug, dest, terr))
			return
		}
		if first, taken := seenDest[dest]; taken {
			// Loud, not silent — an unqualified caller (no explicit
			// Target on either source, e.g. a raw API/CLI request that skips
			// the wizard's own basename-collision disambiguation) can still
			// reach here with two repos deriving the SAME default dest, and
			// two Azure DevOps names can derive one directory ("Card Auth" and
			// "Card-Auth"). A silently dropped clone is easy to miss until the
			// agent goes looking for a repo that was never there, so the 201
			// says it as well as the log.
			slog.Warn("wardynd: repo clone target collides with another repo in this run; dropping the later one",
				slog.String("slug", slug), slog.String("dest", dest))
			if first == slug {
				// The same repository named twice — a workspace launch names its
				// repo as the run's own repo too — is one clone, not a drop.
				return
			}
			warnings = append(warnings, fmt.Sprintf(
				"repository %s was NOT cloned: %s is already where %s is cloned — give one of them its own target and launch again",
				slug, dest, first))
			return
		}
		seenDest[dest] = slug
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(url)
		b.WriteByte('\t')
		b.WriteString(dest)
		b.WriteByte('\t')
		b.WriteString(slug)
		b.WriteByte('\t')
		b.WriteString(ref)
	}
	add(legacyRepo, "", "") // legacy single repo → default dest, no ref
	for _, wr := range repos {
		add(wr.Repo, wr.Target, wr.Ref)
	}
	return b.String(), warnings
}

// injectionRuleFromScope decodes an api_key grant scope into its proxy-side
// injection rule. Mirrors the broker's apiKeyScope shape (host, header,
// format, secret_name, require_tls) with the same defaults. require_tls (absent
// == false) is BOUND here: the rule rides runner.InjectionGrant into the proxy's
// config (probeInjections, mintRecordAPIKeyInjections) for the plain lane.
//
// Strict because of require_tls: a lenient Unmarshal would decode a typo like
// `{"require-tls":true}` to false, a security control failing OPEN with every
// gate green. DisallowUnknownFields makes it a 422 at the write boundary
// (validateEligibleGrant). Every scope Wardyn authors (llmcred.go, runs_create.go,
// integrations_run.go, artifact_redirect.go, runs_dispatch_llm.go) carries only
// these keys, so nothing shipped is newly refused.
func injectionRuleFromScope(scope json.RawMessage) (egress.InjectionRule, error) {
	var sc struct {
		Host       string `json:"host"`
		Header     string `json:"header"`
		Format     string `json:"format"`
		SecretName string `json:"secret_name"`
		RequireTLS bool   `json:"require_tls"`
		// Snapshot is DECLARED here only so the strict decode below does not
		// refuse the two scopes that carry it: the immutable dispatch-time
		// credential scope of the captured-AWS-SSO grant
		// (authorBedrockSSOInjection) and of the Bedrock bearer grant
		// (authorBedrockBearerInjection). It is read from the GRANT by
		// resolveAWSSSOInjection / resolveBedrockBearerInjection, never from the
		// rule — an injection rule is a host/header/format binding and has no
		// business carrying identity.
		Snapshot json.RawMessage `json:"snapshot"`
		// PinPath/PinQuery narrow WHICH requests to Host may carry the
		// credential. Unlike Snapshot they ARE the rule's business — they
		// describe the request, not the identity — so they are carried through
		// to the sidecar. Absent on every scope but the captured-AWS-SSO one.
		PinPath  string            `json:"pin_path"`
		PinQuery map[string]string `json:"pin_query"`
	}
	dec := json.NewDecoder(bytes.NewReader(scope))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil {
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
	return egress.InjectionRule{
		Host: sc.Host, Header: sc.Header, SecretName: sc.SecretName, Format: sc.Format,
		RequireTLS: sc.RequireTLS, PinPath: sc.PinPath, PinQuery: sc.PinQuery,
	}, nil
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

// envSecretScopeFields decodes an env_secret grant scope {name, secret_name}.
// Both are REQUIRED (fail closed). name is the sandbox env var the stored
// secret_name's value lands under at dispatch (resolveEnvSecretGrants).
//
// The name is VALIDATED here, not merely decoded, because it is written into a
// process environment: POSIX-portable [A-Z_][A-Z0-9_]* only. Lower case is
// refused too — not for portability but for reviewability, so an env_secret
// grant in a policy diff cannot be mistaken for anything but an env var — and a
// WARDYN_ prefix is refused outright: those names configure the agent's own
// harness (WARDYN_TASK_MODE, WARDYN_GIT_PAT_GRANTS, …), so authoring one would
// turn a credential delivery into a dispatch-config override.
func envSecretScopeFields(scope json.RawMessage) (name, secretName string, err error) {
	var sc struct {
		Name       string `json:"name"`
		SecretName string `json:"secret_name"`
	}
	if err = json.Unmarshal(scope, &sc); err != nil {
		return "", "", err
	}
	if sc.Name == "" || sc.SecretName == "" {
		return "", "", errors.New("env_secret scope requires name and secret_name")
	}
	if !validEnvVarName(sc.Name) {
		return "", "", fmt.Errorf("env_secret name %q must match [A-Z_][A-Z0-9_]*", sc.Name)
	}
	if strings.HasPrefix(sc.Name, "WARDYN_") {
		return "", "", fmt.Errorf("env_secret name %q is reserved: WARDYN_* configures the sandbox harness itself", sc.Name)
	}
	return sc.Name, sc.SecretName, nil
}

// validEnvVarName reports whether s is a POSIX-portable, upper-case environment
// variable name. Hand-rolled rather than regexp: one pass, no package-level
// MustCompile, and the whole rule is three character classes wide.
func validEnvVarName(s string) bool {
	for i, c := range s {
		switch {
		case c >= 'A' && c <= 'Z', c == '_':
		case i > 0 && c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return s != ""
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
