// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The audit rows of an MCP server's own sandbox. Each names the server by its ordinal and key, and never its
// command, arguments or environment, which a person may have written.

// mcpSandboxDatum is what an MCP server's rows say of it, with the cause when there is one.
func mcpSandboxDatum(row store.RunMCPSandbox, cause string) map[string]any {
	d := map[string]any{"ordinal": row.Ordinal, "mcp_sandbox": row.ServerKey}
	if cause != "" {
		d["cause"] = cause
	}
	return d
}

// auditMCPSandboxCreate writes run.mcp_sandbox.create for row: success with its sandbox, or failure or denied
// with the cause.
func (s *Server) auditMCPSandboxCreate(ctx context.Context, row store.RunMCPSandbox, outcome, cause string) {
	data := mcpSandboxDatum(row, cause)
	if row.SandboxRef != "" {
		data["sandbox_ref"] = row.SandboxRef
	}
	s.recordAudit(ctx, s.auditEvent(&row.RunID, types.ActorSystem, "wardynd", "run.mcp_sandbox.create", row.ServerKey, outcome, mustJSON(data)))
}

// auditMCPSandboxRestart writes run.mcp_sandbox.restart for row, by the restart watcher (actor system) or by
// a person, with the cause and the restarts the row counts in its window.
func (s *Server) auditMCPSandboxRestart(ctx context.Context, actorType types.ActorType, actor string, row store.RunMCPSandbox, outcome, cause string) {
	data := mcpSandboxDatum(row, cause)
	data["restarts"] = row.Restarts
	s.recordAudit(ctx, s.auditEvent(&row.RunID, actorType, actor, "run.mcp_sandbox.restart", row.ServerKey, outcome, mustJSON(data)))
}

// auditMCPSandboxEnd writes run.mcp_sandbox.end for row with its cause, by the run's end (actor system) or a
// person's stop. With row nil it is the end of every server of the run whose rows could not be read.
func (s *Server) auditMCPSandboxEnd(ctx context.Context, runID uuid.UUID, actorType types.ActorType, actor string, row *store.RunMCPSandbox, outcome, cause string) {
	data, target := map[string]any{}, ""
	if row != nil {
		data, target = mcpSandboxDatum(*row, cause), row.ServerKey
	} else if cause != "" {
		data["cause"] = cause
	}
	s.recordAudit(ctx, s.auditEvent(&runID, actorType, actor, "run.mcp_sandbox.end", target, outcome, mustJSON(data)))
}

// The causes a row's move to stopped records (last_cause), one per writer.
const (
	mcpEndCompleted = "completed"     // the run's end (finalizeRunEffects)
	mcpEndKilled    = "killed"        // a kill (killTeardownTail, the kill confirm)
	mcpEndUnstarted = "unstarted"     // dispatch returned before the run was RUNNING
	mcpEndLost      = "lost"          // a lost path
	mcpEndEnded     = "ended"         // the lease's end kept the run
	mcpEndPerson    = "person"        // a person's Stop
	mcpEndLimit     = "restart_limit" // past mcpRestartMax restarts in mcpRestartWindow
	mcpEndReplaced  = "superseded"    // a revive's keep found the row moved on
)

// mcpSandboxEndTimeout bounds one run's MCP end, its own bound and never the caller's: each row's graceful
// proxy stop, its kill and its row write (four servers' stops fit in it, EST).
const mcpSandboxEndTimeout = reconcileFinalizeTimeout

// mcpSandboxSeams are the store and the runner an MCP sandbox needs; ok is false on a deployment with neither.
func (s *Server) mcpSandboxSeams() (store.RunMCPSandboxStore, runner.MCPSandboxer, bool) {
	st, ok := s.cfg.Store.(store.RunMCPSandboxStore)
	m, mok := s.cfg.Runner.(runner.MCPSandboxer)
	return st, m, ok && mok
}

// mcpSandboxIDOf is the runner's name for row, which a row with no sandbox ref still has.
func mcpSandboxIDOf(run types.AgentRun, row store.RunMCPSandbox) runner.MCPSandboxID {
	return mcpSandboxIDAt(run, row.Ordinal)
}

// endRunMCPSandboxes ends every MCP sandbox of the run that is not stopped: each one's proxy is stopped
// gracefully, so it flushes its last decisions, its container is killed, and its row is stopped with cause.
// It runs on its own bound and returns nothing, so no caller's own stop or answer changes, and it is not
// fenced: a newer revive's servers are ended too, which costs availability only. When the rows cannot be
// read it kills every MCP object of the run by listing, so no container or secret outlives the cut; the rows
// keep their state until a later end or the watcher's kill. It is idempotent.
func (s *Server) endRunMCPSandboxes(ctx context.Context, runID uuid.UUID, cause string) {
	st, m, ok := s.mcpSandboxSeams()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpSandboxEndTimeout)
	defer cancel()
	rows, err := st.ListRunMCPSandboxes(ctx, runID)
	var run types.AgentRun
	if err == nil && slices.ContainsFunc(rows, mcpRowUp) {
		run, err = s.cfg.Store.GetRun(ctx, runID) // the class names the substrate
	}
	if err != nil {
		kerr := m.KillRunMCPSandboxes(ctx, runID)
		slog.WarnContext(ctx, "wardynd: a run's MCP servers could not be read at its end; every one found was killed",
			slog.String("run_id", runID.String()), slog.Any("err", err), slog.Any("kill_err", kerr))
		s.auditMCPSandboxEnd(ctx, runID, types.ActorSystem, "wardynd", nil, "failure", cause)
		return
	}
	for _, row := range rows {
		if mcpRowUp(row) {
			s.endMCPSandbox(ctx, run, row, cause)
		}
	}
}

// mcpRowUp reports whether a row is not stopped.
func mcpRowUp(row store.RunMCPSandbox) bool { return row.State != store.MCPSandboxStopped }

// endMCPSandbox is endRunMCPSandboxes' end of one row, by its name, so a starting row with no ref is ended
// too: the kill (the proxy drained first), then the row stopped with cause, then run.mcp_sandbox.end. A row
// another writer stopped first keeps its cause and writes no second row. A kill that failed leaves the row
// up, with a failure row, so the run's containment lists it and the kill confirm and the watcher kill it
// again; nothing restarts it, since a run that is over, lost or kept is never one mcpCreateAllowed admits.
// gone reports whether the kill succeeded.
func (s *Server) endMCPSandbox(ctx context.Context, run types.AgentRun, row store.RunMCPSandbox, cause string) (gone bool) {
	st, m, ok := s.mcpSandboxSeams()
	if !ok {
		return true
	}
	if kerr := m.KillMCPSandbox(ctx, mcpSandboxIDOf(run, row)); kerr != nil {
		slog.WarnContext(ctx, "wardynd: killing an MCP server's sandbox failed", slog.String("run_id", run.ID.String()),
			slog.String("server", row.ServerKey), slog.Any("err", kerr))
		s.auditMCPSandboxEnd(ctx, run.ID, types.ActorSystem, "wardynd", &row, "failure", cause)
		return false
	}
	stopped, werr := st.StopMCPSandboxRow(ctx, run.ID, row.Ordinal, cause)
	if werr == nil && !stopped {
		return true
	}
	outcome := "success"
	if werr != nil {
		outcome = "failure"
		slog.WarnContext(ctx, "wardynd: recording an MCP server's end failed", slog.String("run_id", run.ID.String()),
			slog.String("server", row.ServerKey), slog.Any("err", werr))
	}
	s.auditMCPSandboxEnd(ctx, run.ID, types.ActorSystem, "wardynd", &row, outcome, cause)
	return true
}

// mcpSandboxesUp is the keys of the run's MCP sandbox rows that are not stopped, in ordinal order.
func (s *Server) mcpSandboxesUp(ctx context.Context, runID uuid.UUID) ([]string, error) {
	st, _, ok := s.mcpSandboxSeams()
	if !ok {
		return nil, nil
	}
	rows, err := st.ListRunMCPSandboxes(ctx, runID)
	if err != nil {
		return nil, err
	}
	var up []string
	for _, row := range rows {
		if mcpRowUp(row) {
			up = append(up, row.ServerKey)
		}
	}
	return up, nil
}

// confirmMCPSandboxesGone is the kill confirm's MCP step: every sandbox of the run is killed again by its
// name, each row still up is ended, and while a kill fails or the rows cannot be read the run's containment
// is not confirmed (errKillConfirmBusy, retried).
func (s *Server) confirmMCPSandboxesGone(ctx context.Context, run types.AgentRun) error {
	st, m, ok := s.mcpSandboxSeams()
	if !ok {
		return nil
	}
	rows, err := st.ListRunMCPSandboxes(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("%w: read the run's MCP servers: %w", errKillConfirmBusy, err)
	}
	var left []string
	for _, row := range rows {
		var gone bool
		if mcpRowUp(row) {
			gone = s.endMCPSandbox(ctx, run, row, mcpEndKilled)
		} else {
			gone = m.KillMCPSandbox(ctx, mcpSandboxIDOf(run, row)) == nil
		}
		if !gone {
			left = append(left, row.ServerKey)
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("%w: the MCP servers %s are not confirmed gone", errKillConfirmBusy, strings.Join(left, ", "))
	}
	return nil
}

// mcpSandboxesUpAfterEnd is mcpSandboxesUp for a run that is over, nil for one still running: its MCP servers
// are then the run's own, not containment.
func (s *Server) mcpSandboxesUpAfterEnd(ctx context.Context, run types.AgentRun) ([]string, error) {
	if !isTerminalRunState(run.State) {
		return nil, nil
	}
	return s.mcpSandboxesUp(ctx, run.ID)
}

// mcpServer is one server of the run that starts from a command, as the run's component snapshot holds it.
type mcpServer struct {
	index   int // its position in the snapshot's components, which mcpSandboxPolicy reads
	ordinal int // its run_components ordinal: its row's
	rank    int // its 0-based rank among the run's servers that start from a command (RelayRank)
	key     string
	def     types.ComponentDefinition
}

// mcpServersOf is comps' servers that start from a command, in ordinal order. An erased row has none.
func mcpServersOf(comps runComponents) []mcpServer {
	keys := mcpServerKeys(comps.attached)
	var out []mcpServer
	for i, a := range comps.attached {
		if !a.snapshot.Erased && a.snapshot.Definition.LocalMCP() {
			out = append(out, mcpServer{index: i, ordinal: a.snapshot.Ordinal, rank: len(out), key: keys[i], def: a.snapshot.Definition})
		}
	}
	return out
}

// mcpRunServers is the run's components as its snapshot holds them and its servers that start from a
// command. The snapshot is the one source at dispatch, a restart and a revive, so each derives the same
// keys, ordinals and policies.
func (s *Server) mcpRunServers(ctx context.Context, runID uuid.UUID) (runComponents, []mcpServer, error) {
	snap, err := s.runComponentSnapshot(ctx, runID)
	if err != nil {
		return runComponents{}, nil, err
	}
	var comps runComponents
	for _, rc := range snap {
		comps.attached = append(comps.attached, attachedComponent{snapshot: rc})
	}
	return comps, mcpServersOf(comps), nil
}

// mcpServerGrants is what one server's own secrets are delivered by: its env and file secret grants and its
// header injections.
type mcpServerGrants struct {
	env, files []types.GrantSpec
	inject     []runner.InjectionGrant
}

// mcpOwn is one server's own config and secrets, resolved for its sandbox only.
type mcpOwn struct {
	env, secretEnv map[string]string
	files          []runner.ManagedFile
	inject         []runner.InjectionGrant
	mitm           []string
	values         []string // each secret value, which the relay scrubs from every answer
}

// mcpMade is one server's sandbox as dispatch created it, for the start.
type mcpMade struct {
	srv mcpServer
	row store.RunMCPSandbox
	pol mcpPolicy
}

// mcpSandboxDispatch is dispatch's half of the run's servers that start from a command.
type mcpSandboxDispatch struct {
	comps   runComponents
	servers []mcpServer
	grants  []mcpServerGrants // by server
	own     []mcpOwn          // by server
	made    []mcpMade
}

// mcpSandboxDispatchFor reads the run's servers that start from a command when servers (the gate's dispatch
// half) has one; nil when it has none. false means the run was failed.
func (s *Server) mcpSandboxDispatchFor(ctx context.Context, run types.AgentRun, servers []componentMCP) (*mcpSandboxDispatch, bool) {
	if !slices.ContainsFunc(servers, func(m componentMCP) bool { return m.Launch != nil }) {
		return nil, true
	}
	comps, local, err := s.mcpRunServers(ctx, run.ID)
	if err != nil || len(local) == 0 {
		slog.ErrorContext(ctx, "wardynd: reading the run's MCP servers at dispatch failed", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "the run's MCP servers could not be read")
		return nil, false
	}
	return &mcpSandboxDispatch{comps: comps, servers: local, grants: make([]mcpServerGrants, len(local))}, true
}

// mcpSandboxDispatchAt is dispatch's first MCP step: the run's servers that start from a command, their own
// secrets taken off the agent's grants and injections, then resolved for their sandboxes. false means the run
// was failed.
func (s *Server) mcpSandboxDispatchAt(ctx context.Context, run types.AgentRun, servers []componentMCP, policy *types.RunPolicySpec,
	injections *[]runner.InjectionGrant, rows []types.CredentialGrant) (*mcpSandboxDispatch, bool) {
	d, ok := s.mcpSandboxDispatchFor(ctx, run, servers)
	if !ok {
		return nil, false
	}
	takeMCPServerSecrets(d, policy, injections)
	return d, s.resolveMCPServerSecrets(ctx, run, d, rows)
}

// takeMCPServerSecrets moves each server's own secrets off the agent: its env and file secret grants off
// policy, its header injections off injections, so the agent's sandbox and proxy hold none of them (I5 c).
// It runs after the ceiling's re-assertion, so what it takes is only what the run is still given. It
// removes and never adds.
func takeMCPServerSecrets(d *mcpSandboxDispatch, policy *types.RunPolicySpec, injections *[]runner.InjectionGrant) {
	if d == nil {
		return
	}
	for i, srv := range d.servers {
		for _, sec := range srv.def.Secrets {
			want := componentGrant(sec)
			for _, g := range policy.EligibleGrants {
				if sameSecretGrant(g, want) {
					switch g.Kind {
					case types.GrantEnvSecret:
						d.grants[i].env = append(d.grants[i].env, g)
					case types.GrantFileSecret:
						d.grants[i].files = append(d.grants[i].files, g)
					}
				}
			}
			policy.EligibleGrants = slices.DeleteFunc(policy.EligibleGrants, func(g types.GrantSpec) bool { return sameSecretGrant(g, want) })
			if sec.Delivery.Mode != types.ComponentDeliveryHeader {
				continue
			}
			*injections = slices.DeleteFunc(*injections, func(in runner.InjectionGrant) bool {
				if hostEqual(in.Rule.Host, sec.Delivery.Host) && in.Rule.SecretName == sec.SecretName {
					d.grants[i].inject = append(d.grants[i].inject, in)
					return true
				}
				return false
			})
		}
	}
}

// sameSecretGrant reports whether a and b deliver the same secret the same way: the kind, and the variable,
// file or host with the secret's name, read from each scope as its resolver reads it.
func sameSecretGrant(a, b types.GrantSpec) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case types.GrantEnvSecret:
		n1, s1, e1 := envSecretScopeFields(a.Scope)
		n2, s2, e2 := envSecretScopeFields(b.Scope)
		return e1 == nil && e2 == nil && n1 == n2 && s1 == s2
	case types.GrantFileSecret:
		f1, s1, e1 := fileSecretScopeFields(a.Scope)
		f2, s2, e2 := fileSecretScopeFields(b.Scope)
		return e1 == nil && e2 == nil && f1 == f2 && s1 == s2
	case types.GrantAPIKey:
		r1, e1 := injectionRuleFromScope(a.Scope)
		r2, e2 := injectionRuleFromScope(b.Scope)
		return e1 == nil && e2 == nil && hostEqual(r1.Host, r2.Host) && r1.SecretName == r2.SecretName
	}
	return false
}

// resolveMCPServerOwn resolves one server's config and secrets for its sandbox: its config as its plain env
// (each name the component validator admits), its env and file secrets through the agent's resolvers (the
// same checks, masking and audit rows) into maps of its own, and its header injections, with the hosts its
// proxy intercepts to set them. A refusal sentence means the server cannot be given its secrets: a runner
// that cannot deliver a file, or a grant kept on the runner, which an MCP sandbox has no keystore for.
func (s *Server) resolveMCPServerOwn(ctx context.Context, run types.AgentRun, srv mcpServer, g mcpServerGrants, rows []types.CredentialGrant) (mcpOwn, string) {
	own := mcpOwn{env: map[string]string{}, secretEnv: map[string]string{}, inject: g.inject}
	for _, k := range slices.Sorted(maps.Keys(srv.def.Config)) {
		if types.ValidComponentEnvName(k) == nil {
			own.env[k] = srv.def.Config[k]
		}
	}
	all := maps.Clone(own.env)
	names, envResident, placed := s.resolveEnvSecretValues(ctx, run, types.RunPolicySpec{EligibleGrants: g.env}, rows, all)
	if len(envResident) > 0 {
		return mcpOwn{}, "the MCP server " + srv.key + " has a secret kept on the runner, which its sandbox cannot be given"
	}
	for _, n := range names {
		own.secretEnv[n] = all[n]
	}
	for _, v := range placed {
		own.values = append(own.values, string(v))
	}
	files, _, fileResident, ok := s.resolveFileSecretValues(ctx, run, types.RunPolicySpec{EligibleGrants: g.files}, rows)
	if !ok {
		return mcpOwn{}, "the run's runner cannot deliver a file to the MCP server " + srv.key
	}
	if len(fileResident) > 0 {
		return mcpOwn{}, "the MCP server " + srv.key + " has a secret kept on the runner, which its sandbox cannot be given"
	}
	own.files = files
	for _, f := range files {
		own.values = append(own.values, string(f.Content))
	}
	for _, in := range g.inject {
		if in.Rule.RequireTLS {
			own.mitm = append(own.mitm, net.JoinHostPort(in.Rule.Host, componentHeaderPort))
		}
	}
	return own, ""
}

// resolveMCPServerSecrets resolves every server's own config and secrets (resolveMCPServerOwn), before the
// run's masking manifest completes, so each value is on it. false means the run was failed.
func (s *Server) resolveMCPServerSecrets(ctx context.Context, run types.AgentRun, d *mcpSandboxDispatch, rows []types.CredentialGrant) bool {
	if d == nil {
		return true
	}
	for i, srv := range d.servers {
		own, refused := s.resolveMCPServerOwn(ctx, run, srv, d.grants[i], rows)
		if refused != "" {
			s.failAndRevoke(ctx, run.ID, types.RunStarting, refused)
			return false
		}
		d.own = append(d.own, own)
	}
	return true
}

// mcpReadyWait is how long dispatch waits, for every server together, for its sandboxes to come up (Q6 A), and
// mcpReadyPoll how often it asks. Variables so a test can shorten them.
var (
	mcpReadyWait = 60 * time.Second
	mcpReadyPoll = 500 * time.Millisecond
)

// Causes of a failed row that are not an error's own reason (item 7).
const (
	mcpFailReadiness  = "readiness"
	mcpFailStateWrite = "state_write"
	mcpFailCreate     = "create"
	mcpFailStart      = "start"
)

// mcpCreateCause is a failed create's last_cause: the reason for a host the sandbox would be reachable
// from, else the step.
func mcpCreateCause(err error, step string) string {
	if errors.Is(err, runner.ErrMCPSandboxHostReachable) {
		return reasonMCPSandboxHostReachable
	}
	return step
}

// mcpRelayToken is a fresh relay token, 32 random bytes in base64url, and its hex SHA-256.
func mcpRelayToken() (token, digest string) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // crypto/rand.Read never returns an error
	token = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:])
}

// mintMCPSandboxToken is a fresh MCP-audience token of run, under its SPIFFE ID, for one sandbox's proxy.
func (s *Server) mintMCPSandboxToken(ctx context.Context, run types.AgentRun) (identity.RunIdentity, error) {
	sponsor, err := s.runSponsor(ctx, run.CreatedBy)
	if err != nil {
		return identity.RunIdentity{}, err
	}
	return s.mintRunIdentityAs(ctx, run.ID, runWorkloadOf(run.SPIFFEID, run.ID), runIdentitySubject(ctx, run.CreatedBy), sponsor, run.OperatorOwned, mcpSandboxAudience)
}

// mcpRelayListen is the MCP proxy's relay listener: the runner binds it to the proxy's address on the run's
// relay link and keeps the port.
const mcpRelayListen = ":3130"

// renderMCPProxyConfig is one MCP sandbox's proxy config: only the fields an MCP sandbox's proxy may carry
// (the proxy refuses any other), its policy, its token, its own CA, its header injections and the values
// its relay scrubs.
func (s *Server) renderMCPProxyConfig(run types.AgentRun, pol types.RunPolicySpec, token string, caCert, caKey []byte, relaySHA string, own mcpOwn) ([]byte, error) {
	inj := make([]proxy.InjectionConfig, 0, len(own.inject))
	for _, g := range own.inject {
		inj = append(inj, proxy.InjectionConfig{InjectionRule: g.Rule, GrantID: g.GrantID})
	}
	return json.Marshal(proxy.Config{
		RunID: run.ID, ControlPlaneURL: s.cfg.ControlPlaneURL, ControlPlaneCAPEM: s.cfg.ControlPlaneCAPEM,
		RunToken: token, ControlPlaneAudience: mcpSandboxAudience, Policy: pol, Injection: inj,
		Listen: ":" + strconv.Itoa(runner.ProxyListenPort), MITMCACertPEM: string(caCert), MITMCAKeyPEM: string(caKey),
		MITMHosts: own.mitm, MCPRelayInbound: &proxy.MCPRelayInboundConfig{Listen: mcpRelayListen, TokenSHA256: relaySHA},
		RelayScrubValues: own.values,
	})
}

// sealMCPProxyConfig seals one MCP sandbox's config for its row, nil on a deployment that keeps no run
// config (runProxyConfigStore).
func (s *Server) sealMCPProxyConfig(runID uuid.UUID, key string, cfgJSON []byte) ([]byte, error) {
	if len(s.cfg.RunConfigKey) != kek.DEKSize {
		return nil, nil
	}
	return kek.Seal(s.cfg.RunConfigKey, cfgJSON, kek.Encode("run", runID.String(), "mcp", key))
}

// mcpBuilt is one sandbox ready to create: its spec, its sealed config and its token's id.
type mcpBuilt struct {
	spec   runner.MCPSandboxSpec
	sealed []byte
	jti    string
}

// buildMCPSandbox is one sandbox's spec: a fresh MCP token and CA, the policy rendered into its proxy config,
// the server's own config and secrets, and the platform's variables (its CA, the trust variables, the
// launch). The spec holds nothing of the agent's.
func (s *Server) buildMCPSandbox(ctx context.Context, run types.AgentRun, image string, srv mcpServer, pol mcpPolicy, own mcpOwn, relaySHA string) (mcpBuilt, error) {
	if srv.def.MCP == nil || srv.def.MCP.Launch == nil {
		return mcpBuilt{}, errors.New("the server has no command")
	}
	tok, err := s.mintMCPSandboxToken(ctx, run)
	if err != nil {
		return mcpBuilt{}, fmt.Errorf("mint the MCP proxy's token: %w", err)
	}
	caCert, caKey, err := generateRunCA(s.cfg.Now())
	if err != nil {
		return mcpBuilt{}, fmt.Errorf("make the MCP proxy's CA: %w", err)
	}
	cfgJSON, err := s.renderMCPProxyConfig(run, pol.spec, tok.Token, caCert, caKey, relaySHA, own)
	if err != nil {
		return mcpBuilt{}, fmt.Errorf("render the MCP proxy's config: %w", err)
	}
	sealed, err := s.sealMCPProxyConfig(run.ID, srv.key, cfgJSON)
	if err != nil {
		return mcpBuilt{}, fmt.Errorf("seal the MCP proxy's config: %w", err)
	}
	platform := map[string]string{"WARDYN_MITM_CA_PEM": string(caCert), "WARDYN_MCP_LAUNCH_JSON": string(mustJSON(srv.def.MCP.Launch))}
	runner.SetSandboxCATrustVars(platform, false)
	return mcpBuilt{sealed: sealed, jti: tok.JTI, spec: runner.MCPSandboxSpec{
		ID: mcpSandboxIDAt(run, srv.ordinal), ServerKey: srv.key, Image: image, Launch: *srv.def.MCP.Launch,
		Env: own.env, SecretEnv: own.secretEnv, ManagedFiles: own.files, PlatformEnv: platform, ProxyConfig: cfgJSON,
		RelayTokenSHA256: relaySHA, Resources: resourceLimitsToRunner(pol.spec.Resources), RelayRank: srv.rank,
	}}, nil
}

// mcpSandboxRef is the sandbox_ref a row records for the run's sandbox at ordinal: the runner names its
// objects by the run and the ordinal (MCPSandboxID), so the ref is those two.
func mcpSandboxRef(run types.AgentRun, ordinal int) string {
	return run.ID.String() + "/mcp/" + strconv.Itoa(ordinal)
}

// mcpSandboxIDAt is the runner's name for the run's sandbox at ordinal.
func mcpSandboxIDAt(run types.AgentRun, ordinal int) runner.MCPSandboxID {
	return runner.MCPSandboxID{RunID: run.ID, Ordinal: ordinal, Class: run.ConfinementClass}
}

// mcpReserved is what a sandbox's row records it reserves.
func mcpReserved(rl *types.ResourceLimits) types.RunnerAmounts {
	if rl == nil {
		return types.RunnerAmounts{}
	}
	return types.RunnerAmounts{CPUMillis: int64(rl.CPUMillis), MemoryMiB: int64(rl.MemoryMiB)}
}

// mcpDispatchRefusal is the dispatch re-check of the gate (P1 item 4): step 2, the switch and the setting (a
// site config read error reads as off), the site's upstream proxy, step 3b, the runner's MCP sandbox kinds
// on the run's own capability read, and step 4, the limit. "" passes.
func (s *Server) mcpDispatchRefusal(ctx context.Context, run types.AgentRun, n int, server string, site types.SiteConfig, siteErr error) (reason, sentence string) {
	facts := mcpGateFacts{stdio: n, off: mcpOwnSandboxOff(site, siteErr), kind: mcpGateKind(run.MachineKind), local: run.Placement == types.PlacementLocal}
	if st := mcpGate(mcpAtSwitch, facts); st != nil {
		return st.reason, mcpSandboxSentence(st.sentence, server, "")
	}
	if site.UpstreamProxyURL != "" || site.UpstreamProxySecretRef != "" {
		return reasonMCPOwnSandboxUnsupported, mcpSandboxSentence(mcpUpstreamProxySentence, server, "")
	}
	caps, err := s.runCapabilities(ctx, run)
	if err != nil {
		return reasonMCPOwnSandboxUnsupported, mcpSandboxSentence(mcpOwnSandboxUnsupportedSentence, server, mcpRunnerUnnamed)
	}
	facts.kinds = caps.MCPSandboxKinds
	if _, ok := s.cfg.Runner.(runner.MCPSandboxer); !ok {
		facts.kinds = nil
	}
	if st := mcpGate(mcpAtFold, facts); st != nil {
		return st.reason, mcpSandboxSentence(st.sentence, server, mcpRunnerUnnamed)
	}
	return "", ""
}

// mcpGateKind is a run's machine kind as the gate compares it with a runner's MCP sandbox kinds: the stored
// "" is a container, as P1's gate reads it.
func mcpGateKind(k types.MachineKind) types.MachineKind {
	if k == "" {
		return types.MachineContainer
	}
	return k
}

// mcpPolicyBaseAt is what dispatch derives each server's MCP policy from: the policy the agent's proxy runs
// under (the sealed policy's sidecar copy, P_run), the layers the seal read, the ceiling's own denies and the
// per-sandbox caps the agent's sandbox got, and, for a server in "own" mode, the creator's ceiling, read once
// from the request the dispatch serves.
func (s *Server) mcpPolicyBaseAt(ctx context.Context, d *mcpSandboxDispatch, pRun types.RunPolicySpec, layers []sealLayer, ceiling dispatchCeiling) (mcpPolicyBase, error) {
	b := mcpPolicyBase{run: pRun, comps: d.comps, layers: layers, deny: ceiling.deny, ungoverned: s.runUngoverned(ctx)}
	b.ceiling.Limits.MaxCPUMillis, b.ceiling.Limits.MaxMemoryMiB = ceiling.maxCPUMillis, ceiling.maxMemoryMiB
	if !b.ungoverned && slices.ContainsFunc(d.servers, func(m mcpServer) bool { return mcpPolicyModeOf(m.def) == types.ComponentMCPSandboxOwn }) {
		c, err := s.effectiveCeiling(ctx)
		if err != nil {
			return mcpPolicyBase{}, err
		}
		b.ceiling = c
	}
	return b, nil
}

// keepFinalWithMCPSandboxes creates the run's MCP sandboxes, whose relay targets the agent's final proxy config
// then holds, and keeps that config (keepFinalRunProxyConfig).
func (s *Server) keepFinalWithMCPSandboxes(ctx, createCtx context.Context, run types.AgentRun, spec *runner.SandboxSpec, d *mcpSandboxDispatch, layers []sealLayer,
	ceiling dispatchCeiling, site types.SiteConfig, siteErr error, stopCreateLease func()) bool {
	return s.createRunMCPSandboxesAt(ctx, createCtx, run, spec, d, layers, ceiling, site, siteErr, stopCreateLease) &&
		s.keepFinalRunProxyConfig(ctx, run, spec.ProxyConfig, stopCreateLease)
}

// createRunMCPSandboxesAt is createRunMCPSandboxes on the base mcpPolicyBaseAt reads: P_run is the agent's
// proxy policy as dispatch rendered it (spec.ProxyConfig.Policy, the sealed policy's sidecar copy), which
// this step reads and never writes.
func (s *Server) createRunMCPSandboxesAt(ctx, createCtx context.Context, run types.AgentRun, spec *runner.SandboxSpec, d *mcpSandboxDispatch, layers []sealLayer, ceiling dispatchCeiling,
	site types.SiteConfig, siteErr error, stopCreateLease func()) bool {
	if d == nil {
		return true
	}
	b, err := s.mcpPolicyBaseAt(ctx, d, spec.ProxyConfig.Policy, layers, ceiling)
	if err != nil {
		stopCreateLease()
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "the ceiling an MCP server's policy is held to could not be read")
		return false
	}
	return s.createRunMCPSandboxes(ctx, createCtx, run, spec, d, b, site, siteErr, stopCreateLease)
}

// createRunMCPSandboxes is dispatch's create step, after the run's masking manifest is complete and before
// its final proxy config is kept: the gate re-checked, then per server a relay token, an MCP token and a CA
// of its own, its policy, its row inserted as starting, then CreateMCPSandbox, which starts nothing, and its
// relay target written into the agent's proxy config. It also refuses an agent spec that holds any of a
// server's secrets. false means the run was failed; what was created is ended by the dispatch's deferred
// endUnstartedMCPSandboxes.
func (s *Server) createRunMCPSandboxes(ctx, createCtx context.Context, run types.AgentRun, spec *runner.SandboxSpec, d *mcpSandboxDispatch, b mcpPolicyBase,
	site types.SiteConfig, siteErr error, stopCreateLease func()) bool {
	if d == nil {
		return true
	}
	fail := func(reason, sentence string) bool {
		stopCreateLease()
		s.failAndRevoke(ctx, run.ID, types.RunStarting, sentence)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create", run.ID.String(), "failure",
			mustJSON(map[string]any{"error": sentence, "reason": reason})))
		return false
	}
	if reason, sentence := s.mcpDispatchRefusal(ctx, run, len(d.servers), d.servers[0].key, site, siteErr); reason != "" {
		return fail(reason, sentence)
	}
	if leak := mcpAgentSpecLeak(*spec, d.servers, d.own); leak != "" {
		return fail(reasonMCPOwnSandboxUnsupported, "an MCP server's secret reached the agent's sandbox ("+leak+"); the run was stopped")
	}
	st, m, _ := s.mcpSandboxSeams()
	for i, srv := range d.servers {
		pol, err := s.mcpSandboxPolicy(ctx, b, srv.index)
		if err != nil {
			return fail(reasonMCPOwnSandboxUnsupported, "the policy of the MCP server "+srv.key+" could not be derived")
		}
		relayTok, relaySHA := mcpRelayToken()
		built, err := s.buildMCPSandbox(ctx, run, spec.Image, srv, pol, d.own[i], relaySHA)
		if err != nil {
			slog.ErrorContext(ctx, "wardynd: building an MCP server's sandbox failed", slog.String("run_id", run.ID.String()), slog.String("server", srv.key), slog.Any("err", err))
			return fail(reasonMCPOwnSandboxUnsupported, "the sandbox of the MCP server "+srv.key+" could not be prepared")
		}
		built.spec.OnWaiting = spec.OnWaiting
		row := store.RunMCPSandbox{RunID: run.ID, Ordinal: srv.ordinal, ServerKey: srv.key, State: store.MCPSandboxStarting,
			PolicyDigest: pol.spec.Digest(), Reserved: mcpReserved(pol.spec.Resources), ProxyConfigSealed: built.sealed,
			RelayTokenSHA256: relaySHA, CPTokenJTI: built.jti, GrantIDs: mcpGrantIDs(d.own[i])}
		if err := st.InsertRunMCPSandbox(ctx, row); err != nil {
			return fail(mcpFailStateWrite, "the MCP server "+srv.key+" could not be recorded")
		}
		sb, err := m.CreateMCPSandbox(createCtx, built.spec)
		if err != nil {
			_ = m.KillMCPSandbox(ctx, built.spec.ID)
			cause := mcpCreateCause(err, mcpFailCreate)
			_, _ = st.FailMCPSandboxIfStarting(ctx, run.ID, srv.ordinal, cause)
			s.auditMCPSandboxCreate(ctx, row, "failure", cause)
			if cause == reasonMCPSandboxHostReachable {
				return fail(cause, mcpSandboxSentence(mcpHostReachableSentence, srv.key, ""))
			}
			return fail(reasonMCPOwnSandboxUnsupported, "the sandbox of the MCP server "+srv.key+" could not be created")
		}
		row.RelayAddr = sb.RelayAddr
		if ok, err := st.RecordMCPSandboxCreated(ctx, run.ID, srv.ordinal, store.MCPSandboxCreated{RelayAddr: sb.RelayAddr,
			PolicyDigest: row.PolicyDigest, ProxyConfigSealed: built.sealed}); err != nil || !ok {
			return fail(mcpFailStateWrite, "the MCP server "+srv.key+" could not be recorded")
		}
		if !setMCPRelay(spec, srv.key, sb.RelayAddr, relayTok) {
			return fail(reasonMCPOwnSandboxUnsupported, "the MCP server "+srv.key+" has no entry in the agent's configuration")
		}
		d.made = append(d.made, mcpMade{srv: srv, row: row, pol: pol})
	}
	return true
}

// mcpGrantIDs is the grants a server's proxy may resolve: its header injections'.
func mcpGrantIDs(own mcpOwn) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(own.inject))
	for _, in := range own.inject {
		ids = append(ids, in.GrantID)
	}
	return ids
}

// setMCPRelay writes server key's relay target into the agent's proxy config; false when the config has no
// entry for it.
func setMCPRelay(spec *runner.SandboxSpec, key, addr, token string) bool {
	for i := range spec.ProxyConfig.MCPServers {
		if e := &spec.ProxyConfig.MCPServers[i]; e.Key == key && e.URL == "" {
			e.Relay = &proxy.MCPRelayTarget{Addr: addr, Token: token}
			return true
		}
	}
	return false
}

// mcpAgentSpecLeak names the first way the agent's spec holds one of a server's own secrets: a secret
// variable's or file's name, or any secret's value, in its env, its secret env or its managed files. ""
// when it holds none (DESIGN §3, I5 c).
func mcpAgentSpecLeak(spec runner.SandboxSpec, servers []mcpServer, own []mcpOwn) string {
	for i := range own {
		for name := range own[i].secretEnv {
			if _, ok := spec.Env[name]; ok {
				return servers[i].key + ": env " + name
			}
			if _, ok := spec.SecretEnv[name]; ok {
				return servers[i].key + ": secret env " + name
			}
		}
		for _, f := range own[i].files {
			if slices.ContainsFunc(spec.ManagedFiles, func(a runner.ManagedFile) bool { return a.Path == f.Path }) {
				return servers[i].key + ": file " + f.Path
			}
		}
		for _, v := range own[i].values {
			if v == "" {
				continue
			}
			in := func(m map[string]string) bool {
				return slices.ContainsFunc(slices.Collect(maps.Values(m)), func(x string) bool { return strings.Contains(x, v) })
			}
			if in(spec.Env) || in(spec.SecretEnv) || slices.ContainsFunc(spec.ManagedFiles, func(a runner.ManagedFile) bool { return bytes.Contains(a.Content, []byte(v)) }) {
				return servers[i].key + ": a secret value"
			}
		}
	}
	return ""
}

// startRunMCPSandboxes is dispatch's start step, after the run's recording starts and before the agent's
// sandbox is created: per server run.mcp_policy.resolve, StartMCPSandbox and run.mcp_sandbox.create, the row
// moved to ready only while the run is STARTING (else what started is killed and the dispatch ends), then
// one wait of up to mcpReadyWait for every server together. A server that does not come up, or does not
// start, is failed alone and killed: the run goes on, and the relay answers its calls -32002 (Q6 A).
func (s *Server) startRunMCPSandboxes(ctx context.Context, run types.AgentRun, d *mcpSandboxDispatch, stopCreateLease func()) bool {
	if d == nil {
		return true
	}
	st, m, _ := s.mcpSandboxSeams()
	var waiting []mcpMade
	for _, made := range d.made {
		s.auditMCPPolicyResolve(ctx, run.ID, made.srv.ordinal, made.srv.key, made.pol)
		id := mcpSandboxIDAt(run, made.srv.ordinal)
		if err := m.StartMCPSandbox(ctx, id); err != nil {
			_ = m.KillMCPSandbox(ctx, id)
			cause := mcpCreateCause(err, mcpFailStart)
			_, _ = st.FailMCPSandboxIfUp(ctx, run.ID, made.srv.ordinal, cause)
			s.auditMCPSandboxCreate(ctx, made.row, "failure", cause)
			continue
		}
		made.row.SandboxRef = mcpSandboxRef(run, made.srv.ordinal)
		if ok, err := st.MarkMCPSandboxStartedIfStarting(ctx, run.ID, made.srv.ordinal, made.row.SandboxRef); err != nil || !ok {
			_ = m.KillMCPSandbox(ctx, id) // the run moved on (a refused admission, a kill): what started goes
			stopCreateLease()
			return false
		}
		s.auditMCPSandboxCreate(ctx, made.row, "success", "")
		waiting = append(waiting, made)
	}
	deadline := s.cfg.Now().Add(mcpReadyWait)
	for len(waiting) > 0 {
		waiting = slices.DeleteFunc(waiting, func(made mcpMade) bool {
			sb, err := m.MCPSandboxStatus(ctx, mcpSandboxIDAt(run, made.srv.ordinal))
			return err == nil && sb.Live
		})
		if len(waiting) == 0 || !s.cfg.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(mcpReadyPoll):
		}
	}
	for _, made := range waiting {
		_ = m.KillMCPSandbox(ctx, mcpSandboxIDAt(run, made.srv.ordinal))
		if ok, _ := st.FailMCPSandboxIfUp(ctx, run.ID, made.srv.ordinal, mcpFailReadiness); ok {
			s.auditMCPSandboxEnd(ctx, run.ID, types.ActorSystem, "wardynd", &made.row, "failure", mcpFailReadiness)
		}
	}
	return true
}

// endUnstartedMCPSandboxes is dispatch's deferred end: a dispatch that returns before the run is RUNNING
// (every return after the creates) ends what it created, so no MCP container outlives it (I8).
func (s *Server) endUnstartedMCPSandboxes(ctx context.Context, run types.AgentRun, started *bool) {
	if !*started {
		s.endRunMCPSandboxes(ctx, run.ID, mcpEndUnstarted)
	}
}
