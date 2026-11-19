// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "slices"

// Policy generations. Dispatch records a run's policy once (generation 1, agent_runs.policy_digest). A
// revive rebuilds the run's proxy under the owner's current limits, so the run then enforces a new
// policy: generation 2, and so on. Each generation keeps the run tokens its proxy held, so a row the
// proxy sent is stamped with the generation of the token it presented, even when it arrives after the
// next revive. The token is the carrier: the proxy's config gains no key, so a proxy of any release
// starts as before.

// PolicyDigestUnknown is the policy_digest of a row from a run's proxy that no token ties to one of the
// run's policies, once the run has had more than one. It is never the first policy's digest.
const PolicyDigestUnknown = "unknown"

// PolicyGeneration is one policy a run's proxy enforced, as agent_runs.policy_generations stores it.
type PolicyGeneration struct {
	Generation int    `json:"generation"`
	Digest     string `json:"digest"`
	// TokenIDs are the ids of the run tokens this generation's proxy held: the one it started with, and
	// the last renewal's two when the next revive cut its lineage.
	TokenIDs []string `json:"token_ids,omitempty"`
	// Abandoned is a revive that never replaced the old proxy: the generation before it is current again.
	Abandoned bool `json:"abandoned,omitempty"`
}

// RunPolicyState is what a run's row holds about its policies.
type RunPolicyState struct {
	// Digest is the current generation's digest (agent_runs.policy_digest).
	Digest string
	// Generations is empty until the first revive, which records generation 1 beside its own.
	Generations []PolicyGeneration
	// LiveTokenIDs are the current lineage's last renewal, its token and the one it presented.
	LiveTokenIDs []string
}

// PolicyStamp is what one audit row is stamped with. The zero value stamps nothing.
type PolicyStamp struct {
	Digest     string
	Generation int
}

// current is the stamp of the newest generation not abandoned, or generation 1 before any revive.
func (s RunPolicyState) current() PolicyStamp {
	for i := len(s.Generations) - 1; i >= 0; i-- {
		if g := s.Generations[i]; !g.Abandoned {
			return g.stamp()
		}
	}
	return PolicyGeneration{Generation: 1, Digest: s.Digest}.stamp()
}

// CurrentGeneration is the number of the generation the run's proxy enforces: the newest one not
// abandoned, or 1 before any revive and once every revive's generation was abandoned. Unlike current,
// it keeps the number when generation 1 has no digest.
func (s RunPolicyState) CurrentGeneration() int {
	for i := len(s.Generations) - 1; i >= 0; i-- {
		if g := s.Generations[i]; !g.Abandoned {
			return g.Generation
		}
	}
	return 1
}

func (g PolicyGeneration) stamp() PolicyStamp {
	if g.Digest == "" {
		return PolicyStamp{} // dispatched before the digest was recorded
	}
	return PolicyStamp{Digest: g.Digest, Generation: g.Generation}
}

// Stamp returns the stamp for one of the run's rows. fromProxy says the run's proxy sent it, under the
// run token whose id is token ("" when unknown); a row the control plane writes itself is under the
// current generation. A run with one policy needs no token. Once it has had more, a proxy row is the
// generation its token belongs to, the current one for the live lineage, and unknown otherwise.
func (s RunPolicyState) Stamp(fromProxy bool, token string) PolicyStamp {
	if !fromProxy || len(s.Generations) < 2 {
		return s.current()
	}
	if token != "" {
		for _, g := range s.Generations {
			if slices.Contains(g.TokenIDs, token) {
				return g.stamp()
			}
		}
		if slices.Contains(s.LiveTokenIDs, token) {
			return s.current()
		}
	}
	return PolicyStamp{Digest: PolicyDigestUnknown}
}

// Advance returns the state after a revive starts the next generation under digest, its proxy on the
// fresh token. retired are the outgoing proxy's tokens, kept on the generation it enforced. The number
// is one past the last generation, abandoned ones included, so no number names two policies.
func (s RunPolicyState) Advance(digest string, retired []string, fresh string) (RunPolicyState, int) {
	gens := make([]PolicyGeneration, 0, len(s.Generations)+2)
	for _, g := range s.Generations {
		g.TokenIDs = slices.Clone(g.TokenIDs)
		gens = append(gens, g)
	}
	if len(gens) == 0 {
		gens = append(gens, PolicyGeneration{Generation: 1, Digest: s.Digest})
	}
	out := len(gens) - 1
	for out > 0 && gens[out].Abandoned {
		out--
	}
	for _, id := range retired {
		if id != "" && !slices.Contains(gens[out].TokenIDs, id) {
			gens[out].TokenIDs = append(gens[out].TokenIDs, id)
		}
	}
	next := PolicyGeneration{Generation: gens[len(gens)-1].Generation + 1, Digest: digest}
	if fresh != "" {
		next.TokenIDs = []string{fresh}
	}
	gens = append(gens, next)
	return RunPolicyState{Digest: digest, Generations: gens, LiveTokenIDs: s.LiveTokenIDs}, next.Generation
}

// Abandon returns the state after the revive that started generation gen left the old proxy running:
// gen is marked abandoned and the generation before it is current again. Any other gen changes nothing.
func (s RunPolicyState) Abandon(gen int) RunPolicyState {
	n := len(s.Generations)
	if n == 0 || s.Generations[n-1].Generation != gen {
		return s
	}
	gens := slices.Clone(s.Generations)
	gens[n-1].Abandoned = true
	out := RunPolicyState{Generations: gens, LiveTokenIDs: s.LiveTokenIDs}
	out.Digest = out.current().Digest
	return out
}
