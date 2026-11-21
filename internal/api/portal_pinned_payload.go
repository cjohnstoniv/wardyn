// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pinnedPayload is the part of a portal step's audit event that is fixed with its id and time: who wrote it and
// what it says.
type pinnedPayload struct {
	ActorType types.ActorType `json:"actor_type"`
	Actor     string          `json:"actor"`
	Data      json.RawMessage `json:"data"`
}

// sealedPinned is a pinnedPayload held under its human actor's audit key: the handle of the key generation, which
// says nothing about its owner, and the payload sealed under it.
type sealedPinned struct {
	Handle uuid.UUID `json:"handle"`
	Sealed string    `json:"sealed"`
}

// pinnedEnvelope is what the ledger row holds: the payload itself for a system actor, whose name is no one's, or
// the sealed form for a person.
type pinnedEnvelope struct {
	ActorType types.ActorType `json:"actor_type,omitempty"`
	Actor     string          `json:"actor,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Sealed    *sealedPinned   `json:"sealed,omitempty"`
}

const pinnedAADLabel = "wardyn.deprovision.pin"

// sealPinned is the form of p the ledger row holds. A human actor's payload, which names a person, is sealed under
// that person's audit key, the key person erasure destroys; so a pending payload leaves no readable name behind
// once they are erased, and a database copy is no more readable than the audit table's own sealed rows. Without a key
// service (the audit_personal_fields scope is off then) it is held as it is.
func (s *Server) sealPinned(ctx context.Context, p pinnedPayload) ([]byte, error) {
	if p.ActorType != types.ActorHuman || s.cfg.SubjectKeys == nil {
		return json.Marshal(pinnedEnvelope{ActorType: p.ActorType, Actor: p.Actor, Data: p.Data})
	}
	owner, err := s.pinOwner(ctx, p.Actor)
	if err != nil {
		return nil, fmt.Errorf("seal a pinned audit event: %w", err)
	}
	handle, key, err := s.cfg.SubjectKeys.CurrentHandle(ctx, owner, subjectkey.PurposeAuditSeal)
	if err != nil {
		return nil, fmt.Errorf("seal a pinned audit event: %w", err)
	}
	defer clear(key)
	plain, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	ct, err := kek.Seal(key, plain, kek.Encode(pinnedAADLabel, handle.String()))
	if err != nil {
		return nil, fmt.Errorf("seal a pinned audit event: %w", err)
	}
	return json.Marshal(pinnedEnvelope{Sealed: &sealedPinned{Handle: handle, Sealed: base64.RawURLEncoding.EncodeToString(ct)}})
}

// principalNamer is the directory lookup the audit sealer keys its rows by.
type principalNamer interface {
	PrincipalForName(ctx context.Context, name string) (string, error)
}

// pinOwner is the owner of the audit key a pinned payload of actor is sealed under: the person's principal as the
// audit sealer resolves it (an email form, in any case, or an entra principal names the person it belongs to), so
// person erasure, which destroys the key of the principal and of each name it proved, reaches it. A name that
// resolves to none, or to more than one, is its own owner, as for the sealer.
func (s *Server) pinOwner(ctx context.Context, actor string) (string, error) {
	if n, ok := s.cfg.Store.(principalNamer); ok {
		return n.PrincipalForName(ctx, actor)
	}
	return actor, nil
}

// pinState says what a ledger row's pinned payload is worth to a retry.
type pinState int

const (
	pinAbsent pinState = iota // no payload: the row holds none (a pin made before payloads were held)
	pinOpen                   // the first attempt's payload
	pinErased                 // sealed under a key that was destroyed: the person it names was erased
)

// openPinned is the payload a ledger row holds, and what became of it.
func (s *Server) openPinned(ctx context.Context, raw []byte) (p pinnedPayload, st pinState, err error) {
	if len(raw) == 0 {
		return p, pinAbsent, nil
	}
	var env pinnedEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return p, pinAbsent, fmt.Errorf("read a pinned audit event: %w", err)
	}
	if env.Sealed == nil {
		if env.Actor == "" {
			return p, pinAbsent, nil
		}
		return pinnedPayload{ActorType: env.ActorType, Actor: env.Actor, Data: env.Data}, pinOpen, nil
	}
	if s.cfg.SubjectKeys == nil {
		return p, pinAbsent, errors.New("a pinned audit event is sealed and no key service is configured")
	}
	key, err := s.cfg.SubjectKeys.KeyByHandle(ctx, subjectkey.PurposeAuditSeal, env.Sealed.Handle)
	if errors.Is(err, subjectkey.ErrDataLoss) {
		return p, pinErased, nil
	}
	if err != nil {
		return p, pinAbsent, fmt.Errorf("open a pinned audit event: %w", err)
	}
	defer clear(key)
	ct, err := base64.RawURLEncoding.DecodeString(env.Sealed.Sealed)
	if err != nil {
		return p, pinAbsent, fmt.Errorf("read a pinned audit event: %w", err)
	}
	plain, err := kek.Open(key, ct, kek.Encode(pinnedAADLabel, env.Sealed.Handle.String()))
	if err != nil {
		return p, pinAbsent, fmt.Errorf("open a pinned audit event: %w", err)
	}
	if err := json.Unmarshal(plain, &p); err != nil {
		return p, pinAbsent, fmt.Errorf("read a pinned audit event: %w", err)
	}
	return p, pinOpen, nil
}

// pinnedErasedActor is the actor a replayed event names when the person who first wrote it was erased: no one.
const pinnedErasedActor = "erased"

// errStepSettled is a step another worker finished while this one went to pin its event: nothing is left to write.
var errStepSettled = errors.New("the step was finished by another worker")

// pinStepEvent fixes the audit event a portal step writes on the step's ledger row, the first attempt's winning
// whole: its id, time, actor and data. ev is rewritten to what the row holds, and ctx is marked as a replay, so a
// second writer of the same step, on any replica, writes the same event and adds no row. key names the row the
// caller read and must carry its id; in a bound suspension tail the pin also requires, in the same statement,
// that the row belongs to the suspension the tail is bound to. A row a later suspension replaced is
// store.ErrSuperseded, and nothing is pinned or written. A step another worker finished meanwhile is
// errStepSettled, whether or not it ever pinned an event, and nothing is written to it. The payload is held sealed
// under a human actor's audit key (sealPinned); once that key is erased the replay keeps the pinned id and time,
// is attributed to the system as "erased", and keeps the caller's data. Other origins are untouched.
func (s *Server) pinStepEvent(ctx context.Context, o leaverOrigin, st scimStore, now time.Time, id uuid.UUID, kind string, key store.JobKey, ev *types.AuditEvent) (context.Context, error) {
	if !o.portal {
		return ctx, nil
	}
	pinner, ok := st.(store.PortalEventPinner)
	if !ok {
		return ctx, errors.New("the store cannot pin a portal step's audit event")
	}
	payload, err := s.sealPinned(ctx, pinnedPayload{ActorType: ev.ActorType, Actor: ev.Actor, Data: ev.Data})
	if err != nil {
		return ctx, err
	}
	var bound *time.Time
	if b, in := suspensionBound(ctx); in && kind == store.JobKindSuspend && b.id == id {
		bound = &b.at
	}
	held, err := pinner.PinPortalEvent(ctx, id, kind, key, uuid.New(), now, payload, bound)
	if err != nil {
		return ctx, err
	}
	if held.Done {
		return ctx, errStepSettled
	}
	ev.ID, ev.Time = held.ID, held.At
	p, state, err := s.openPinned(ctx, held.Payload)
	if err != nil {
		return ctx, err
	}
	switch state {
	case pinOpen:
		ev.ActorType, ev.Actor, ev.Data = p.ActorType, p.Actor, p.Data
	case pinErased:
		// The first attempt's actor is gone with their key. The event keeps its id and time, which are the first
		// attempt's, so it is not credited to the retrying caller, who did not act then; their own request has its own
		// receipt at its own time.
		ev.ActorType, ev.Actor = types.ActorSystem, pinnedErasedActor
	default:
		ev.ActorType, ev.Actor = types.ActorSystem, portalLifecycleActor
	}
	return store.WithAuditReplay(ctx), nil
}

// errAuditHeldBySpool is a portal step's audit row that only this replica's local spool holds: not in the shared
// table yet, so the step does not settle on it.
var errAuditHeldBySpool = errors.New("the audit row is held by this replica's spool and is not in the shared audit table yet")

// recordAuditStepEvent writes a step's audit row. A portal step settles in the shared ledger only once the row is
// in the shared audit table: a row the audit spool took is not enough, since the spool is one replica's, and losing
// it would leave a done step with no stored receipt for any other replica to write. A recorder that answers
// audit.ErrSpooled leaves the step pending here; one that answers nil is not trusted either (a sealing recorder can
// append to the spool alone and answer nil), so the step's finish itself requires the pinned id and time in the
// audit table, in the statement that marks it done (store.ErrReceiptNotStored). Either way the retry replays the
// same pinned event, which adds no row once the table has it. Other origins keep recordAuditStrict.
func (s *Server) recordAuditStepEvent(ctx context.Context, o leaverOrigin, ev types.AuditEvent) error {
	if !o.portal {
		return s.recordAuditStrict(ctx, ev)
	}
	if s.cfg.Audit == nil {
		return nil
	}
	err := s.cfg.Audit.Record(ctx, ev)
	if errors.Is(err, audit.ErrSpooled) {
		return errAuditHeldBySpool
	}
	return err
}
