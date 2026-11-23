// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run_dispatch_secret_facts row: what a dispatch, and every later revive, decided about the two credentials
// that have no row of their own (a Bedrock AWS sign-in whose role credentials live in the sandbox, and a git token
// the sandbox's helper holds), and the id of the run token it gave the run's proxy. A snapshot's secrets check
// reads the row and never server config, and trusts it only while that id is the one in the run's stored proxy
// config (secretsPresent).

// errNoSecretFactsStore: this store cannot record the facts a snapshot's check reads.
var errNoSecretFactsStore = errors.New("this store does not record run secret facts")

// runTokenAlgorithms are the signature algorithms runTokenJTI accepts when it parses a run token. It verifies
// nothing, so the list only has to cover what an identity provider may sign with.
var runTokenAlgorithms = []jose.SignatureAlgorithm{
	jose.ES256, jose.ES384, jose.ES512, jose.RS256, jose.RS384, jose.RS512, jose.PS256, jose.PS384, jose.PS512, jose.EdDSA,
}

// runTokenJTI reads a run token's jti without checking its signature or expiry. It is only used on a token read
// back from the run's own sealed proxy config, or handed to this dispatch, whose seal or origin is the integrity;
// a stored token is usually past its expiry, which a verified read would refuse.
func runTokenJTI(token string) (string, error) {
	parsed, err := jwt.ParseSigned(token, runTokenAlgorithms)
	if err != nil {
		return "", fmt.Errorf("parse the run token: %w", err)
	}
	var claims jwt.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return "", fmt.Errorf("read the run token's claims: %w", err)
	}
	if claims.ID == "" {
		return "", errors.New("the run token has no jti")
	}
	return claims.ID, nil
}

// currentRunJTI is the jti of the run token in the run's stored proxy config: the token its running proxy was
// started with. Dispatch stores it and every revive replaces it; a proxy renewal does not, which is why
// agent_runs.token_jti is not read here.
func (s *Server) currentRunJTI(ctx context.Context, runID uuid.UUID) (string, error) {
	raw, err := s.loadRunProxyConfig(ctx, runID)
	if err != nil {
		return "", err
	}
	var cfg struct {
		RunToken string `json:"run_token"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("parse the run's proxy config: %w", err)
	}
	return runTokenJTI(cfg.RunToken)
}

// recordDispatchSecretFacts writes the row for a linux_vm run dispatched while features.machine_snapshots is on,
// before its sandbox is created. With the switch off, or for any other machine, it writes nothing, and such a run
// can never be snapshotted. A row that cannot be written fails the run: it would otherwise hold a credential the
// check cannot see. held is whether every dispatch check before it held: when it is false nothing is written and
// false is returned. ok=false: the run is already FAILED.
func (s *Server) recordDispatchSecretFacts(ctx context.Context, run types.AgentRun, p dispatchParams, llm llmTransport,
	siteCfg types.SiteConfig, siteCfgOK, held bool,
) bool {
	if !held {
		return false
	}
	if run.MachineKind != types.MachineLinuxVM || !siteCfgOK || !machineSnapshotsOn(siteCfg) {
		return true
	}
	facts := store.DispatchSecretFacts{
		BedrockResident: llm.bedrockReady && llm.bedrockAudit.mode != "bearer",
		GitTokenResident: !p.PATBroker && slices.ContainsFunc(p.Policy.EligibleGrants, func(g types.GrantSpec) bool {
			return g.Kind == types.GrantGitPAT
		}),
	}
	// A token whose jti cannot be read leaves the row's jti empty, which no stored config matches, so every
	// snapshot of the run is refused; the run itself still starts.
	if jti, err := runTokenJTI(p.RunToken); err != nil {
		slog.WarnContext(ctx, "api: the run token's jti could not be read; the run cannot be snapshotted",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
	} else {
		facts.RecordedJTI = jti
	}
	err := errNoSecretFactsStore
	if st, ok := s.cfg.Store.(store.RunSecretFacts); ok {
		err = st.RecordRunDispatchSecretFacts(ctx, run.ID, facts)
	}
	if err == nil {
		return true
	}
	slog.ErrorContext(ctx, "api: record the run's secret facts", slog.String("run_id", run.ID.String()), slog.Any("err", err))
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch", run.ID.String(), "failure",
		mustJSON(map[string]any{"reason": reasonRunSecretFactsUnrecorded})))
	s.failAndRevoke(ctx, run.ID, types.RunStarting, "This run was not launched: the facts a snapshot of it is checked against could not be recorded.")
	return false
}

// recordRevivedSecretFacts moves the row to a revive's new run token, whatever the switch says, and ORs in
// whether the revived proxy hands a git token to the sandbox (a git_pat grant the revive did not broker). It only
// updates a row the dispatch wrote. A failure is logged and not returned: the row then still names the old token,
// which the stored config no longer holds, so every snapshot of the run is refused until a later revive succeeds.
func (s *Server) recordRevivedSecretFacts(ctx context.Context, run types.AgentRun, cfg *proxy.Config, freshJTI string) {
	st, ok := s.cfg.Store.(store.RunSecretFacts)
	if !ok {
		return
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err == nil {
		raw := slices.ContainsFunc(grants, func(g types.CredentialGrant) bool {
			return g.Spec.Kind == types.GrantGitPAT && !slices.Contains(cfg.BrokeredPATGrantIDs, g.ID)
		})
		err = st.RecordRevivedSecretFacts(ctx, run.ID, raw, freshJTI)
	}
	if err != nil {
		slog.ErrorContext(ctx, "api: record a revived run's secret facts; the run cannot be snapshotted until its next revive",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
}
