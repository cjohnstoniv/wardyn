// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
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
