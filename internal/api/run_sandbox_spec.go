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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run's recorded sandbox spec (run_sandbox_specs): the agent half of the spec dispatch created the run's sandbox
// with, recorded once, on the final spec, so a move can create the sandbox again at a destination. Dispatch builds
// the spec from values that exist only while it runs, and a revive restarts a kept sandbox without one, so this
// record is the only copy.
//
// It never holds a credential. The proxy config, the secret environment and every managed file delivered as a
// secret (AgentOwned) stay out, and so does Claude Code's MCP file when it carries a per-run MCP token. Two flags
// say what the record could not carry: Resident, when dispatch placed a credential the sandbox holds, and MCPToken,
// when it minted a per-run MCP token. A run with either is never moved (moveRefusal). The record is sealed under
// the run config key, bound to its run, as the run's proxy config is, because an organisation component's config
// in Env may itself be a value the organisation never meant to send. It is deleted with the run's proxy config when
// the run goes terminal, and by the same purge behind that delete (store.PG.DeleteRunProxyConfig).

// runSandboxSpecAADLabel is the domain label of the record's associated data, so a sealed record opens only as the
// record of the run it was sealed for.
const runSandboxSpecAADLabel = "wardyn/run-sandbox-spec/v1"

// errRunSandboxSpecNotKept: this deployment keeps no sandbox spec (no key, or a store without the table).
var errRunSandboxSpecNotKept = errors.New("this deployment does not keep run sandbox specs")

// runSandboxSpecRecord is the recorded agent half of a run's sandbox spec.
type runSandboxSpecRecord struct {
	Image            string                      `json:"image"`
	ImageArches      []string                    `json:"image_arches,omitempty"`
	ConfinementClass types.ConfinementClass      `json:"confinement_class"`
	MachineKind      types.MachineKind           `json:"machine_kind,omitempty"`
	Env              map[string]string           `json:"env,omitempty"`
	Mounts           []runner.Mount              `json:"mounts,omitempty"`
	Drives           []types.DriveMount          `json:"drives,omitempty"`
	ManagedFiles     []runner.ManagedFile        `json:"managed_files,omitempty"`
	UserMountRoots   []string                    `json:"user_mount_roots,omitempty"`
	UserWritable     *runner.UserWritableCeiling `json:"user_writable,omitempty"`
	Interactive      bool                        `json:"interactive"`
	Resources        runner.Resources            `json:"resources"`
	OrgConfigKeys    []string                    `json:"org_config_keys,omitempty"`
	AgentPolicy      runSandboxSpecAgentPolicy   `json:"agent_policy"`
	Resident         bool                        `json:"resident"`
	MCPToken         bool                        `json:"mcp_token"`
}

// runSandboxSpecAgentPolicy is what dispatch decided about Claude Code's managed settings, without the document:
// where it went, its size, whether the run is on the hold lane, whether it was the locked document, and how many
// tool rules it wrote.
type runSandboxSpecAgentPolicy struct {
	Path      string `json:"path,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	Hold      bool   `json:"hold,omitempty"`
	Locked    bool   `json:"locked,omitempty"`
	ToolRules int    `json:"tool_rules,omitempty"`
}

// moveRefusal is the one rule that keeps a recorded run from moving: a credential the sandbox holds (reason
// move_resident_credential, layer resident), else a per-run MCP token (move_mcp_token, layer mcp), else nothing.
func (rec runSandboxSpecRecord) moveRefusal() (reason, layer string) {
	switch {
	case rec.Resident:
		return reasonMoveResidentCredential, "resident"
	case rec.MCPToken:
		return reasonMoveMCPToken, "mcp"
	}
	return "", ""
}

// runSandboxSpecFor is the record of spec, the final spec dispatch creates the run's sandbox with. resident is the
// run's runner_resident values, orgConfigKeys the Env keys an organisation component's config set, and ap what
// dispatch decided about the managed settings.
func runSandboxSpecFor(spec runner.SandboxSpec, resident []residentValue, orgConfigKeys []string, ap runAgentPolicy) runSandboxSpecRecord {
	rec := runSandboxSpecRecord{
		Image: spec.Image, ImageArches: spec.ImageArches, ConfinementClass: spec.ConfinementClass, MachineKind: spec.MachineKind,
		Env: spec.Env, Mounts: spec.Mounts, Drives: spec.Drives, UserMountRoots: spec.UserMountRoots,
		UserWritable: spec.UserWritable, Interactive: spec.Interactive, Resources: spec.Resources,
		OrgConfigKeys: slices.Clone(orgConfigKeys),
		AgentPolicy:   runSandboxSpecAgentPolicy{Path: ap.path, Bytes: ap.bytes, Hold: ap.hold, Locked: ap.locked, ToolRules: ap.toolRules},
		// A binding exists only where dispatch minted a harness's MCP token.
		MCPToken: len(spec.ProxyConfig.MCPBindings) > 0,
		Resident: len(resident) > 0,
	}
	for k := range spec.SecretEnv {
		// Codex's MCP map is the one secret key that is not a resident credential: its token is MCPToken's.
		rec.Resident = rec.Resident || k != codexMCPEnv
	}
	for _, f := range spec.ManagedFiles {
		switch {
		case f.AgentOwned:
			rec.Resident = true
		case rec.MCPToken && (f.Path == claudeMCPAutonomousPath || f.Path == claudeMCPInteractivePath):
			// The file holds the token's URLs, so it stays out with the token.
		default:
			rec.ManagedFiles = append(rec.ManagedFiles, f)
		}
	}
	return rec
}

func runSandboxSpecAAD(runID uuid.UUID) []byte {
	return kek.Encode(runSandboxSpecAADLabel, runID.String())
}

func (s *Server) runSandboxSpecStore() (store.RunSandboxSpecs, bool) {
	st, ok := s.cfg.Store.(store.RunSandboxSpecs)
	return st, ok && len(s.cfg.RunConfigKey) == kek.DEKSize
}

// keepRunSandboxSpec records the run's sandbox spec, sealed. A move is optional, so a record that cannot be written
// is logged and the run is dispatched: it then has no record, and a move of it is refused move_spec_unrecorded.
func (s *Server) keepRunSandboxSpec(ctx context.Context, runID uuid.UUID, spec runner.SandboxSpec, resident []residentValue,
	orgConfigKeys []string, ap runAgentPolicy) {
	if err := s.storeRunSandboxSpec(ctx, runID, runSandboxSpecFor(spec, resident, orgConfigKeys, ap)); err != nil {
		slog.WarnContext(ctx, "wardynd: the run's sandbox spec was not recorded; it cannot be moved",
			slog.String("run_id", runID.String()), slog.String("err", err.Error()))
	}
}

func (s *Server) storeRunSandboxSpec(ctx context.Context, runID uuid.UUID, rec runSandboxSpecRecord) error {
	st, ok := s.runSandboxSpecStore()
	if !ok {
		return nil // as the run's proxy config: a deployment that keeps none records nothing, and its runs never move
	}
	plain, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode sandbox spec: %w", err)
	}
	sealed, err := kek.Seal(s.cfg.RunConfigKey, plain, runSandboxSpecAAD(runID))
	if err != nil {
		return fmt.Errorf("seal sandbox spec: %w", err)
	}
	return st.PutRunSandboxSpec(ctx, runID, sealed)
}

// loadRunSandboxSpec reads and opens runID's record: store.ErrNotFound when it has none, errRunSandboxSpecNotKept
// when this deployment keeps none.
func (s *Server) loadRunSandboxSpec(ctx context.Context, runID uuid.UUID) (runSandboxSpecRecord, error) {
	st, ok := s.runSandboxSpecStore()
	if !ok {
		return runSandboxSpecRecord{}, errRunSandboxSpecNotKept
	}
	sealed, err := st.GetRunSandboxSpec(ctx, runID)
	if err != nil {
		return runSandboxSpecRecord{}, err
	}
	plain, err := kek.Open(s.cfg.RunConfigKey, sealed, runSandboxSpecAAD(runID))
	if err != nil {
		return runSandboxSpecRecord{}, fmt.Errorf("open sandbox spec: %w", err)
	}
	var rec runSandboxSpecRecord
	if err := json.Unmarshal(plain, &rec); err != nil {
		return runSandboxSpecRecord{}, fmt.Errorf("decode sandbox spec: %w", err)
	}
	return rec, nil
}
