// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A sealed field is a JSON string in place of the value it hides:
//
//	seal1.<key version>.<subject>.<ciphertext>      under the subject's own key
//	seal1p.<subject>.<ciphertext>                   under the platform pending key
//
// subject and ciphertext are unpadded base64url. The ciphertext is kek.Seal of
// the original JSON value; its additional data binds the event id, the action,
// the field's path, the subject and the key version, so a ciphertext moved to
// another event, field or person fails to open. The row hash covers the
// string, so the chain verifies whether or not the key is still there.
const (
	sealedPrefix  = "seal1."
	pendingPrefix = "seal1p."
	aadLabel      = "wardyn/audit-seal/v1"

	// PendingMarker is the data key a row carries while any of its fields waits
	// under the pending key. Such a row lives only in the spool: the drain
	// re-seals it before the store sees it.
	PendingMarker = "pending_subject"

	// ErasedValue is what a reader shows for a field whose subject key was
	// destroyed.
	ErasedValue = "[erased]"
)

// ErrKeyErased is what a SealKeys returns, wrapped, for a key that was
// destroyed (or whose row is gone): what it sealed is unrecoverable.
var ErrKeyErased = errors.New("audit seal: the subject's key is destroyed")

// ErrReplayDeferred is what the spool drain's recorder returns for a pending
// row it cannot re-seal yet. The drain keeps the line and retries; it is never
// counted toward quarantine, because the line is not what is wrong.
var ErrReplayDeferred = errors.New("audit replay deferred: a pending row cannot be re-sealed yet")

// SealKeys is the per-subject key service; *subjectkey.Manager (through
// cmd/wardynd's adapter) is the production one. Current may create the
// subject's first key; Key reads a version and fails ErrKeyErased once it is
// destroyed.
type SealKeys interface {
	Current(ctx context.Context, owner, purpose string) (version int, key []byte, err error)
	Key(ctx context.Context, owner, purpose string, version int) ([]byte, error)
}

// Unsealer opens the sealed fields of rows being read.
type Unsealer interface {
	Unseal(ctx context.Context, evs []types.AuditEvent) ([]types.AuditEvent, error)
}

var _ Unsealer = (*Sealer)(nil)

// Sealer seals the personal fields of audit rows under their subjects' keys.
type Sealer struct {
	// Keys serves the subjects' keys.
	Keys SealKeys
	// Pending returns the platform pending key (32 bytes), nil when there is
	// none. It seals a field whose subject key could not be had, so the row can
	// wait in the spool instead of being dropped or written in the clear.
	Pending func() []byte
	// Resolve maps a name an event carries (an email form, an entra: principal)
	// to the person's principal, the key every erasure destroys by. Nil keeps
	// the name as it is.
	Resolve func(ctx context.Context, name string) (string, error)
	// GoneSince reports whether subject's seal key was destroyed after since. A
	// pending field that waited past its subject's erasure is then erased on
	// re-seal instead of sealed under a fresh key. Nil means never.
	GoneSince func(ctx context.Context, subject string, since time.Time) (bool, error)
}

func sealAAD(eventID, action, path, subject string, version int) []byte {
	return kek.Encode(aadLabel, eventID, action, path, subject, strconv.Itoa(version))
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// subjectOf is the name ev's field of kind k describes, "" when no person does.
func subjectOf(ev types.AuditEvent, k Subject) string {
	switch k {
	case SubjectActor:
		if ev.ActorType == types.ActorHuman {
			return ev.Actor
		}
	case SubjectTarget:
		return ev.Target
	}
	return ""
}

// emptyValue is a field with nothing to hide.
func emptyValue(v json.RawMessage) bool {
	t := bytes.TrimSpace(v)
	return len(t) == 0 || bytes.Equal(t, []byte("null")) || bytes.Equal(t, []byte(`""`))
}

// rewrite replaces the value at path (dotted) in the JSON object obj with
// fn's result. It reports whether the path was there and fn changed it; obj is
// returned untouched when not.
func rewrite(obj json.RawMessage, path []string, fn func(json.RawMessage) (json.RawMessage, bool, error)) (json.RawMessage, bool, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(obj, &m); err != nil || m == nil {
		return obj, false, nil // not an object: nothing to find
	}
	v, ok := m[path[0]]
	if !ok {
		return obj, false, nil
	}
	var nv json.RawMessage
	var changed bool
	var err error
	if len(path) == 1 {
		nv, changed, err = fn(v)
	} else {
		nv, changed, err = rewrite(v, path[1:], fn)
	}
	if err != nil || !changed {
		return obj, false, err
	}
	m[path[0]] = nv
	out, err := json.Marshal(m)
	return out, err == nil, err
}

// Seal returns ev with the fields sealFields lists for its action sealed. A
// field whose subject key could not be had is sealed under the pending key and
// the row carries PendingMarker: pending is then true, and the row must go to
// the spool, never to the store. No path writes a personal field in the clear;
// when even the pending key is missing, Seal fails.
func (s *Sealer) Seal(ctx context.Context, ev types.AuditEvent) (out types.AuditEvent, pending bool, err error) {
	fields := sealFields[ev.Action]
	if len(fields) == 0 || len(ev.Data) == 0 {
		return ev, false, nil
	}
	data := ev.Data
	for _, f := range fields {
		name := subjectOf(ev, f.Subject)
		if name == "" {
			continue
		}
		var fieldPending bool
		var ferr error
		data, _, ferr = rewrite(data, strings.Split(f.Path, "."), func(v json.RawMessage) (json.RawMessage, bool, error) {
			if emptyValue(v) {
				return v, false, nil
			}
			sealed, p, err := s.sealValue(ctx, ev, f.Path, name, v)
			fieldPending = fieldPending || p
			return sealed, err == nil, err
		})
		if ferr != nil {
			return ev, false, ferr
		}
		pending = pending || fieldPending
	}
	if pending {
		data, err = withPendingMarker(data, true)
		if err != nil {
			return ev, false, err
		}
	}
	ev.Data = data
	return ev, pending, nil
}

// sealValue seals one value under name's key, or under the pending key when
// that key cannot be had.
func (s *Sealer) sealValue(ctx context.Context, ev types.AuditEvent, path, name string, v json.RawMessage) (json.RawMessage, bool, error) {
	subject, version, key, kerr := s.subjectKey(ctx, name)
	if kerr == nil {
		defer clear(key)
		ct, err := kek.Seal(key, v, sealAAD(ev.ID.String(), ev.Action, path, subject, version))
		if err != nil {
			return nil, false, fmt.Errorf("audit seal: %w", err)
		}
		return jsonString(sealedPrefix + strconv.Itoa(version) + "." + b64([]byte(subject)) + "." + b64(ct)), false, nil
	}
	pk := s.pendingKey()
	if pk == nil {
		return nil, false, fmt.Errorf("audit seal: no key for the subject and no pending key to hold the row: %w", kerr)
	}
	ct, err := kek.Seal(pk, v, sealAAD(ev.ID.String(), ev.Action, path, name, 0))
	if err != nil {
		return nil, false, fmt.Errorf("audit seal: %w", err)
	}
	return jsonString(pendingPrefix + b64([]byte(name)) + "." + b64(ct)), true, nil
}

// subjectKey resolves name to its person and returns that person's current key.
func (s *Sealer) subjectKey(ctx context.Context, name string) (subject string, version int, key []byte, err error) {
	subject = name
	if s.Resolve != nil {
		if subject, err = s.Resolve(ctx, name); err != nil {
			return "", 0, nil, err
		}
	}
	version, key, err = s.Keys.Current(ctx, subject, SealPurpose)
	return subject, version, key, err
}

func (s *Sealer) pendingKey() []byte {
	if s.Pending == nil {
		return nil
	}
	if k := s.Pending(); len(k) == kek.DEKSize {
		return k
	}
	return nil
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// withPendingMarker sets or removes PendingMarker in the data object.
func withPendingMarker(data json.RawMessage, on bool) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("audit seal: data is not an object: %w", err)
	}
	if on {
		m[PendingMarker] = json.RawMessage("true")
	} else {
		delete(m, PendingMarker)
	}
	return json.Marshal(m)
}

// IsPending reports whether ev waits under the pending key.
func IsPending(ev types.AuditEvent) bool {
	return len(ev.Data) > 0 && bytes.Contains(ev.Data, []byte(`"`+PendingMarker+`"`)) && bytes.Contains(ev.Data, []byte(`"`+pendingPrefix))
}

// parseSealed splits a sealed string. ok is false for any other string.
func parseSealed(s string) (version int, subject string, ct []byte, ok bool) {
	rest, found := strings.CutPrefix(s, sealedPrefix)
	if !found {
		return 0, "", nil, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 3 {
		return 0, "", nil, false
	}
	v, err := strconv.Atoi(parts[0])
	if err != nil || v < 1 {
		return 0, "", nil, false
	}
	sub, err1 := base64.RawURLEncoding.DecodeString(parts[1])
	c, err2 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(sub) == 0 {
		return 0, "", nil, false
	}
	return v, string(sub), c, true
}

func parsePending(s string) (subject string, ct []byte, ok bool) {
	rest, found := strings.CutPrefix(s, pendingPrefix)
	if !found {
		return "", nil, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 2 {
		return "", nil, false
	}
	sub, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	c, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	if err1 != nil || err2 != nil || len(sub) == 0 {
		return "", nil, false
	}
	return string(sub), c, true
}

func stringValue(v json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", false
	}
	return s, true
}

// keyMemo holds the keys one read opened, so a page of rows asks for each
// (subject, version) once, and clears them when the read is done.
type keyMemo struct {
	keys   map[string][]byte
	erased map[string]bool
}

func (m *keyMemo) clear() {
	for _, k := range m.keys {
		clear(k)
	}
}

func (s *Sealer) memoKey(ctx context.Context, m *keyMemo, subject string, version int) ([]byte, error) {
	id := subject + "\x00" + strconv.Itoa(version)
	if m.erased[id] {
		return nil, ErrKeyErased
	}
	if k, ok := m.keys[id]; ok {
		return k, nil
	}
	k, err := s.Keys.Key(ctx, subject, SealPurpose, version)
	if errors.Is(err, ErrKeyErased) {
		m.erased[id] = true
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	m.keys[id] = k
	return k, nil
}

// Unseal returns evs with every sealed field opened. A field whose key was
// destroyed reads ErasedValue. A key that cannot be had for another reason (the
// key store is down) fails the whole read, so a reader never mistakes
// ciphertext for the record. A string that does not open under its own
// additional data is left as it is: it is not a seal Wardyn wrote for this row.
func (s *Sealer) Unseal(ctx context.Context, evs []types.AuditEvent) ([]types.AuditEvent, error) {
	var memo *keyMemo
	defer func() {
		if memo != nil {
			memo.clear()
		}
	}()
	var out []types.AuditEvent // copy-on-write: the caller's slice is not touched
	for i := range evs {
		ev := evs[i]
		fields := sealFields[ev.Action]
		if len(fields) == 0 || !bytes.Contains(ev.Data, []byte(`"`+sealedPrefix)) {
			continue
		}
		if memo == nil {
			memo = &keyMemo{keys: map[string][]byte{}, erased: map[string]bool{}}
		}
		data := ev.Data
		for _, f := range fields {
			var err error
			data, _, err = rewrite(data, strings.Split(f.Path, "."), func(v json.RawMessage) (json.RawMessage, bool, error) {
				str, ok := stringValue(v)
				if !ok {
					return v, false, nil
				}
				version, subject, ct, ok := parseSealed(str)
				if !ok {
					return v, false, nil
				}
				key, err := s.memoKey(ctx, memo, subject, version)
				if errors.Is(err, ErrKeyErased) {
					return jsonString(ErasedValue), true, nil
				}
				if err != nil {
					return nil, false, err
				}
				plain, err := kek.Open(key, ct, sealAAD(ev.ID.String(), ev.Action, f.Path, subject, version))
				if err != nil {
					return v, false, nil
				}
				return plain, true, nil
			})
			if err != nil {
				return nil, fmt.Errorf("audit unseal: %w", err)
			}
		}
		if out == nil {
			out = append([]types.AuditEvent(nil), evs...)
		}
		ev.Data = data
		out[i] = ev
	}
	if out == nil {
		return evs, nil
	}
	return out, nil
}

// Reseal is the spool drain's step for a row that waited under the pending key:
// each pending field is opened and sealed under its subject's own key, the
// marker is dropped, and the row can be stored. A field whose subject was
// erased while it waited becomes ErasedValue rather than being sealed under a
// new key. Any failure is returned and the caller keeps the spool line.
func (s *Sealer) Reseal(ctx context.Context, ev types.AuditEvent) (types.AuditEvent, error) {
	fields := sealFields[ev.Action]
	if len(fields) == 0 || !IsPending(ev) {
		return ev, nil
	}
	pk := s.pendingKey()
	if pk == nil {
		return ev, errors.New("audit seal: a pending row is spooled and the pending key is not available")
	}
	data := ev.Data
	for _, f := range fields {
		var err error
		data, _, err = rewrite(data, strings.Split(f.Path, "."), func(v json.RawMessage) (json.RawMessage, bool, error) {
			str, ok := stringValue(v)
			if !ok {
				return v, false, nil
			}
			name, ct, ok := parsePending(str)
			if !ok {
				return v, false, nil
			}
			plain, err := kek.Open(pk, ct, sealAAD(ev.ID.String(), ev.Action, f.Path, name, 0))
			if err != nil {
				return nil, false, fmt.Errorf("a pending field does not open under the pending key: %w", err)
			}
			defer clear(plain)
			subject, version, key, err := s.subjectKeyUnlessGone(ctx, ev.Time, name)
			if errors.Is(err, ErrKeyErased) {
				return jsonString(ErasedValue), true, nil
			}
			if err != nil {
				return nil, false, err
			}
			defer clear(key)
			sealed, err := kek.Seal(key, plain, sealAAD(ev.ID.String(), ev.Action, f.Path, subject, version))
			if err != nil {
				return nil, false, err
			}
			return jsonString(sealedPrefix + strconv.Itoa(version) + "." + b64([]byte(subject)) + "." + b64(sealed)), true, nil
		})
		if err != nil {
			return ev, fmt.Errorf("audit reseal: %w", err)
		}
	}
	data, err := withPendingMarker(data, false)
	if err != nil {
		return ev, err
	}
	ev.Data = data
	return ev, nil
}

// subjectKeyUnlessGone is subjectKey, except ErrKeyErased when the subject's
// key was destroyed after the row was written.
func (s *Sealer) subjectKeyUnlessGone(ctx context.Context, at time.Time, name string) (string, int, []byte, error) {
	subject := name
	if s.Resolve != nil {
		var err error
		if subject, err = s.Resolve(ctx, name); err != nil {
			return "", 0, nil, err
		}
	}
	if s.GoneSince != nil {
		gone, err := s.GoneSince(ctx, subject, at)
		if err != nil {
			return "", 0, nil, err
		}
		if gone {
			return "", 0, nil, ErrKeyErased
		}
	}
	version, key, err := s.Keys.Current(ctx, subject, SealPurpose)
	return subject, version, key, err
}
