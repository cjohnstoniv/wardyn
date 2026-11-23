// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunPolicyEnvelope is what runEffectivePolicy reads for one run, oldest first: its generation state, its newest
// resolve rows, and, when the current generation is 2 or more, that generation's newest rows.
type RunPolicyEnvelope struct {
	State      types.RunPolicyState
	Resolve    []types.AuditEvent // at most 5, under any of resolveActions
	Generation []types.AuditEvent // at most 5 run.policy.generation rows whose data holds {"generation": N}; nil when N < 2
}

// RunPolicyEnvelopeReader reads a page of runs' envelopes in three statements, whatever the page size: the
// runs' policy state, their resolve rows, and the generation rows of the runs a revive moved past generation 1.
type RunPolicyEnvelopeReader interface {
	RunPolicyEnvelopes(ctx context.Context, runIDs []uuid.UUID, resolveActions []string) (map[uuid.UUID]RunPolicyEnvelope, error)
}

var _ RunPolicyEnvelopeReader = PG{}

// runPolicyEnvelopeRows is how many of a run's newest rows each of the two row reads keeps, as the
// effective-policy reader keeps them.
const runPolicyEnvelopeRows = 5

// runPolicyEnvelopeGenerationAction is the row a revive records its generation's policy under.
const runPolicyEnvelopeGenerationAction = "run.policy.generation"

// RunPolicyEnvelopes — see RunPolicyEnvelopeReader. A run the table does not hold is absent from the map, and
// so is a run whose policy state does not decode (logged): its reader would fail the same way.
func (s PG) RunPolicyEnvelopes(ctx context.Context, runIDs []uuid.UUID, resolveActions []string) (map[uuid.UUID]RunPolicyEnvelope, error) {
	out := map[uuid.UUID]RunPolicyEnvelope{}
	if len(runIDs) == 0 {
		return out, nil
	}
	states, err := collect(ctx, s.Pool, "read", "run policy states",
		`SELECT id, policy_digest, policy_generations, COALESCE(token_jti, ''), COALESCE(token_prev_jti, '')
		FROM agent_runs WHERE id = ANY($1)`, []any{runIDs}, runPolicyEnvelopeScanState)
	if err != nil {
		return nil, err
	}
	var genRuns []uuid.UUID
	var gens []int32
	for _, st := range states {
		if st.err != nil {
			slog.WarnContext(ctx, "wardyn: a run's policy state does not decode; its envelope is left out",
				slog.String("run_id", st.id.String()), slog.Any("err", st.err))
			continue
		}
		out[st.id] = RunPolicyEnvelope{State: st.state}
		if n := st.state.CurrentGeneration(); n >= 2 {
			genRuns = append(genRuns, st.id)
			gens = append(gens, int32(n))
		}
	}
	resolve, err := collect(ctx, s.Pool, "query", "run policy resolve rows",
		`SELECT `+auditCols+` FROM (
			SELECT `+auditCols+`, seq, ROW_NUMBER() OVER (PARTITION BY run_id ORDER BY seq DESC) AS rn
			FROM audit_events WHERE run_id = ANY($1) AND action = ANY($2)
		) t WHERE rn <= $3 ORDER BY run_id, seq`,
		[]any{runIDs, resolveActions, runPolicyEnvelopeRows}, scanAuditEvent)
	if err != nil {
		return nil, err
	}
	for _, ev := range resolve {
		if env, ok := runPolicyEnvelopeOf(out, ev); ok {
			env.Resolve = append(env.Resolve, ev)
			out[*ev.RunID] = env
		}
	}
	if len(genRuns) == 0 {
		return out, nil
	}
	generation, err := collect(ctx, s.Pool, "query", "run policy generation rows",
		`SELECT `+auditCols+` FROM (
			SELECT `+auditCols+`, seq, ROW_NUMBER() OVER (PARTITION BY run_id ORDER BY seq DESC) AS rn
			FROM unnest($1::uuid[], $2::int[]) AS p(rid, gen)
			JOIN audit_events ON run_id = p.rid AND action = $3 AND data @> jsonb_build_object('generation', p.gen)
		) t WHERE rn <= $4 ORDER BY run_id, seq`,
		[]any{genRuns, gens, runPolicyEnvelopeGenerationAction, runPolicyEnvelopeRows}, scanAuditEvent)
	if err != nil {
		return nil, err
	}
	for _, ev := range generation {
		if env, ok := runPolicyEnvelopeOf(out, ev); ok {
			env.Generation = append(env.Generation, ev)
			out[*ev.RunID] = env
		}
	}
	return out, nil
}

// runPolicyEnvelopeOf is the envelope ev's run already has in out.
func runPolicyEnvelopeOf(out map[uuid.UUID]RunPolicyEnvelope, ev types.AuditEvent) (RunPolicyEnvelope, bool) {
	if ev.RunID == nil {
		return RunPolicyEnvelope{}, false
	}
	env, ok := out[*ev.RunID]
	return env, ok
}

// runPolicyEnvelopeState is one run's policy state row, decoded as RunPolicyStateOf decodes it; err is set
// when its generations do not decode.
type runPolicyEnvelopeState struct {
	id    uuid.UUID
	state types.RunPolicyState
	err   error
}

func runPolicyEnvelopeScanState(row pgx.Row) (runPolicyEnvelopeState, error) {
	var out runPolicyEnvelopeState
	var gens []byte
	var jti, prev string
	if err := row.Scan(&out.id, &out.state.Digest, &gens, &jti, &prev); err != nil {
		return out, err
	}
	if err := json.Unmarshal(gens, &out.state.Generations); err != nil {
		out.err = fmt.Errorf("store: read run policy state: %w", err)
		return out, nil
	}
	for _, id := range []string{jti, prev} {
		if id != "" {
			out.state.LiveTokenIDs = append(out.state.LiveTokenIDs, id)
		}
	}
	return out, nil
}
