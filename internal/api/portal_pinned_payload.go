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
	handle, key, err := s.cfg.SubjectKeys.CurrentHandle(ctx, p.Actor, subjectkey.PurposeAuditSeal)
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

// openPinned is the payload a ledger row holds. ok is false when there is none to use: the row has none, or its
// key was destroyed (the person it names was erased), and the caller then keeps its own.
func (s *Server) openPinned(ctx context.Context, raw []byte) (p pinnedPayload, ok bool, err error) {
	if len(raw) == 0 {
		return p, false, nil
	}
	var env pinnedEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return p, false, fmt.Errorf("read a pinned audit event: %w", err)
	}
	if env.Sealed == nil {
		return pinnedPayload{ActorType: env.ActorType, Actor: env.Actor, Data: env.Data}, env.Actor != "", nil
	}
	if s.cfg.SubjectKeys == nil {
		return p, false, errors.New("a pinned audit event is sealed and no key service is configured")
	}
	key, err := s.cfg.SubjectKeys.KeyByHandle(ctx, subjectkey.PurposeAuditSeal, env.Sealed.Handle)
	if errors.Is(err, subjectkey.ErrDataLoss) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("open a pinned audit event: %w", err)
	}
	defer clear(key)
	ct, err := base64.RawURLEncoding.DecodeString(env.Sealed.Sealed)
	if err != nil {
		return p, false, fmt.Errorf("read a pinned audit event: %w", err)
	}
	plain, err := kek.Open(key, ct, kek.Encode(pinnedAADLabel, env.Sealed.Handle.String()))
	if err != nil {
		return p, false, fmt.Errorf("open a pinned audit event: %w", err)
	}
	if err := json.Unmarshal(plain, &p); err != nil {
		return p, false, fmt.Errorf("read a pinned audit event: %w", err)
	}
	return p, true, nil
}
