// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package broker implements Wardyn's token broker: the ONLY component that
// holds long-lived secrets and the sole issuer of short-lived run credentials.
//
// SECURITY INVARIANTS (see ARCHITECTURE.md, non-negotiable):
//
//   - Approval mints the credential. For a grant whose spec sets
//     RequiresApproval, the mint happens ONLY inside the same Postgres
//     transaction that verifies approvals.state='APPROVED' for this run, and
//     writes approvals.minted_jti back in that same transaction. Single-use:
//     a non-empty minted_jti blocks any second mint.
//   - No widening. The scope minted is EXACTLY the scope the approver saw:
//     approvals.requested_scope must deep-equal the grant spec scope, else
//     ErrScopeMismatch. github_token scopes are additionally clamped to a
//     ceiling of contents:write + pull_requests:write.
//   - Secrets never enter the sandbox. api_key grants resolve to a proxy-side
//     egress.InjectionRule (returned by reference, never the secret value).
//   - Fail closed. cloud_sts hard-requires SPIRE and is refused here.
//   - Full attribution. credential.mint audit events carry actor_type=agent
//     and actor=run SPIFFE ID.
package broker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// defaultMaxTTL caps minted credential lifetime (GrantSpec.TTLSeconds may
// narrow but never widen). Mirrors types.GrantSpec documentation: max 1h.
const defaultMaxTTL = time.Hour

// branchNamespaceFormat is the push-branch confinement convention recorded in
// minted github_token metadata.
//
// IMPORTANT (honesty): this is ADVISORY METADATA ONLY and is NOT enforced today.
// A GitHub installation token cannot self-restrict to a ref prefix, so the
// minted token can push to ANY branch (including the default) within its granted
// repos. Real enforcement is [v0.5 — planned] and requires a push-ref-inspecting
// git-proxy (TLS-intercept tier) or GitHub-side branch-protection rulesets; the
// broker records the namespace now so that future layer has an authoritative
// value to clamp against. Do not represent branch confinement as enforced until
// that layer ships. See threatmodel/THREAT-MODEL.md asset #4.
const branchNamespaceFormat = "wardyn/%s/*"

// Sentinel and typed errors. All map to fail-closed REST responses.
var (
	// ErrScopeMismatch fires when approvals.requested_scope does not deep-equal
	// the grant spec scope (the no-widening invariant).
	ErrScopeMismatch = errors.New("broker: requested scope does not match grant spec scope (no-widening invariant)")
	// ErrRequiresSPIRE fires for cloud_sts grants; the embedded path refuses them.
	ErrRequiresSPIRE = errors.New("broker: cloud_sts grants hard-require the spire identity provider")
	// ErrAlreadyMinted fires on a second mint attempt for a single-use approval.
	ErrAlreadyMinted = errors.New("broker: credential already minted for this approval (single-use)")
	// ErrNotApproved fires when the joined approval is not in state APPROVED.
	ErrNotApproved = errors.New("broker: approval is not in state APPROVED")
	// ErrGrantNotFound fires when the grant id resolves to no row.
	ErrGrantNotFound = errors.New("broker: grant not found")
	// ErrRunMismatch fires when the caller's run does not own the grant.
	ErrRunMismatch = errors.New("broker: caller run does not own this grant")
	// ErrRunRevoked fires when the run has been revoked (kill-switch) before the
	// mint: the mint tx checks identity_revocations and fails closed.
	ErrRunRevoked = errors.New("broker: run is revoked; refusing to mint (kill-switch)")
	// ErrUnknownGrantKind fires for an unrecognized grant kind.
	ErrUnknownGrantKind = errors.New("broker: unknown grant kind")
)

// ErrApprovalPending signals that a human approval gate is still open. The REST
// layer renders this as 409 with the approval id so the caller can poll.
type ErrApprovalPending struct {
	ApprovalID uuid.UUID
}

func (e ErrApprovalPending) Error() string {
	return fmt.Sprintf("broker: approval %s pending", e.ApprovalID)
}

// ErrApprovalDenied signals the human denied the approval. Fail closed.
type ErrApprovalDenied struct {
	ApprovalID uuid.UUID
	Reason     string
}

func (e ErrApprovalDenied) Error() string {
	return fmt.Sprintf("broker: approval %s denied: %s", e.ApprovalID, e.Reason)
}

// Minted is the result of a successful mint. For github_token, Token carries
// the installation token. For api_key, the secret value NEVER appears: only an
// egress.InjectionRule (resolved proxy-side at use time) is returned.
type Minted struct {
	Kind       types.GrantKind `json:"kind"`
	JTI        string          `json:"jti"`
	ExpiresAt  time.Time       `json:"expires_at"`
	GrantID    uuid.UUID       `json:"grant_id"`
	ApprovalID uuid.UUID       `json:"approval_id,omitempty"`
	// Token is the bearer credential for github_token, the stored PAT VALUE for
	// git_pat, or the stored PRIVATE KEY material for ssh_key. Empty for api_key
	// (whose value never leaves the broker).
	Token string `json:"token,omitempty"`
	// Username is the git username to pair with Token for git_pat (ADO=pat,
	// GitLab=oauth2, or an explicit override) or ssh_key (default "git"). Empty for
	// github_token (the helper uses x-access-token) and api_key.
	Username string `json:"username,omitempty"`
	// KnownHosts is the OpenSSH known_hosts material for an ssh_key grant whose
	// scope named a known_hosts_secret_ref. Empty otherwise (ssh_key runs fall back
	// to the image-baked /etc/ssh/ssh_known_hosts for github.com / ADO). It is
	// public host-key data, not a secret, so it is NOT mask-registered.
	KnownHosts string `json:"known_hosts,omitempty"`
	// Injection is the proxy-side rule for api_key. Nil for github_token/git_pat.
	Injection *egress.InjectionRule `json:"injection,omitempty"`
	// Metadata carries kind-specific, non-secret context (e.g. github_token
	// branch namespace, repos, clamped permissions).
	Metadata map[string]string `json:"metadata,omitempty"`
}

// GitHubMinter mints a short-lived, down-scoped GitHub App installation token.
// The real implementation (githubMinter) holds the App private key; the fake
// (FakeGitHubMinter) is used in unit tests. Down-scoping is via repositories +
// permissions on the installation-token request.
type GitHubMinter interface {
	MintInstallationToken(ctx context.Context, repos []string, permissions map[string]string, ttl time.Duration) (token string, expiresAt time.Time, err error)
}

// Querier is the minimal transaction surface the broker needs. It is satisfied
// by pgx.Tx; tests fake it so the mint logic runs with no Postgres. Method
// shapes match pgx so the production Begin/Commit/Rollback dance is unchanged.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) Row
	// Exec runs a statement and returns the number of rows affected. Rows
	// affected is load-bearing for the single-use guard: a 0-row conditional
	// `UPDATE approvals SET minted_jti ... WHERE minted_jti=''` means another
	// mint won the race, so the caller fails closed. pgx's CommandTag is the
	// source; the fake mirrors its rows-affected semantics.
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
}

// Row is the single-row result surface (subset of pgx.Row).
type Row interface {
	Scan(dest ...any) error
}

// TxBeginner opens a transaction exposing a Querier. *pgxAdapter wraps a real
// *pgxpool.Pool; tests provide a fake. Commit/Rollback bound the mint tx.
type TxBeginner interface {
	Begin(ctx context.Context) (Tx, error)
}

// Tx is a Querier with commit/rollback. Rollback after Commit is a no-op.
type Tx interface {
	Querier
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// Broker is the token broker. It is safe for concurrent use; all mutating
// state lives in Postgres and every mint is serialized by SELECT ... FOR UPDATE
// on the grant+approval rows.
type Broker struct {
	db       TxBeginner
	secrets  secretstore.Store
	audit    audit.Recorder
	identity identity.Provider
	github   GitHubMinter
	// maskReg, when non-nil, receives minted token bytes so they are masked
	// from PTY captures and asciicast uploads. A nil Registry is a safe no-op.
	maskReg *secretmask.Registry
}

// New constructs a Broker. github may be nil if no github_token grants will be
// minted; a nil minter on a github_token grant fails closed.
func New(db TxBeginner, secrets secretstore.Store, rec audit.Recorder, idp identity.Provider, gh GitHubMinter) *Broker {
	return &Broker{db: db, secrets: secrets, audit: rec, identity: idp, github: gh}
}

// WithMaskRegistry attaches a secret-mask Registry to the Broker. After a
// successful github_token mint the token bytes are registered under the
// caller's RunID so they are masked from PTY/asciicast output. A nil reg is
// accepted (no-op). Call before the Broker is used.
func (b *Broker) WithMaskRegistry(reg *secretmask.Registry) *Broker {
	b.maskReg = reg
	return b
}

// grantApprovalRow is the joined grant+approval state read under FOR UPDATE.
type grantApprovalRow struct {
	grantID        uuid.UUID
	grantRunID     uuid.UUID
	grantSpec      types.GrantSpec
	approvalID     uuid.UUID // uuid.Nil when no approval row (auto-mint path)
	approvalRunID  uuid.UUID
	approvalState  types.ApprovalState
	requestedScope json.RawMessage
	mintedJTI      string
	hasApproval    bool
}

// MintForGrant is the public entry point. It verifies the caller's run owns the
// grant, then routes to the approval-gated or auto-mint path. If the grant
// requires approval and no decided approval exists, it ensures a PENDING
// approval (creating one if absent) and returns ErrApprovalPending.
func (b *Broker) MintForGrant(ctx context.Context, caller *identity.Claims, grantID uuid.UUID) (Minted, error) {
	if caller == nil {
		return Minted{}, errors.New("broker: nil caller claims")
	}

	// Read the grant (and any approval) to decide routing. This pre-check is
	// outside the mint tx; the authoritative single-use + state checks happen
	// inside MintOnApproval's FOR UPDATE transaction.
	spec, grantRunID, err := b.loadGrant(ctx, grantID)
	if err != nil {
		return Minted{}, err
	}
	if grantRunID != caller.RunID {
		return Minted{}, ErrRunMismatch
	}

	if spec.Kind == types.GrantCloudSTS {
		b.auditMint(ctx, caller, grantID, uuid.Nil, "", spec.Scope, "denied")
		return Minted{}, ErrRequiresSPIRE
	}

	if !spec.RequiresApproval {
		// Auto-mintable: no approval row exists or is created, so there is no
		// single-use guard — the grant is re-mintable BY DESIGN (credential-
		// helper refresh semantics). Every mint is serialized by the grant-row
		// FOR UPDATE lock, capped at the <=1h TTL, and individually audited.
		return b.mint(ctx, caller, grantID, uuid.Nil)
	}

	// Approval-gated: find or create the approval, inspect its state.
	ap, err := b.ensureApproval(ctx, grantID, grantRunID, spec)
	if err != nil {
		return Minted{}, err
	}
	switch ap.State {
	case types.ApprovalPending:
		return Minted{}, ErrApprovalPending{ApprovalID: ap.ID}
	case types.ApprovalDenied, types.ApprovalExpired:
		return Minted{}, ErrApprovalDenied{ApprovalID: ap.ID, Reason: ap.Reason}
	case types.ApprovalApproved:
		return b.mint(ctx, caller, grantID, ap.ID)
	default:
		return Minted{}, fmt.Errorf("broker: unexpected approval state %q", ap.State)
	}
}

// MintOnApproval mints the credential for an already-APPROVED, approval-gated
// grant. It is the security chokepoint: a single FOR UPDATE transaction reads
// the grant joined to its approval, verifies APPROVED + matching run +
// single-use + no-widening, calls the kind-specific minter, and writes
// minted_jti back before committing. It is also reachable directly from the
// approval FSM (decide -> mint) by callers that already hold approved state.
func (b *Broker) MintOnApproval(ctx context.Context, runID, grantID uuid.UUID) (Minted, error) {
	caller := &identity.Claims{RunID: runID, SPIFFEID: spiffeForRun(runID)}
	return b.mint(ctx, caller, grantID, uuid.Nil)
}

// mint runs the authoritative single-transaction mint. approvalHint, when
// non-Nil, narrows the SELECT to that approval id (used after MintForGrant's
// ensureApproval); when Nil it resolves the approval (or auto-approval) by
// grant id. caller.SPIFFEID is the audit actor.
func (b *Broker) mint(ctx context.Context, caller *identity.Claims, grantID, approvalHint uuid.UUID) (Minted, error) {
	tx, err := b.db.Begin(ctx)
	if err != nil {
		return Minted{}, fmt.Errorf("broker: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	row, err := selectGrantApprovalForUpdate(ctx, tx, grantID, approvalHint)
	if err != nil {
		return Minted{}, err
	}
	if row.grantRunID != caller.RunID {
		return Minted{}, ErrRunMismatch
	}

	if row.grantSpec.Kind == types.GrantCloudSTS {
		b.auditMint(ctx, caller, grantID, row.approvalID, "", row.grantSpec.Scope, "denied")
		return Minted{}, ErrRequiresSPIRE
	}

	// Single-use guard: a written minted_jti blocks re-mint.
	if row.mintedJTI != "" {
		return Minted{}, ErrAlreadyMinted
	}

	// Chokepoint self-enforcement: an approval-required grant must carry an
	// approval row. MintForGrant's routing guarantees this, but re-check it in
	// the tx so the mint is self-contained for EVERY entry point (MintOnApproval
	// passes a Nil approval hint and would otherwise auto-mint an approval-gated
	// grant that has no approval row). Fail closed.
	if row.grantSpec.RequiresApproval && !row.hasApproval {
		b.auditMint(ctx, caller, grantID, uuid.Nil, "", row.grantSpec.Scope, "denied")
		return Minted{}, ErrNotApproved
	}

	if row.hasApproval {
		if row.approvalState != types.ApprovalApproved {
			if row.approvalState == types.ApprovalPending {
				return Minted{}, ErrApprovalPending{ApprovalID: row.approvalID}
			}
			return Minted{}, ErrNotApproved
		}
		if row.approvalRunID != caller.RunID {
			return Minted{}, ErrRunMismatch
		}
		// No-widening: the approver saw exactly requested_scope; it must
		// deep-equal the grant spec scope.
		if !jsonScopeEqual(row.requestedScope, row.grantSpec.Scope) {
			b.auditMint(ctx, caller, grantID, row.approvalID, "", row.grantSpec.Scope, "denied")
			return Minted{}, ErrScopeMismatch
		}
	}

	// Kill-switch: refuse to mint for a run that has already been revoked. The
	// check runs inside the mint tx so a durably-recorded revocation blocks a
	// subsequent mint — closing the gap where a mint reaching the broker after
	// the run was killed still produced a live credential. The sub-RTT concurrent
	// case (a revoke committing during this tx, after the check) is the published
	// 1h-minted-token residual, not closed here. See threatmodel §5 #7.
	if revoked, err := runRevoked(ctx, tx, row.grantRunID); err != nil {
		return Minted{}, err
	} else if revoked {
		b.auditMint(ctx, caller, grantID, row.approvalID, "", row.grantSpec.Scope, "denied")
		return Minted{}, ErrRunRevoked
	}

	// Mint the kind-specific credential.
	minted, err := b.mintKind(ctx, caller, row.grantSpec)
	if err != nil {
		b.auditMint(ctx, caller, grantID, row.approvalID, "", row.grantSpec.Scope, "failure")
		return Minted{}, err
	}
	minted.GrantID = grantID
	minted.ApprovalID = row.approvalID

	// Write minted_jti back in the SAME transaction (the provable join), and
	// require the conditional UPDATE to affect exactly one row. This rows-affected
	// check is LOAD-BEARING for single-use, not a backstop: the row.mintedJTI fast
	// path above only catches contenders whose statement snapshot postdates the
	// winner's commit. A contender that BLOCKS on the FOR UPDATE OF g lock mid-tx
	// resumes on its ORIGINAL snapshot and reads a stale minted_jti='' from the
	// joined approval row — Postgres runs EvalPlanQual only for the locked tuple
	// (g), never re-fetching the non-locked, nullable-side approval — so it passes
	// the fast path and mints a real token. Only this conditional UPDATE, which
	// re-checks minted_jti='' against the latest committed row and returns 0 rows,
	// stops that second credential from being returned. On 0 rows we fail closed;
	// the deferred Rollback discards the tx, so the minted token above is never
	// returned and expires at its <=1h TTL. (Proven by a two-session PG16
	// experiment; see TestPG_ConcurrentMintOnApproval_ExactlyOnce.)
	if row.hasApproval {
		n, err := tx.Exec(ctx,
			`UPDATE approvals SET minted_jti = $1 WHERE id = $2 AND minted_jti = ''`,
			minted.JTI, row.approvalID)
		if err != nil {
			return Minted{}, fmt.Errorf("broker: write minted_jti: %w", err)
		}
		if n != 1 {
			// A concurrent mint already claimed this approval. The token minted
			// above is discarded (never returned); audit the loss so the throwaway
			// mint is visible in the trail rather than silent.
			b.auditMint(ctx, caller, grantID, row.approvalID, "", row.grantSpec.Scope, "denied")
			return Minted{}, ErrAlreadyMinted
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Minted{}, fmt.Errorf("broker: commit mint tx: %w", err)
	}
	committed = true

	b.auditMint(ctx, caller, grantID, row.approvalID, minted.JTI, row.grantSpec.Scope, "success")

	// Register the minted token in the mask registry so PTY/asciicast streams
	// can mask verbatim occurrences of the credential. A nil registry is a
	// no-op; only value-bearing kinds (github_token, git_pat) set Token — api_key
	// never does (its value stays proxy-side).
	if minted.Token != "" && b.maskReg != nil {
		b.maskReg.Add(caller.RunID, []byte(minted.Token))
	}

	return minted, nil
}

// mintKind dispatches to the kind-specific minter. github_token scopes are
// clamped to the contents:write + pull_requests:write ceiling and tagged with
// the per-run branch namespace. api_key resolves to a proxy InjectionRule
// (secret value never returned). cloud_sts is refused (caller already checked).
func (b *Broker) mintKind(ctx context.Context, caller *identity.Claims, spec types.GrantSpec) (Minted, error) {
	ttl := ttlFor(spec)
	switch spec.Kind {
	case types.GrantGitHubToken:
		return b.mintGitHub(ctx, caller, spec, ttl)
	case types.GrantAPIKey:
		return b.mintAPIKey(spec)
	case types.GrantGitPAT:
		return b.mintGitPAT(ctx, spec)
	case types.GrantSSHKey:
		return b.mintSSHKey(ctx, spec)
	case types.GrantCloudSTS:
		return Minted{}, ErrRequiresSPIRE
	default:
		return Minted{}, fmt.Errorf("%w: %q", ErrUnknownGrantKind, spec.Kind)
	}
}

// githubScope is the JSON shape of a github_token grant scope.
type githubScope struct {
	Repos       []string          `json:"repos"`
	Permissions map[string]string `json:"permissions"`
}

func (b *Broker) mintGitHub(ctx context.Context, caller *identity.Claims, spec types.GrantSpec, ttl time.Duration) (Minted, error) {
	if b.github == nil {
		return Minted{}, errors.New("broker: github_token grant but no GitHubMinter configured (fail closed)")
	}
	var sc githubScope
	if err := json.Unmarshal(spec.Scope, &sc); err != nil {
		return Minted{}, fmt.Errorf("broker: decode github scope: %w", err)
	}

	// Branch-confinement clamp: permissions ceiling is contents:write +
	// pull_requests:write MAX. Requested perms are intersected DOWN to this
	// ceiling; anything outside is dropped (fail closed, never widen).
	clamped := clampGitHubPermissions(sc.Permissions)

	token, expiresAt, err := b.github.MintInstallationToken(ctx, sc.Repos, clamped, ttl)
	if err != nil {
		return Minted{}, fmt.Errorf("broker: mint installation token: %w", err)
	}

	branchNS := fmt.Sprintf(branchNamespaceFormat, caller.RunID.String())
	clampedJSON, _ := json.Marshal(clamped)
	reposJSON, _ := json.Marshal(sc.Repos)
	return Minted{
		Kind:      types.GrantGitHubToken,
		JTI:       newJTI(),
		ExpiresAt: expiresAt,
		Token:     token,
		Metadata: map[string]string{
			"branch_namespace": branchNS,
			"repos":            string(reposJSON),
			"permissions":      string(clampedJSON),
		},
	}, nil
}

// apiKeyScope is the JSON shape of an api_key grant scope.
type apiKeyScope struct {
	Host       string `json:"host"`
	Header     string `json:"header"`
	Format     string `json:"format"`
	SecretName string `json:"secret_name"`
}

// mintAPIKey resolves the secret NAME to a proxy InjectionRule. The secret
// VALUE is never read or returned here — late binding happens proxy-side, at
// egress time, by name. This is intentional late binding, not an oversight:
// existence is checked earlier, at create time, by validateInlineSecretRefs
// (internal/api/inline_policy.go) for both the inline and stored/default
// policy paths; mintAPIKey itself does NOT re-check existence here.
func (b *Broker) mintAPIKey(spec types.GrantSpec) (Minted, error) {
	var sc apiKeyScope
	if err := json.Unmarshal(spec.Scope, &sc); err != nil {
		return Minted{}, fmt.Errorf("broker: decode api_key scope: %w", err)
	}
	if sc.Host == "" || sc.SecretName == "" {
		return Minted{}, errors.New("broker: api_key scope requires host and secret_name")
	}
	format := sc.Format
	if format == "" {
		format = "Bearer %s"
	}
	header := sc.Header
	if header == "" {
		header = "Authorization"
	}
	return Minted{
		Kind:      types.GrantAPIKey,
		JTI:       newJTI(),
		ExpiresAt: time.Now().Add(ttlFor(spec)),
		Injection: &egress.InjectionRule{
			Host:       sc.Host,
			Header:     header,
			SecretName: sc.SecretName,
			Format:     format,
		},
		Metadata: map[string]string{"secret_name": sc.SecretName, "host": sc.Host},
	}, nil
}

// gitPATScope is the JSON shape of a git_pat grant scope.
type gitPATScope struct {
	Host       string `json:"host"`
	SecretName string `json:"secret_name"`
	Username   string `json:"username"`
}

// reservedBrokerSecretNames mirrors internal/api.sinkReservedSecret at the broker
// SINK: names that must never be resolved into a credential VALUE. Returning
// wardyn-signing-key / wardyn-session-key as a git password would let a policy
// exfiltrate the identity-signing or session-HMAC key. The three resident AWS
// Bedrock SigV4 credentials (aws-access-key-id / aws-secret-access-key /
// aws-session-token) are here for the same reason: resolveBedrockAuth reads them
// DIRECTLY to sign requests, never via a grant, so a git_pat/ssh_key grant naming
// one is only an exfil attempt. bedrock-api-key is intentionally ABSENT — the
// Bedrock BEARER path authors a host-pinned api_key grant that legitimately
// resolves it, and api_key values never leave the broker anyway (resolved at the
// injection sink, not here). The broker cannot import the api package, so this
// list is kept in sync by hand; the policy validator rejects these at write time.
var reservedBrokerSecretNames = map[string]bool{
	"wardyn-signing-key":    true,
	"wardyn-session-key":    true,
	"aws-access-key-id":     true,
	"aws-secret-access-key": true,
	"aws-session-token":     true,
}

// reservedBrokerSecret mirrors internal/api.reservedSecret (secrets.go): the
// static keys above PLUS the managed-harness OAuth-blob pattern
// (wardyn-harness-<provider>-oauth). The static map alone missed the pattern, so
// a policy could name e.g. "wardyn-harness-anthropic-oauth" as a git_pat/ssh_key
// secret and have the broker resolve the resident OAuth token into the sandbox.
func reservedBrokerSecret(name string) bool {
	if reservedBrokerSecretNames[name] {
		return true
	}
	return strings.HasPrefix(name, "wardyn-harness-") && strings.HasSuffix(name, "-oauth")
}

// mintGitPAT resolves a stored Personal Access Token and returns its VALUE to
// the git credential helper as username/password for a matched non-GitHub host.
//
// This is the OPPOSITE of mintAPIKey (whose secret value never leaves the
// broker; the proxy injects it header-side): git-over-HTTPS to ADO/GitLab is an
// opaque CONNECT tunnel the proxy cannot inject Basic-auth into without MITM, so
// the PAT MUST reach git through the helper — exactly like the minted
// github_token. Fails closed on missing host/secret_name, a reserved secret
// name (defense-in-depth at the sink), or an unresolvable secret.
//
// ExpiresAt is only an emission/freshness window (ttlFor) — the PAT
// itself is a long-lived, operator-managed secret that Wardyn CANNOT expire or
// down-scope; per-use revocation/scoping would need the host's token API
// (ADO/GitLab), out of scope. This is the honesty ceiling for this grant kind.
// The returned Token is masked from PTY/asciicast by the maskReg.Add in mint().
func (b *Broker) mintGitPAT(ctx context.Context, spec types.GrantSpec) (Minted, error) {
	var sc gitPATScope
	if err := json.Unmarshal(spec.Scope, &sc); err != nil {
		return Minted{}, fmt.Errorf("broker: decode git_pat scope: %w", err)
	}
	if sc.Host == "" || sc.SecretName == "" {
		return Minted{}, errors.New("broker: git_pat scope requires host and secret_name")
	}
	if reservedBrokerSecret(sc.SecretName) {
		return Minted{}, fmt.Errorf("broker: git_pat secret name %q is reserved for platform internals", sc.SecretName)
	}
	if b.secrets == nil {
		return Minted{}, errors.New("broker: git_pat grant but no secret store configured (fail closed)")
	}
	value, err := b.secrets.Get(ctx, sc.SecretName)
	if err != nil {
		return Minted{}, fmt.Errorf("broker: read git_pat secret %q: %w", sc.SecretName, err)
	}
	return Minted{
		Kind:      types.GrantGitPAT,
		JTI:       newJTI(),
		ExpiresAt: time.Now().Add(ttlFor(spec)),
		Token:     string(value),
		Username:  gitPATUsername(sc.Host, sc.Username),
		Metadata:  map[string]string{"secret_name": sc.SecretName, "host": sc.Host},
	}, nil
}

// sshKeyScope is the JSON shape of an ssh_key grant scope.
type sshKeyScope struct {
	Host                string `json:"host"`
	KeySecretRef        string `json:"key_secret_ref"`
	Username            string `json:"username"`
	KnownHostsSecretRef string `json:"known_hosts_secret_ref"`
}

// mintSSHKey resolves a stored SSH PRIVATE KEY and returns its VALUE (plus, when
// named, the known_hosts material) to agent-run for a git-over-SSH clone.
//
// SECURITY EXCEPTION (documented honestly, mirrors mintGitPAT's honesty ceiling):
// git's SSH transport has NO credential-helper seam (git credential.helper is
// HTTP-only), so — unlike git_pat (returned to the helper, never on disk) or
// api_key (never leaves the broker; the proxy injects it) — an SSH key CANNOT be
// brokered without becoming resident: the ssh client reads it from a file. So the
// key material is returned here and agent-run writes it 0400, agent-owned, then
// WIPES it right after the clone (deploy/images/*/agent-run). The readable window
// is the clone only, but within it code running AS the agent uid can read the key
// — the same residual as WARDYN_GIT_HELPER_SECRET. This is the accepted
// exception the owner chose when enabling the SSH SCM lane; there is no way to
// down-scope or expire an SSH private key from Wardyn's side (out of scope, host
// SSH-key API). The returned Token is mask-registered by mint()'s maskReg.Add.
//
// Fails closed on missing host/key_secret_ref, a reserved secret name (defense-
// in-depth at the sink), an unresolvable key secret, or an unresolvable
// known_hosts secret when one was named.
func (b *Broker) mintSSHKey(ctx context.Context, spec types.GrantSpec) (Minted, error) {
	var sc sshKeyScope
	if err := json.Unmarshal(spec.Scope, &sc); err != nil {
		return Minted{}, fmt.Errorf("broker: decode ssh_key scope: %w", err)
	}
	if sc.Host == "" || sc.KeySecretRef == "" {
		return Minted{}, errors.New("broker: ssh_key scope requires host and key_secret_ref")
	}
	if reservedBrokerSecret(sc.KeySecretRef) || reservedBrokerSecret(sc.KnownHostsSecretRef) {
		return Minted{}, fmt.Errorf("broker: ssh_key secret name is reserved for platform internals")
	}
	if b.secrets == nil {
		return Minted{}, errors.New("broker: ssh_key grant but no secret store configured (fail closed)")
	}
	key, err := b.secrets.Get(ctx, sc.KeySecretRef)
	if err != nil {
		return Minted{}, fmt.Errorf("broker: read ssh_key secret %q: %w", sc.KeySecretRef, err)
	}
	// Optional operator-supplied known_hosts (for a custom host the image-baked
	// /etc/ssh/ssh_known_hosts does not cover). For github.com / ADO the baked file
	// is authoritative and this ref is normally unset.
	var knownHosts string
	if sc.KnownHostsSecretRef != "" {
		kh, kerr := b.secrets.Get(ctx, sc.KnownHostsSecretRef)
		if kerr != nil {
			return Minted{}, fmt.Errorf("broker: read ssh_key known_hosts secret %q: %w", sc.KnownHostsSecretRef, kerr)
		}
		knownHosts = string(kh)
	}
	username := sc.Username
	if username == "" {
		username = "git" // github.com and ssh.dev.azure.com both authenticate as user "git"
	}
	return Minted{
		Kind:       types.GrantSSHKey,
		JTI:        newJTI(),
		ExpiresAt:  time.Now().Add(ttlFor(spec)),
		Token:      string(key),
		Username:   username,
		KnownHosts: knownHosts,
		Metadata:   map[string]string{"key_secret_ref": sc.KeySecretRef, "host": sc.Host},
	}, nil
}

// RevokeRun best-effort revokes credentials minted for a run, part of the
// kill-switch cascade. HONEST LIMITATION: GitHub App installation tokens
// CANNOT be revoked individually before their (<=1h) expiry — the GitHub API
// has no per-token revocation endpoint. We therefore (a) audit each minted jti
// as a revoke with outcome=success for the audit join, recording in the event
// data that GitHub tokens expire rather than revoke, and (b) rely on identity
// revocation (identity.Provider.RevokeRun, called by the kill cascade) to deny
// any further mints for the run. Returns the count attempted.
func (b *Broker) RevokeRun(ctx context.Context, runID uuid.UUID) error {
	jtis, err := b.mintedJTIsForRun(ctx, runID)
	if err != nil {
		return err
	}
	actor := spiffeForRun(runID)
	for _, jti := range jtis {
		data, _ := json.Marshal(map[string]any{
			"jti":  jti,
			"note": "github installation tokens expire (<=1h); no per-token revocation API — relying on TTL expiry + identity denylist",
		})
		ev := types.AuditEvent{
			ID:        uuid.New(),
			Time:      time.Now().UTC(),
			RunID:     &runID,
			ActorType: types.ActorSystem,
			Actor:     "wardyn-broker",
			Action:    "credential.revoke",
			Target:    actor,
			Outcome:   "success",
			Data:      data,
		}
		if err := b.audit.Record(ctx, ev); err != nil {
			audit.LogWriteFailure(ctx, ev, err)
		}
	}
	return nil
}

// auditMint emits a credential.mint audit event with full attribution.
func (b *Broker) auditMint(ctx context.Context, caller *identity.Claims, grantID, approvalID uuid.UUID, jti string, scope json.RawMessage, outcome string) {
	d := map[string]any{
		"grant_id": grantID.String(),
		"scope":    json.RawMessage(scope),
	}
	if approvalID != uuid.Nil {
		d["approval_id"] = approvalID.String()
	}
	if jti != "" {
		d["jti"] = jti
	}
	data, _ := json.Marshal(d)
	var runID *uuid.UUID
	if caller.RunID != uuid.Nil {
		r := caller.RunID
		runID = &r
	}
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		RunID:     runID,
		ActorType: types.ActorAgent,
		Actor:     caller.SPIFFEID,
		Action:    "credential.mint",
		Target:    grantID.String(),
		Outcome:   outcome,
		Data:      data,
	}
	if err := b.audit.Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
}
