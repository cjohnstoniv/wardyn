// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Read failures in the queue's judgement, against a real Postgres. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// membersUnreadable is the store with a pool's members unreadable.
type membersUnreadable struct{ store.PG }

func (membersUnreadable) ListRunnerPoolMembers(context.Context, uuid.UUID) ([]types.RunnerPoolMember, error) {
	return nil, errors.New("members unreadable")
}

// A pool whose runners cannot be read is not judged to have no runner free: the read is an error, and
// the run keeps its place with no wait recorded against it.
func TestPG_QueueRunnerFree_AFailedRunnerReadIsAnErrorNotAWait(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	e.srv.cfg.Store = membersUnreadable{e.pg}
	room, err := e.srv.queueRunnerFree(withOIDCHuman(context.Background(), e.personP), pool, queuedLaunch{Request: []byte(runBody(""))})
	if err == nil || room.wait != "" {
		t.Fatalf("queueRunnerFree with the pool's runners unreadable = %+v, %v; want an error and no wait", room, err)
	}
}

// judgingCutShort promotes as the store does, then reports the judging cut short.
type judgingCutShort struct{ store.PG }

func (s judgingCutShort) PromoteRunQueue(ctx context.Context, pass store.PromotePass, admit store.QueueAdmit) ([]store.PromotedRun, []store.QueueHeld, error) {
	promoted, held, err := s.PG.PromoteRunQueue(ctx, pass, admit)
	return promoted, held, errors.Join(err, errors.New("judging cut short"))
}

// A pass whose judging was cut short after it promoted a run still starts that run.
func TestPG_PromoteQueue_APartlyJudgedPassStartsWhatItPromoted(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	waiting, occupant := e.waitingPerson(pool, "")
	e.srv.cfg.Store = judgingCutShort{e.pg}
	e.free(occupant)
	e.srv.promoteQueue(context.Background(), pool.ID, 10*time.Second)
	if got := e.waitStarted(waiting.ID); got.SPIFFEID == "" {
		t.Errorf("the promoted run is %s %q with nothing minted; want it started", got.State, got.StatusDetail)
	}
}
