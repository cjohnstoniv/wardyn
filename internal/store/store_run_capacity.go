// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Caps on the capacity aggregate's two lists.
const (
	capacityOwnerCap         = 50
	capacityUnschedulableCap = 20
)

// RunCapacitySums is one set of configured-reservation sums. Holding counts the rows that
// hold capacity; Unknown counts the holding rows whose reservation was never recorded
// (runner_kind NULL), which add to no sum. A run's held figure is its agent request plus its proxy.
type RunCapacitySums struct {
	Holding               int   `json:"holding"`
	Unknown               int   `json:"unknown"`
	AgentCPURequestMillis int64 `json:"agent_cpu_request_millis"`
	AgentCPULimitMillis   int64 `json:"agent_cpu_limit_millis"`
	AgentMemoryRequestMiB int64 `json:"agent_memory_request_mib"`
	AgentMemoryLimitMiB   int64 `json:"agent_memory_limit_mib"`
	ProxyCPUMillis        int64 `json:"proxy_cpu_millis"`
	ProxyMemoryMiB        int64 `json:"proxy_memory_mib"`
	// ProxyCPUUncapped counts recorded rows whose proxy had no CPU cap (Docker).
	ProxyCPUUncapped int   `json:"proxy_cpu_uncapped"`
	HeldCPUMillis    int64 `json:"held_cpu_millis"`
	HeldMemoryMiB    int64 `json:"held_memory_mib"`
}

// RunCapacityRunner is one runner kind's sums. Basis says what they mean: Kubernetes sums
// requests, Docker has no reservation and reports caps.
type RunCapacityRunner struct {
	Basis string `json:"basis"`
	RunCapacitySums
}

// RunCapacityOwner is one owner's holding runs, summed per runner kind (never across kinds).
// Ordering uses the deployment's current kind.
type RunCapacityOwner struct {
	Owner   string                     `json:"owner"`
	Holding int                        `json:"holding"`
	Runners map[string]RunCapacitySums `json:"by_runner"`
}

// RunCapacityAge is one age bucket (since created_at) of holding runs.
type RunCapacityAge struct {
	Bucket string `json:"bucket"`
	Count  int    `json:"count"`
}

// RunCapacityUnschedulable is one STARTING run waiting for room, with its recorded reservation
// (nil where unrecorded).
type RunCapacityUnschedulable struct {
	ID                    uuid.UUID `json:"id"`
	Owner                 string    `json:"owner"`
	WaitedSeconds         int64     `json:"waited_seconds"`
	Reason                string    `json:"reason"`
	RunnerKind            *string   `json:"runner_kind"`
	AgentCPURequestMillis *int64    `json:"agent_cpu_request_millis"`
	AgentMemoryRequestMiB *int64    `json:"agent_memory_request_mib"`
	ProxyCPUMillis        *int64    `json:"proxy_cpu_millis"`
	ProxyMemoryMiB        *int64    `json:"proxy_memory_mib"`
}

// RunCapacity is the fleet's configured reservations, computed from the non-terminal rows.
type RunCapacity struct {
	States             map[string]int               `json:"states"`
	Paused             int                          `json:"paused"`
	Kept               int                          `json:"kept"`
	Totals             RunCapacityRunner            `json:"totals"`
	AgeBuckets         []RunCapacityAge             `json:"age_buckets"`
	ByRunner           map[string]RunCapacityRunner `json:"by_runner"`
	ByOwner            []RunCapacityOwner           `json:"by_owner"`
	ByOwnerTruncated   bool                         `json:"by_owner_truncated"`
	Unschedulable      []RunCapacityUnschedulable   `json:"unschedulable"`
	UnschedulableTotal int                          `json:"unschedulable_total"`
	// OldestActiveSeconds is the age since created_at of the oldest holding run (0 when none).
	OldestActiveSeconds int64 `json:"oldest_active_seconds"`
}

// RunCapacityOpts parameterises RunCapacity. CurrentKind is the deployment's runner kind; Totals
// carries only that kind's entry; other kinds appear only under ByRunner. UnschedulableReason maps a status_detail to its capacity-blocker
// reason, or "" when it is not one.
type RunCapacityOpts struct {
	Now                 time.Time
	CurrentKind         string
	UnschedulableReason func(detail string) string
}

var capacityAgeBounds = []struct {
	bucket string
	under  time.Duration
}{
	{"under_1h", time.Hour}, {"1h_to_8h", 8 * time.Hour}, {"8h_to_24h", 24 * time.Hour},
	{"1d_to_7d", 7 * 24 * time.Hour}, {"over_7d", 0},
}

func capacityBasis(kind string) string {
	if kind == "k8s" {
		return "requests"
	}
	return "caps"
}

// capacityRow is one holding row's recorded reservation; nil columns are unrecorded.
type capacityRow struct {
	known            bool
	c, cl, m, ml, pm int64
	pc               *int64
}

// add folds one holding row into s. An unrecorded row counts as unknown and adds to no sum.
func (s *RunCapacitySums) add(r capacityRow) {
	s.Holding++
	if !r.known {
		s.Unknown++
		return
	}
	s.AgentCPURequestMillis += r.c
	s.AgentCPULimitMillis += r.cl
	s.AgentMemoryRequestMiB += r.m
	s.AgentMemoryLimitMiB += r.ml
	s.ProxyMemoryMiB += r.pm
	s.HeldCPUMillis += r.c
	s.HeldMemoryMiB += r.m + r.pm
	if r.pc == nil {
		s.ProxyCPUUncapped++
		return
	}
	s.ProxyCPUMillis += *r.pc
	s.HeldCPUMillis += *r.pc
}

func zeroNil(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// RunCapacity reads every non-terminal row in one query (the positive
// types.NonTerminalRunStates list, never NOT IN terminal, so a new terminal state cannot be
// counted as active) and folds it per the state table in the 0.8.6 fleet design: STARTING,
// RUNNING and WAITING_FOR_CONFIRMATION rows with lost_at NULL hold capacity, paused ones
// included; kept rows (lost_at set) count as Kept; PENDING rows appear only in States. A kept
// row whose agent still runs (HoldsSandboxSQL: kept after an outage, before its end) also
// holds its agent's reservation, without the proxy that was stopped.
func (s PG) RunCapacity(ctx context.Context, o RunCapacityOpts) (RunCapacity, error) {
	states := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		states = append(states, string(st))
	}
	rows, err := s.Pool.Query(ctx, `SELECT id, created_by, state, created_at, paused_at, lost_at, status_detail,
		   runner_target, runner_kind, agent_cpu_request_millis, agent_cpu_limit_millis,
		   agent_memory_request_mib, agent_memory_limit_mib, proxy_cpu_millis, proxy_memory_mib,
		   `+HoldsSandboxSQL+`
		 FROM agent_runs WHERE state = ANY($1)`, states)
	if err != nil {
		return RunCapacity{}, fmt.Errorf("store: run capacity: %w", err)
	}
	defer rows.Close()

	out := RunCapacity{
		States:        map[string]int{},
		AgeBuckets:    make([]RunCapacityAge, len(capacityAgeBounds)),
		ByRunner:      map[string]RunCapacityRunner{o.CurrentKind: {Basis: capacityBasis(o.CurrentKind)}},
		ByOwner:       []RunCapacityOwner{},
		Unschedulable: []RunCapacityUnschedulable{},
	}
	for i, b := range capacityAgeBounds {
		out.AgeBuckets[i].Bucket = b.bucket
	}
	owners := map[string]*RunCapacityOwner{}
	type unsched struct {
		RunCapacityUnschedulable
		created time.Time
	}
	var stuck []unsched

	for rows.Next() {
		var (
			id                         uuid.UUID
			owner, state, detail, rtgt string
			created                    time.Time
			paused, lost               *time.Time
			kind                       *string
			c, cl, m, ml, pc, pm       *int64
			holds                      bool
		)
		if err := rows.Scan(&id, &owner, &state, &created, &paused, &lost, &detail, &rtgt, &kind,
			&c, &cl, &m, &ml, &pc, &pm, &holds); err != nil {
			return RunCapacity{}, fmt.Errorf("store: run capacity: scan: %w", err)
		}
		out.States[state]++
		if state == string(types.RunPending) {
			continue
		}
		if lost != nil {
			out.Kept++
			if !holds {
				continue
			}
			pc, pm = new(int64), new(int64) // its proxy was stopped
		}
		if paused != nil {
			out.Paused++
		}
		// A legacy row is attributed to its runner through runner_target.
		k := rtgt
		if kind != nil {
			k = *kind
		}
		row := capacityRow{known: kind != nil, c: zeroNil(c), cl: zeroNil(cl), m: zeroNil(m),
			ml: zeroNil(ml), pm: zeroNil(pm), pc: pc}

		r, ok := out.ByRunner[k]
		if !ok {
			r.Basis = capacityBasis(k)
		}
		r.add(row)
		out.ByRunner[k] = r

		ow := owners[owner]
		if ow == nil {
			ow = &RunCapacityOwner{Owner: owner, Runners: map[string]RunCapacitySums{}}
			owners[owner] = ow
		}
		ow.Holding++
		sums := ow.Runners[k]
		sums.add(row)
		ow.Runners[k] = sums

		age := o.Now.Sub(created)
		bi := len(capacityAgeBounds) - 1
		for i, b := range capacityAgeBounds {
			if b.under > 0 && age < b.under {
				bi = i
				break
			}
		}
		out.AgeBuckets[bi].Count++
		if sec := int64(age / time.Second); sec > out.OldestActiveSeconds {
			out.OldestActiveSeconds = sec
		}

		if state == string(types.RunStarting) && o.UnschedulableReason != nil {
			if reason := o.UnschedulableReason(detail); reason != "" {
				u := unsched{created: created}
				u.ID, u.Owner, u.Reason = id, owner, reason
				u.WaitedSeconds = int64(age / time.Second)
				u.RunnerKind, u.AgentCPURequestMillis, u.AgentMemoryRequestMiB = kind, c, m
				u.ProxyCPUMillis, u.ProxyMemoryMiB = pc, pm
				stuck = append(stuck, u)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return RunCapacity{}, fmt.Errorf("store: run capacity: %w", err)
	}

	// Totals: the current kind's entry only; other kinds appear only under ByRunner.
	out.Totals = out.ByRunner[o.CurrentKind]

	list := make([]RunCapacityOwner, 0, len(owners))
	for _, ow := range owners {
		list = append(list, *ow)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ac, bc := a.Runners[o.CurrentKind].HeldCPUMillis, b.Runners[o.CurrentKind].HeldCPUMillis; ac != bc {
			return ac > bc
		}
		if a.Holding != b.Holding {
			return a.Holding > b.Holding
		}
		return a.Owner < b.Owner
	})
	if len(list) > capacityOwnerCap {
		list, out.ByOwnerTruncated = list[:capacityOwnerCap], true
	}
	out.ByOwner = list

	sort.Slice(stuck, func(i, j int) bool { return stuck[i].created.Before(stuck[j].created) })
	out.UnschedulableTotal = len(stuck)
	for i := 0; i < len(stuck) && i < capacityUnschedulableCap; i++ {
		out.Unschedulable = append(out.Unschedulable, stuck[i].RunCapacityUnschedulable)
	}
	return out, nil
}
