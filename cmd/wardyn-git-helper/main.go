// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-git-helper is the git credential helper for Wardyn-governed
// agent sandboxes. It implements the git-credential protocol (get/store/erase)
// as a thin broker client: on "get" it resolves a stored Personal Access Token
// matched by host (Azure DevOps / GitLab / a plain GitHub PAT via
// WARDYN_GIT_PAT_GRANTS, username resolved by the broker), calls the proxy's
// local mint route, then prints the credentials to stdout for git to consume.
// The credential never touches disk or argv — stdout only, consumed ephemerally.
//
// It does NOT serve the GitHub App lane for a BROKERED run — one whose repos the
// git broker serves (WARDYN_GIT_BROKER_REPOS non-empty). There, agent-run points
// each granted repo at the proxy's /wardyn/gh/ route, which mints the repo-scoped
// installation token SERVER-SIDE, applies the per-repo allowlist, and parses every
// git-receive-pack ref against the run's branch namespace. A GitHub host is
// therefore REFUSED here (see resolveGrantForHost) so the token never enters the
// sandbox at all — it used to be printed to stdout, and a push carrying it
// straight to github.com:443 is an opaque tunnel no pkt-line parser can inspect.
// A run with a github_token grant but NO brokered repos has no broker route and
// no injected deny, so it still mints here, unchanged; so does a GitHub host with
// no App grant at all, falling through to a git_pat grant.
//
// The helper is configured GLOBALLY (git config --system credential.helper), so
// for any host it does NOT broker (no matching grant) it emits NOTHING and exits
// 0, letting git fall through to its normal behavior. It must never break
// unrelated git auth.
//
// Invariants:
//   - The brokered credential is never persisted to disk, env, or args — it is
//     written to stdout only, for git to consume ephemerally (security
//     invariant 1). It is never logged.
//   - Requests to the proxy use a direct transport (Proxy: nil) — the proxy URL
//     is a known on-segment address, not subject to HTTP_PROXY env.
//   - If no grant is configured for the host (a github host absent from
//     WARDYN_GIT_PAT_GRANTS, or any other host absent from it) the helper exits 0
//     with no output so git falls through to its normal prompting. This is
//     intentional: runs without grants must not be blocked. A github host on a
//     BROKERED run (WARDYN_GIT_BROKER_REPOS non-empty) takes the same silent-exit
//     path, by refusal rather than by absence.
//
// Caller authentication (in-sandbox token-exfiltration hardening):
//
//	The sandbox shares one kernel and one uid (agent). Without a check, ANY
//	in-sandbox process that speaks git's credential protocol — a sub-process, a
//	snooping `wardyn attach` shell, a careless tool — could invoke this helper
//	and have a LIVE GitHub token streamed to it on stdout. To raise the bar, the
//	helper requires the caller to PRESENT a per-run secret before it emits a
//	token:
//
//	  - The canonical per-run secret lives in a FILE that is owned by the agent
//	    uid and mode 0400 (provisioned by agent-run; see deploy/images/*/agent-run).
//	    Its path is passed to the helper via the git credential.helper config
//	    (`--secret-file <path>`). The helper reads it as the EXPECTED value.
//	  - The caller PRESENTS the secret via the WARDYN_GIT_HELPER_SECRET
//	    environment variable. agent-run exports it for its descendants (the agent
//	    process and its git invocations), so it is process-scoped — it is NOT in
//	    the container-wide sandbox env, so a separate attach exec or any
//	    non-descendant process does not inherit it.
//	  - Before minting, the helper constant-time-compares the presented value
//	    against the expected file content. A mismatch (or no presented value)
//	    fails CLOSED: no token is emitted, but git is NOT errored (we return
//	    success with empty output, exactly like the no-grant case) so the helper
//	    never breaks git itself.
//	  - If no secret file is provisioned for the run (the file is absent/empty —
//	    e.g. an interactive run that never goes through agent-run, or a
//	    deployment that has not wired the gate), the helper preserves the legacy
//	    behaviour and mints, so legitimate git is never blocked.
//
//	RESIDUAL (documented honestly): this secret binds a caller going through
//	THIS BINARY — it stops a caller that speaks git's credential protocol
//	(a sub-process, a snooping attach shell) by routing through
//	wardyn-git-helper itself. It does NOT bind the credential at its source:
//	the proxy's local mint route (POST /wardyn/v1/credentials/mint) is itself
//	unauthenticated — like every other /wardyn/... local route, it trusts
//	anything that can reach it as "the sandbox" — so a caller willing to skip
//	this binary, read the grant id straight out of the container-wide
//	WARDYN_GIT_PAT_GRANTS env (unlike WARDYN_GIT_HELPER_SECRET, that one is
//	NOT process-scoped), and POST the route directly is not bound at all.
//	Within the "goes through this binary" set, the secret further raises the
//	bar from "any in-sandbox process" to "code executing AS the agent uid": a
//	process running as the agent user can still read the 0400 secret file (or
//	read WARDYN_GIT_HELPER_SECRET from a descendant's /proc/<pid>/environ, or
//	simply be a descendant of agent-run) and thereby obtain the token via the
//	binary too. Closing either gap requires authenticating the CALLER at the
//	proxy's mint route itself (e.g. SPIFFE-attested mint, or a per-invocation
//	nonce), which is future work. Interactive runs ARE gated: their idle
//	main process is agent-run too, and its --idle path calls
//	provision_git_helper_secret exactly as task mode does (see
//	deploy/images/common/agent-run-lib.sh). What differs is inheritance —
//	`wardyn attach` is a FRESH exec, not a descendant of that process tree,
//	so it never inherits WARDYN_GIT_HELPER_SECRET and this helper refuses it
//	until the human presents the secret themselves. That refusal is a real
//	gate, not a wall: the 0400 file is agent-readable, which is the same
//	residual described above.
//
//	The secret is never logged.
//
// Usage (set by git credential helper config):
//
//	git config credential.helper \
//	  '/usr/local/bin/wardyn-git-helper --secret-file /home/agent/.wardyn/git-helper.secret'
//
// git invokes: wardyn-git-helper [--secret-file <path>] get|store|erase
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
)

const (
	defaultProxyURL        = "http://wardyn-proxy:3128"
	defaultApprovalTimeout = 120 * time.Second
	pollInterval           = 3 * time.Second

	// envHelperSecret is the env var through which a caller PRESENTS the per-run
	// helper secret. agent-run exports it for its descendants only; it is NOT in
	// the container-wide sandbox env (so an attach exec / non-descendant process
	// does not inherit it). See the package doc, "Caller authentication".
	envHelperSecret = "WARDYN_GIT_HELPER_SECRET"

	// envGitPATGrants is a JSON object {host: grant_id} of git_pat grants for the
	// run: the helper mints a stored PAT (returned as username/password) for a
	// host present here. Absent/empty => no PAT grants (helper still handles
	// github.com via WARDYN_GITHUB_GRANT_ID).
	envGitPATGrants = "WARDYN_GIT_PAT_GRANTS"

	// envGitBrokerRepos is the space-separated set of "<org>/<repo>" this run's
	// GitHub access is BROKERED for (dispatch writes it from the same map
	// api.confineGitBrokerEgress keys on). Non-empty is the ONE signal that the
	// /wardyn/gh/ route exists and the broker-managed GitHub hosts are denied —
	// see resolveGrantForHost.
	envGitBrokerRepos = "WARDYN_GIT_BROKER_REPOS"
)

func main() {
	secretFile, op, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "wardyn-git-helper:", err)
		os.Exit(1)
	}
	if op == "" {
		fmt.Fprintln(os.Stderr, "wardyn-git-helper: usage: wardyn-git-helper [--secret-file <path>] <get|store|erase>")
		os.Exit(1)
	}
	if err := run(op, secretFile, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "wardyn-git-helper:", err)
		os.Exit(1)
	}
}

// parseArgs splits the helper's argv (os.Args[1:]) into the optional
// --secret-file path and the git operation (get/store/erase). git invokes the
// helper as "<configured args> <operation>", so any flags precede the operation
// positional (e.g. "--secret-file /p get"). An empty secretFile means no
// caller-auth gate is configured (legacy / manual invocation).
func parseArgs(argv []string) (secretFile, op string, err error) {
	fset := flag.NewFlagSet("wardyn-git-helper", flag.ContinueOnError)
	fset.SetOutput(io.Discard) // format our own errors; never write to git's stdout
	sf := fset.String("secret-file", "", "path to the per-run caller-auth secret file (0400, agent-owned)")
	if perr := fset.Parse(argv); perr != nil {
		return "", "", perr
	}
	if rest := fset.Args(); len(rest) > 0 {
		op = rest[0]
	}
	return *sf, op, nil
}

// run implements the git-credential protocol for the given subcommand.
// secretFile is the configured caller-auth secret path ("" = no gate).
// stdin/stdout/stderr are parameterised for testing.
func run(cmd, secretFile string, stdin io.Reader, stdout, stderr io.Writer) error {
	switch cmd {
	case "store", "erase":
		// Tokens are never persisted — consume and ignore the input.
		_, _ = io.Copy(io.Discard, stdin)
		return nil
	case "get":
		return runGet(secretFile, stdin, stdout, stderr)
	default:
		// Unknown sub-commands are silently ignored; git may add new ones.
		_, _ = io.Copy(io.Discard, stdin)
		return nil
	}
}

// credInput holds the key=value pairs git passes on stdin.
type credInput struct {
	Protocol string
	Host     string
	// Other keys (username, path, …) are parsed but not used.
}

// parseInput reads git's key=value credential lines from r. git terminates the
// block with a blank line or EOF.
func parseInput(r io.Reader) credInput {
	var ci credInput
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(k) {
		case "protocol":
			ci.Protocol = v
		case "host":
			// The host may include port (e.g. "github.com:443"); strip it.
			ci.Host, _, _ = strings.Cut(v, ":")
		}
	}
	return ci
}

// isGitHubHost returns true for github.com and *.github.com.
func isGitHubHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h == "github.com" || strings.HasSuffix(h, ".github.com")
}

// runGet handles the "get" subcommand: parse stdin, resolve the grant for the
// host (GitHub installation token OR a matched git_pat), authenticate the
// caller, mint via proxy, poll if approval pending, print username/password to
// stdout. secretFile is the configured caller-auth secret path ("" = no gate).
//
// An unmatched host emits NOTHING and returns nil so git falls through to its
// normal behavior — the helper is configured GLOBALLY (credential.helper), so it
// MUST never break unrelated git auth.
func runGet(secretFile string, stdin io.Reader, stdout, stderr io.Writer) error {
	ci := parseInput(stdin)

	grantID, fallbackUser := resolveGrantForHost(ci.Host, stderr)
	if grantID == "" {
		// Not a host we broker for (and, for github.com, no grant configured):
		// output nothing, let git fall through. PRESERVES the legacy fall-through.
		return nil
	}

	proxyURL := os.Getenv("WARDYN_PROXY_URL")
	if proxyURL == "" {
		proxyURL = defaultProxyURL
	}

	// Caller authentication: before minting a LIVE credential, require the caller
	// to present the per-run secret (see the package doc, "Caller authentication").
	// This gates emission so a stray in-sandbox process / attach exec that merely
	// speaks git's credential protocol cannot exfiltrate a token/PAT. Failing this
	// check is FAIL CLOSED — emit NOTHING — but we still return nil so git is not
	// errored (it falls through exactly as for the no-grant case).
	if ok, reason := authenticateCaller(secretFile); !ok {
		// Never log the secret or the token.
		fmt.Fprintf(stderr, "wardyn-git-helper: %s; refusing to emit a brokered token\n", reason)
		return nil
	}

	// EnvDuration, not a bare ParseDuration: a typo'd interval (e.g. "30" with
	// no unit) must fail loud at exit 2, not silently keep the default.
	approvalTimeout := cliutil.EnvDuration("WARDYN_APPROVAL_TIMEOUT", defaultApprovalTimeout)

	// Direct transport: ignore HTTP_PROXY env — the proxy address is a known
	// on-segment address, never accessed through another proxy.
	transport := &http.Transport{
		Proxy: nil,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), approvalTimeout+10*time.Second)
	defer cancel()

	token, username, err := mintWithApproval(ctx, client, proxyURL, grantID, approvalTimeout, stderr)
	if err != nil {
		return err
	}
	// The github path returns no username in the mint response; fall back to the
	// host-derived default (x-access-token). git_pat returns its resolved
	// username (ADO=pat, GitLab=oauth2, or an explicit override).
	if username == "" {
		username = fallbackUser
	}

	// Emit git credential protocol output: echo protocol/host then the credential.
	if ci.Protocol != "" {
		fmt.Fprintf(stdout, "protocol=%s\n", ci.Protocol)
	}
	if ci.Host != "" {
		fmt.Fprintf(stdout, "host=%s\n", ci.Host)
	}
	fmt.Fprintf(stdout, "username=%s\n", username)
	fmt.Fprintf(stdout, "password=%s\n", token)
	return nil
}

// resolveGrantForHost picks the credential grant id + fallback git username for
// the requested host:
//   - a GitHub host (github.com / *.github.com) on a BROKERED run
//     (WARDYN_GIT_BROKER_REPOS non-empty) is REFUSED — see below;
//   - otherwise a GitHub host routes to WARDYN_GITHUB_GRANT_ID with the
//     installation-token username x-access-token, unchanged;
//   - with no App grant configured, a GitHub host falls through to a plain
//     git_pat grant for it in WARDYN_GIT_PAT_GRANTS (a classic or fine-grained
//     GitHub PAT), unchanged;
//   - a non-GitHub host present in WARDYN_GIT_PAT_GRANTS (JSON {host:
//     grant_id}) routes to that PAT grant; its username is resolved by the
//     broker and comes back in the mint response, so the fallback here is "".
//
// WHY A BROKERED RUN REFUSES. That lane is BROKERED, not helper-served: agent-run
// rewrites every granted repo's remote to the proxy's /wardyn/gh/<org>/<repo>
// route (url.<broker>.insteadOf), where the installation token is minted
// SERVER-SIDE and never enters the sandbox, the per-repo allowlist is applied,
// and a git-receive-pack's ref updates are parsed against the run's branch
// namespace. Minting the same token here handed it to whatever spoke git's
// credential protocol — on stdout, inside a one-uid sandbox — and a push carrying
// it straight to github.com:443 is an opaque CONNECT tunnel that walks around the
// pkt-line parser entirely. Dispatch also subtracts AND denies the broker-managed
// hosts for a brokered run (api.confineGitBrokerEgress), so this path has no route
// left either; refusing keeps the token itself out of the sandbox.
//
// WHY THE PREDICATE IS WARDYN_GIT_BROKER_REPOS, NOT WARDYN_GITHUB_GRANT_ID. Those
// two are NOT the same set. Confinement (and the /wardyn/gh/ route) fire on the
// broker map being non-empty — the run's github_token grant scope.repos plus its
// declared clone set. A github_token grant that declares NO repos anywhere (what
// every shipped example policy produces before `--repo` is added) has no broker
// route and no deny, so refusing on the grant alone left such a run with no
// credential path at all AND github.com still reachable — worse than either
// design. Keying on the broker set makes the refusal predicate exactly the
// confinement predicate: refuse where the broker actually serves, mint where the
// helper is still the only path.
//
// An unmatched host returns ("", "") so runGet emits nothing and git falls
// through — never breaking unrelated git auth. Same for the two refusals here:
// they log to stderr and emit nothing.
func resolveGrantForHost(host string, stderr io.Writer) (grantID, fallbackUser string) {
	if isGitHubHost(host) {
		if strings.TrimSpace(os.Getenv(envGitBrokerRepos)) != "" {
			fmt.Fprintln(stderr, "wardyn-git-helper: this run's GitHub access is brokered through wardyn-proxy (/wardyn/gh/), where the token is minted server-side; refusing to mint an installation token into the sandbox")
			return "", ""
		}
		if gid := os.Getenv("WARDYN_GITHUB_GRANT_ID"); gid != "" {
			return gid, "x-access-token"
		}
		if gid := patGrantForHost(host); gid != "" {
			return gid, ""
		}
		fmt.Fprintln(stderr, "wardyn-git-helper: WARDYN_GITHUB_GRANT_ID not set and no git_pat grant for this host; skipping broker mint (git will prompt)")
		return "", ""
	}
	if gid := patGrantForHost(host); gid != "" {
		return gid, ""
	}
	return "", ""
}

// patGrantForHost parses WARDYN_GIT_PAT_GRANTS (JSON {host: grant_id}) and
// returns the grant id for host, or "" if the env is unset/absent/unparseable
// or the host has no entry. Host match is exact and case-insensitive (a trailing
// dot on either side is tolerated). Unparseable JSON fails closed to "".
func patGrantForHost(host string) string {
	raw := os.Getenv(envGitPATGrants)
	if raw == "" {
		return ""
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	for k, v := range m {
		if strings.ToLower(strings.TrimSuffix(k, ".")) == h {
			return v
		}
	}
	return ""
}

// authenticateCaller decides whether the current invocation is allowed to mint a
// brokered token. It implements the caller-auth gate described in the package
// doc:
//
//   - If no secret file is provisioned for this run (path empty, file absent, or
//     empty file), there is no gate configured: return ok=true (legacy mint).
//     This keeps legitimate git working for runs that never provisioned the gate
//     (interactive runs, deployments not wired for it) — never blocking git.
//   - If a secret file IS provisioned, the caller MUST present a matching secret
//     via the WARDYN_GIT_HELPER_SECRET env var. Comparison is constant-time. A
//     missing/wrong presented secret, or an unreadable-but-present file, returns
//     ok=false so the caller fails CLOSED (no token emitted).
//
// The secret value is never logged; reason strings carry no secret material.
func authenticateCaller(secretFile string) (ok bool, reason string) {
	expected, provisioned, rerr := loadExpectedSecret(secretFile)
	if !provisioned {
		// No per-run caller-auth secret configured for this run: legacy allow.
		return true, ""
	}
	if rerr != nil {
		// The secret file exists but could not be read (e.g. the caller runs as a
		// uid other than the agent owner of a 0400 file). Auth is provisioned but
		// unverifiable here: fail closed.
		return false, "caller-auth secret file present but unreadable"
	}
	presented := os.Getenv(envHelperSecret)
	if presented == "" {
		return false, "caller did not present " + envHelperSecret
	}
	if subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) != 1 {
		return false, "caller presented an invalid " + envHelperSecret
	}
	return true, ""
}

// loadExpectedSecret reads the per-run caller-auth secret from path. It reports
// provisioned=false (the no-gate case) when path is empty, the file does not
// exist, or the file is empty — these all mean "no caller-auth secret is
// configured for this run". A present-but-unreadable file returns
// provisioned=true with a non-nil err so the caller can fail closed.
func loadExpectedSecret(path string) (secret string, provisioned bool, err error) {
	if path == "" {
		return "", false, nil
	}
	b, e := os.ReadFile(path)
	if e != nil {
		if errors.Is(e, fs.ErrNotExist) {
			return "", false, nil // not provisioned -> legacy allow
		}
		return "", true, e // present but unreadable -> provisioned, fail closed
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", false, nil // empty file -> treat as not provisioned
	}
	return s, true, nil
}

// mintResponse is the broker's 200 success shape (from internal/api/internal.go).
// Username is populated for git_pat (ADO=pat, GitLab=oauth2, or an override) and
// empty for github_token (the helper falls back to x-access-token).
type mintResponse struct {
	Kind      string `json:"kind"`
	Token     string `json:"token"`
	Username  string `json:"username"`
	JTI       string `json:"jti"`
	ExpiresAt string `json:"expires_at"`
}

// mint 409-conflict "code" values — mirrors internal/api/internal.go's
// mintConflict* constants (the ONE place both sides of this wire contract
// must agree, W19-W19a-2).
const (
	mintConflictPending       = "pending"
	mintConflictDenied        = "denied"
	mintConflictScopeMismatch = "scope_mismatch"
	mintConflictAlreadyMinted = "already_minted"
)

// pendingResponse is the broker's 409 shape — covers all four conflict
// conditions (Code discriminates which; ApprovalID/Denied/Reason are
// populated only for the two approval-flow codes).
type pendingResponse struct {
	Code       string `json:"code"`
	ApprovalID string `json:"approval_id"`
	Denied     bool   `json:"denied"`
	Reason     string `json:"reason"`
	Error      string `json:"error"`
}

// approvalResponse is the poll shape for GET /wardyn/v1/approvals/{id}.
type approvalResponse struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// mintWithApproval calls POST {proxy}/wardyn/v1/credentials/mint and handles
// the 409 approval-pending loop. It never persists the returned credential.
// Returns (token, username, err); username is the git_pat username (empty for
// github_token, whose caller falls back to x-access-token).
func mintWithApproval(ctx context.Context, client *http.Client, proxyURL, grantID string, approvalTimeout time.Duration, stderr io.Writer) (string, string, error) {
	token, username, approvalID, err := callMint(ctx, client, proxyURL, grantID)
	if err != nil {
		return "", "", err
	}
	if token != "" {
		return token, username, nil
	}

	// 409 with an approval_id: poll until decided or timeout.
	if approvalID == "" {
		return "", "", fmt.Errorf("mint returned 409 without approval_id")
	}

	fmt.Fprintf(stderr, "wardyn-git-helper: credential approval pending (%s); waiting up to %s for a human to approve in the Wardyn UI\n",
		approvalID, approvalTimeout)

	deadline := time.Now().Add(approvalTimeout)
	for {
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("timed out waiting for approval %s — a human approval is still pending in the Wardyn UI", approvalID)
		}

		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(pollInterval):
		}

		state, err := pollApproval(ctx, client, proxyURL, approvalID)
		if err != nil {
			// Transient poll error: log and keep waiting.
			fmt.Fprintf(stderr, "wardyn-git-helper: poll approval %s: %v (retrying)\n", approvalID, err)
			continue
		}

		switch strings.ToUpper(state) {
		case "APPROVED":
			// Re-mint now that approval is granted.
			token, username, _, err := callMint(ctx, client, proxyURL, grantID)
			if err != nil {
				return "", "", fmt.Errorf("re-mint after approval: %w", err)
			}
			if token == "" {
				return "", "", fmt.Errorf("re-mint after approval returned no token")
			}
			return token, username, nil
		case "DENIED":
			return "", "", fmt.Errorf("credential approval %s was denied by the operator", approvalID)
		case "EXPIRED":
			return "", "", fmt.Errorf("credential approval %s expired before a decision was made", approvalID)
		default:
			// Still PENDING: keep polling.
		}
	}
}

// callMint POSTs to the proxy's local mint route. Returns (token, username, "",
// nil) on success, ("", "", approvalID, nil) on 409-pending, or ("", "", "",
// err) on all other error conditions.
func callMint(ctx context.Context, client *http.Client, proxyURL, grantID string) (token, username, approvalID string, err error) {
	mintURL, err := buildLocalURL(proxyURL, "/wardyn/v1/credentials/mint")
	if err != nil {
		return "", "", "", fmt.Errorf("build mint URL: %w", err)
	}

	body, err := json.Marshal(map[string]string{"grant_id": grantID})
	if err != nil {
		return "", "", "", fmt.Errorf("marshal mint request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mintURL, bytes.NewReader(body))
	if err != nil {
		return "", "", "", fmt.Errorf("build mint request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("mint request: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", "", "", fmt.Errorf("read mint response: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		var mr mintResponse
		if err := json.Unmarshal(respBody, &mr); err != nil {
			return "", "", "", fmt.Errorf("decode mint response: %w", err)
		}
		if mr.Token == "" {
			return "", "", "", fmt.Errorf("mint response missing token")
		}
		return mr.Token, mr.Username, "", nil

	case http.StatusConflict:
		var pr pendingResponse
		if err := json.Unmarshal(respBody, &pr); err != nil {
			return "", "", "", fmt.Errorf("decode 409 response: %w", err)
		}
		// W19-W19a-2: the four 409 conditions share the status but carry a
		// "code" discriminator — name the real cause instead of falling
		// through to the generic missing-approval_id guess (mintWithApproval)
		// for a condition that was never approval-pending at all.
		switch pr.Code {
		case mintConflictDenied:
			reason := pr.Reason
			if reason == "" {
				reason = "no reason given"
			}
			return "", "", "", fmt.Errorf("credential grant denied: %s", reason)
		case mintConflictAlreadyMinted:
			return "", "", "", fmt.Errorf("credential already minted (single-use) — a prior mint for this grant already succeeded. " +
				"For a git_pat this is the second-git-op case (docs/adoption/corp-network-onboarding-findings.md B2): approve with " +
				"decision_scope=run (`wardyn approve <id> --scope run`) to take a per-run lease instead of one mint per operation")
		case mintConflictScopeMismatch:
			return "", "", "", fmt.Errorf("requested scope does not match the grant (no-widening)")
		case mintConflictPending:
			return "", "", pr.ApprovalID, nil
		default:
			// Pre-code broker (or the legacy shape before W19-W19a-2): fall
			// back to the field-presence heuristic exactly as before.
			if pr.Denied {
				reason := pr.Reason
				if reason == "" {
					reason = "no reason given"
				}
				return "", "", "", fmt.Errorf("credential grant denied: %s", reason)
			}
			return "", "", pr.ApprovalID, nil
		}

	case http.StatusUnprocessableEntity:
		return "", "", "", fmt.Errorf("credential grant requires SPIRE identity provider (not available in this deployment)")

	case http.StatusUnauthorized:
		return "", "", "", fmt.Errorf("proxy authentication failed (401): %s", strings.TrimSpace(string(respBody)))

	case http.StatusNotFound:
		return "", "", "", fmt.Errorf("grant not found (404): check that the grant id is correct")

	default:
		return "", "", "", fmt.Errorf("unexpected mint status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
}

// pollApproval calls GET {proxy}/wardyn/v1/approvals/{id} and returns the
// approval state string (PENDING / APPROVED / DENIED / EXPIRED).
func pollApproval(ctx context.Context, client *http.Client, proxyURL, approvalID string) (string, error) {
	pollURL, err := buildLocalURL(proxyURL, "/wardyn/v1/approvals/"+approvalID)
	if err != nil {
		return "", fmt.Errorf("build poll URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return "", fmt.Errorf("build poll request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("poll request: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("poll status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var ar approvalResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return "", fmt.Errorf("decode approval: %w", err)
	}
	return ar.State, nil
}

// buildLocalURL constructs the URL for a proxy-local route. The path must
// begin with /wardyn/ so it routes to a local handler rather than being
// forwarded as an absolute-URI proxy request.
//
// The proxy's local routes are served ONLY for origin-form (path-only) requests
// addressed to the proxy host itself. We achieve this by issuing a plain HTTP
// request to the proxy host+port with the local path — the proxy sees a request
// with URL.Host="" (origin form) and handles it locally instead of forwarding.
//
// Concretely: if proxyURL is "http://wardyn-proxy:3128" and path is
// "/wardyn/v1/credentials/mint", the resulting URL is
// "http://wardyn-proxy:3128/wardyn/v1/credentials/mint" — a plain HTTP GET/POST
// to the proxy host at the given path. The proxy's ServeHTTP receives it with
// r.URL.Host == "" (no absolute URI), matching the local-route condition.
func buildLocalURL(proxyURL, path string) (string, error) {
	return url.JoinPath(proxyURL, path)
}
