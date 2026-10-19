// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// runner_resident ssh_key and git_pat (design 0.9 §7.4, §6): neither has a
// sandbox field to carry a placeholder. The sandbox mints both through its
// proxy, the ssh key into a 0400 file at clone time and the PAT through the git
// credential helper; for a resident grant the runner's proxy answers that mint
// from ProxyConfig.ResidentMints, whose Token the runner fills from its
// keystore, and never past ExpiresAt.

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// residentPATBrokered is the refusal of a runner_resident git_pat while the PAT
// broker is on: the broker would mint that PAT proxy-side, so it is never also
// resident. Create, dispatch and revive refuse with it.
const residentPATBrokered = "a git_pat is brokered proxy-side while WARDYN_GIT_PAT_BROKER is on"

// authorResidentMints authors the run's runner_resident ssh_key and git_pat
// grants onto spec.ProxyConfig.ResidentMints and adds their values to resident,
// for deliverResident. false means the run has been failed.
//
//   - A resident git_pat with the PAT broker on refuses the run; it is never
//     dropped silently (the broker-off flag was bound at admission).
//   - A grant is authored only when its id is in the post-drop map dispatch
//     wrote into spec's env (WARDYN_SSH_GRANTS, WARDYN_GIT_PAT_GRANTS), so a
//     forge brokered by the GitHub App keeps its single lane.
//   - Each value is read from the namespace localGrantOrigin proved, with no
//     owner fallback, audited and masked (readResidentValue); a value that
//     cannot be read skips its grant, as today's skip rules do, and the proxy
//     then relays the mint, which the control plane refuses for a grant not
//     recorded own.
//   - ExpiresAt is the not_after a delivery now would carry. A stored PAT or key
//     has no expiry of its own, so nothing earlier applies, and the delivery's
//     own not_after, a moment later, is never earlier than it.
func (s *Server) authorResidentMints(ctx context.Context, run types.AgentRun, rows []types.CredentialGrant,
	patBroker bool, spec *runner.SandboxSpec, resident *[]residentValue) bool {
	var mints []proxy.ResidentMint
	sorted := slices.SortedFunc(slices.Values(rows), func(a, b types.CredentialGrant) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	for _, row := range sorted {
		kind := row.Spec.Kind
		if row.Delivery != types.GrantDeliveryResident || (kind != types.GrantSSHKey && kind != types.GrantGitPAT) {
			continue
		}
		if kind == types.GrantGitPAT && patBroker {
			s.refuseResidentDispatch(ctx, run, row.ID, residentPATBrokered)
			return false
		}
		env := "WARDYN_SSH_GRANTS"
		if kind == types.GrantGitPAT {
			env = "WARDYN_GIT_PAT_GRANTS"
		}
		if !grantMapNames(cmp.Or(spec.Env[env], spec.SecretEnv[env]), row.ID) {
			continue
		}
		host, secretName, knownHostsRef, _, err := storedSecretGrantPairing(row.Spec)
		if err != nil {
			s.skipResidentMint(ctx, run, row, "the grant's scope names no stored secret")
			continue
		}
		mint := proxy.ResidentMint{GrantID: row.ID, Kind: kind, Token: placement.ResidentPlaceholder(row.ID), ExpiresAt: residentNotAfter(run, s.cfg.Now())}
		if kind == types.GrantSSHKey {
			_, _, user, _, _ := sshKeyScopeFields(row.Spec.Scope)
			mint.Username, mint.Host = user, host
			if mint.Username == "" {
				mint.Username = "git" // as the broker answers: github.com and ssh.dev.azure.com both take "git"
			}
			if knownHostsRef != "" {
				kh, ok := s.readResidentKnownHosts(ctx, run, row, knownHostsRef)
				if !ok {
					s.skipResidentMint(ctx, run, row, "its known_hosts secret could not be read")
					continue
				}
				mint.KnownHosts = kh
			}
		} else {
			sc, _ := types.DecodeGitPATScope(row.Spec.Scope)
			mint.Username = broker.GitPATUsername(host, sc.Username)
		}
		v, err := s.readResidentValue(ctx, run, row, secretName)
		if err != nil {
			s.skipResidentMint(ctx, run, row, "its stored secret could not be read")
			continue
		}
		mints = append(mints, mint)
		*resident = append(*resident, v)
	}
	spec.ProxyConfig.ResidentMints = mints
	return true
}

// grantMapNames reports whether a {host: grant_id} sandbox map names id.
func grantMapNames(raw string, id uuid.UUID) bool {
	var m map[string]uuid.UUID
	if raw == "" || json.Unmarshal([]byte(raw), &m) != nil {
		return false
	}
	for _, v := range m {
		if v == id {
			return true
		}
	}
	return false
}

// readResidentKnownHosts reads a resident ssh_key's paired known_hosts from the
// namespace its key is read from, recorded as secret.read with purpose
// runner-resident. It is public host-key material, so it goes inline on the
// mint and is not masked; a read that fails skips the grant, as the broker's
// mint fails without it.
func (s *Server) readResidentKnownHosts(ctx context.Context, run types.AgentRun, row types.CredentialGrant, name string) (string, bool) {
	if s.cfg.Secrets == nil || run.RunnerID == nil {
		return "", false
	}
	data := map[string]any{"purpose": string(secretstore.PurposeRunnerResident), "grant_id": row.ID, "runner_id": *run.RunnerID,
		"delivery": placement.ClassSSHKey, "owner": "", "known_hosts": true}
	rctx, srow := secretstore.SiteAudited(ctx)
	val, err := s.cfg.Secrets.For("").Get(rctx, name)
	if err == nil && len(val) == 0 {
		err = secretstore.ErrNotFound
	}
	data = withStoreRow(data, srow)
	if err != nil {
		_, data["reason"], _ = storeReadRefusal(name, err)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "secret.read", name, "failure", mustJSON(data)))
		return "", false
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "secret.read", name, "success", mustJSON(data)))
	return string(val), true
}

// refuseResidentDispatch fails the run before any lease or create, with a
// run.dispatch failure row {reason, grant_id, runner_id}, the shape a refused
// delivery takes (deliverResident).
func (s *Server) refuseResidentDispatch(ctx context.Context, run types.AgentRun, grantID uuid.UUID, detail string) {
	data := map[string]any{"reason": placement.ReasonPlacementCredential, "grant_id": grantID}
	if run.RunnerID != nil {
		data["runner_id"] = *run.RunnerID
	}
	bg := context.WithoutCancel(ctx)
	s.failAndRevoke(bg, run.ID, types.RunStarting, "This run was not launched on your own runner: "+detail)
	s.recordAudit(bg, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch", run.ID.String(), "failure", mustJSON(data)))
}

// skipResidentMint records a resident ssh_key or git_pat grant that dispatch
// authors no mint for: the clone then meets a refusal at the proxy, and this
// row names the grant and why. The run is not failed.
func (s *Server) skipResidentMint(ctx context.Context, run types.AgentRun, row types.CredentialGrant, reason string) {
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.resident_mint.skip", row.ID.String(), "failure",
		mustJSON(map[string]any{"grant_id": row.ID, "kind": row.Spec.Kind, "reason": reason})))
}

// residentRunnerTooOld is the refusal of a resident run whose runner does not
// advertise Caps.Resident.
const residentRunnerTooOld = "the runner does not hold runner_resident credentials; upgrade wardyn-runnerd to 0.9.3 or later"

// beginValueResolution opens the run's masking manifest, then refuses a
// resident run whose runner cannot hold its values: both before dispatch
// resolves any value. false means the run has been failed.
func (s *Server) beginValueResolution(ctx context.Context, run types.AgentRun) bool {
	return s.beginMaskManifest(ctx, run) && s.residentRunnerReady(ctx, run)
}

// residentRunnerReady refuses a local run with a runner_resident grant whose
// runner does not advertise Caps.Resident, before dispatch reads any resident
// value or authors any mint; false means the run has been failed.
func (s *Server) residentRunnerReady(ctx context.Context, run types.AgentRun) bool {
	if run.Placement != types.PlacementLocal || run.RunnerID == nil {
		return true
	}
	rows, err := s.cfg.Store.ListGrantsByRun(ctx, run.ID)
	if err != nil {
		s.refuseResidentDispatch(ctx, run, uuid.Nil, "its grants could not be read to check its runner holds runner_resident credentials")
		return false
	}
	i := slices.IndexFunc(rows, func(g types.CredentialGrant) bool { return g.Delivery == types.GrantDeliveryResident })
	if i < 0 {
		return true
	}
	if c, ok := s.runnerEndpoint(*run.RunnerID).sub.Caps(); ok && c.Resident {
		return true
	}
	s.refuseResidentDispatch(ctx, run, rows[i].ID, residentRunnerTooOld)
	return false
}
