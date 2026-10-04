// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// w4CovKeys is a SealKeys whose Current returns one fixed answer, so a key of
// the wrong size or an error can be injected. Key is never part of what it
// drives.
type w4CovKeys struct {
	key   []byte
	err   error
	calls int
}

func (k *w4CovKeys) Current(context.Context, string, string) (int, []byte, error) {
	k.calls++
	if k.err != nil {
		return 0, nil, k.err
	}
	return 1, append([]byte(nil), k.key...), nil
}

func (k *w4CovKeys) Key(context.Context, string, string, int) ([]byte, error) {
	return nil, errors.New("w4cov: Key is not part of this test")
}

// w4CovPendingEvent is a decide event sealed while the key store is down, so
// its reason waits under the pending key. It returns the sealer's pending key
// provider too, for a second sealer that re-seals it.
func w4CovPendingEvent(t *testing.T) (types.AuditEvent, func() []byte) {
	t.Helper()
	s, mk := newSealer(t)
	mk.down = true
	out, pending, err := s.Seal(context.Background(), decideEvent("alice", "needs a human reason"))
	if err != nil || !pending || !IsPending(out) {
		t.Fatalf("Seal with the key store down = pending %v, err %v; want a pending row", pending, err)
	}
	return out, s.Pending
}

func TestW4CovParseSealed(t *testing.T) {
	good := "seal1.3." + b64([]byte("alice")) + "." + b64([]byte("ciphertext"))
	v, sub, ct, ok := parseSealed(good)
	if !ok || v != 3 || sub != "alice" || string(ct) != "ciphertext" {
		t.Fatalf("parseSealed(good) = %d, %q, %q, %v", v, sub, ct, ok)
	}
	for name, s := range map[string]string{
		"no prefix":          "other." + good[len("seal1."):],
		"a pending string":   "seal1p." + b64([]byte("alice")) + "." + b64([]byte("c")),
		"two parts":          "seal1.3." + b64([]byte("alice")),
		"four parts":         good + ".extra",
		"version not a num":  "seal1.x." + b64([]byte("alice")) + "." + b64([]byte("c")),
		"version zero":       "seal1.0." + b64([]byte("alice")) + "." + b64([]byte("c")),
		"negative version":   "seal1.-1." + b64([]byte("alice")) + "." + b64([]byte("c")),
		"subject not b64":    "seal1.1.!!." + b64([]byte("c")),
		"ciphertext not b64": "seal1.1." + b64([]byte("alice")) + ".!!",
		"empty subject":      "seal1.1.." + b64([]byte("c")),
	} {
		if _, _, _, ok := parseSealed(s); ok {
			t.Errorf("parseSealed accepted %s: %q", name, s)
		}
	}
}

func TestW4CovParsePending(t *testing.T) {
	good := "seal1p." + b64([]byte("alice")) + "." + b64([]byte("ciphertext"))
	sub, ct, ok := parsePending(good)
	if !ok || sub != "alice" || string(ct) != "ciphertext" {
		t.Fatalf("parsePending(good) = %q, %q, %v", sub, ct, ok)
	}
	for name, s := range map[string]string{
		"no prefix":          "other." + good[len("seal1p."):],
		"a sealed string":    "seal1.1." + b64([]byte("alice")) + "." + b64([]byte("c")),
		"one part":           "seal1p." + b64([]byte("alice")),
		"three parts":        good + ".extra",
		"subject not b64":    "seal1p.!!." + b64([]byte("c")),
		"ciphertext not b64": "seal1p." + b64([]byte("alice")) + ".!!",
		"empty subject":      "seal1p.." + b64([]byte("c")),
	} {
		if _, _, ok := parsePending(s); ok {
			t.Errorf("parsePending accepted %s: %q", name, s)
		}
	}
}

func TestW4CovStringValueAndPendingMarker(t *testing.T) {
	if s, ok := stringValue(json.RawMessage(`"text"`)); !ok || s != "text" {
		t.Errorf("stringValue of a string = %q, %v", s, ok)
	}
	for _, v := range []string{`5`, `{"a":1}`, `[`} {
		if s, ok := stringValue(json.RawMessage(v)); ok || s != "" {
			t.Errorf("stringValue(%s) = %q, %v; want no string", v, s, ok)
		}
	}
	if _, err := withPendingMarker(json.RawMessage(`["pending_subject"]`), true); err == nil ||
		!strings.Contains(err.Error(), "audit seal: data is not an object") {
		t.Errorf("withPendingMarker on an array = %v, want the not-an-object refusal", err)
	}
	set, err := withPendingMarker(json.RawMessage(`{"a":1}`), true)
	if err != nil || !jsonEqual(t, set, `{"a":1,"pending_subject":true}`) {
		t.Errorf("withPendingMarker(on) = %s, %v", set, err)
	}
	cleared, err := withPendingMarker(set, false)
	if err != nil || !jsonEqual(t, cleared, `{"a":1}`) {
		t.Errorf("withPendingMarker(off) = %s, %v; the marker is still there", cleared, err)
	}
}

func TestW4CovSealRefusesWhenTheSubjectKeyCannotSeal(t *testing.T) {
	keys := &w4CovKeys{key: []byte("too-short")}
	s := &Sealer{Keys: keys}
	ev := decideEvent("alice", "needs a human reason")
	out, pending, err := s.Seal(context.Background(), ev)
	if err == nil || !strings.Contains(err.Error(), "audit seal:") {
		t.Fatalf("Seal with a short key = %v, want a seal failure", err)
	}
	if pending || string(out.Data) != string(ev.Data) {
		t.Errorf("a failed seal returned pending=%v and changed data %s", pending, out.Data)
	}
	if strings.Contains(string(out.Data), "seal1") {
		t.Error("a failed seal left a sealed marker in the row")
	}
}

func TestW4CovSealFallsBackToPendingWhenTheNameCannotBeResolved(t *testing.T) {
	s, mk := newSealer(t)
	injected := errors.New("directory down")
	s.Resolve = func(context.Context, string) (string, error) { return "", injected }

	out, pending, err := s.Seal(context.Background(), decideEvent("alice", "needs a human reason"))
	if err != nil || !pending || !IsPending(out) {
		t.Fatalf("Seal = pending %v, err %v; want a pending row", pending, err)
	}
	if got, _ := field(t, out, "reason").(string); !strings.HasPrefix(got, pendingPrefix) {
		t.Errorf("reason = %q, want a string under the pending key", got)
	}
	if mk.currCalls != 0 {
		t.Errorf("a key was asked for after the name failed to resolve: %d calls", mk.currCalls)
	}

	s.Pending = nil
	_, _, err = s.Seal(context.Background(), decideEvent("alice", "needs a human reason"))
	if !errors.Is(err, injected) || !strings.Contains(err.Error(), "no pending key") {
		t.Errorf("Seal with no pending key = %v, want the resolve error wrapped", err)
	}
}

func TestW4CovSealRefusesWhenThePendingKeyIsTheWrongSize(t *testing.T) {
	injected := errors.New("key store down")
	s := &Sealer{Keys: &w4CovKeys{err: injected}, Pending: func() []byte { return make([]byte, 16) }}
	_, pending, err := s.Seal(context.Background(), decideEvent("alice", "needs a human reason"))
	if !errors.Is(err, injected) || pending {
		t.Fatalf("Seal = pending %v, err %v; want the key error and no pending row", pending, err)
	}
	if !strings.Contains(err.Error(), "no pending key to hold the row") {
		t.Errorf("the refusal does not say the pending key is missing: %v", err)
	}
}

func TestW4CovUnsealLeavesWhatIsNotAWrittenSealAlone(t *testing.T) {
	s, mk := newSealer(t)
	ctx := context.Background()
	mk.keys["alice"] = [][]byte{make([]byte, 32)}
	for name, data := range map[string]string{
		"a string with the prefix that is not a seal": `{"reason":"seal1.not-a-seal"}`,
		"a non-string at the sealed path":             `{"reason":5,"note":"seal1.x"}`,
	} {
		ev := decideEvent("alice", "x")
		ev.Data = json.RawMessage(data)
		out, err := s.Unseal(ctx, []types.AuditEvent{ev})
		if err != nil {
			t.Fatalf("%s: Unseal = %v", name, err)
		}
		if string(out[0].Data) != data && !jsonEqual(t, out[0].Data, data) {
			t.Errorf("%s: data became %s, want it left as %s", name, out[0].Data, data)
		}
	}
	if mk.keyCalls != 0 {
		t.Errorf("a string that is not a seal asked for a key %d times", mk.keyCalls)
	}
}

func jsonEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var a, b any
	if json.Unmarshal(got, &a) != nil || json.Unmarshal([]byte(want), &b) != nil {
		return false
	}
	ag, _ := json.Marshal(a)
	bg, _ := json.Marshal(b)
	return string(ag) == string(bg)
}

func TestW4CovUnsealIsCopyOnWrite(t *testing.T) {
	s, _ := newSealer(t)
	ctx := context.Background()
	clear1 := decideEvent("alice", "x")
	clear1.Action = "run.start" // an action that seals nothing
	sealed, pending, err := s.Seal(ctx, decideEvent("alice", "a private reason"))
	if err != nil || pending {
		t.Fatalf("Seal = %v, %v", pending, err)
	}

	only := []types.AuditEvent{clear1}
	got, err := s.Unseal(ctx, only)
	if err != nil || len(got) != 1 || &got[0] != &only[0] {
		t.Fatalf("Unseal of an unsealed page = %v, %v; want the same slice back", got, err)
	}

	mixed := []types.AuditEvent{clear1, sealed}
	before := string(mixed[1].Data)
	got, err = s.Unseal(ctx, mixed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Data) != string(clear1.Data) {
		t.Errorf("the unsealed event changed: %s", got[0].Data)
	}
	if r, _ := field(t, got[1], "reason").(string); r != "a private reason" {
		t.Errorf("the sealed event was not opened: %q", r)
	}
	if string(mixed[1].Data) != before {
		t.Error("Unseal wrote through to the caller's slice")
	}
}

func TestW4CovUnsealRemembersAnErasedKeyForTheRestOfTheRead(t *testing.T) {
	s, mk := newSealer(t)
	ctx := context.Background()
	var evs []types.AuditEvent
	for _, reason := range []string{"first private reason", "second private reason"} {
		out, pending, err := s.Seal(ctx, decideEvent("alice", reason))
		if err != nil || pending {
			t.Fatalf("Seal = %v, %v", pending, err)
		}
		evs = append(evs, out)
	}
	mk.destroy("alice")
	mk.keyCalls = 0

	got, err := s.Unseal(ctx, evs)
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range got {
		if r := field(t, ev, "reason"); r != ErasedValue {
			t.Errorf("event %d reason = %v, want %q", i, r, ErasedValue)
		}
	}
	if mk.keyCalls != 1 {
		t.Errorf("an erased key was asked for %d times in one read, want 1", mk.keyCalls)
	}
}

func TestW4CovResealOfARowThatIsNotPendingChangesNothing(t *testing.T) {
	s, mk := newSealer(t)
	ev := decideEvent("alice", "a clear reason")
	out, err := s.Reseal(context.Background(), ev)
	if err != nil || string(out.Data) != string(ev.Data) {
		t.Fatalf("Reseal = %s, %v; want the row back unchanged", out.Data, err)
	}
	if mk.currCalls != 0 {
		t.Errorf("a non-pending row asked for a key: %d", mk.currCalls)
	}
}

func TestW4CovResealNeedsThePendingKey(t *testing.T) {
	pendingEv, _ := w4CovPendingEvent(t)
	s := &Sealer{Keys: &w4CovKeys{key: make([]byte, 32)}}
	out, err := s.Reseal(context.Background(), pendingEv)
	if err == nil || !strings.Contains(err.Error(), "the pending key is not available") {
		t.Fatalf("Reseal with no pending key = %v", err)
	}
	if !IsPending(out) || string(out.Data) != string(pendingEv.Data) {
		t.Error("a refused reseal changed the row: the spool line would be lost")
	}
}

func TestW4CovResealLeavesFieldsThatAreNotPendingStringsAlone(t *testing.T) {
	s, mk := newSealer(t)
	for name, data := range map[string]string{
		"a non-string at the field":    `{"pending_subject":true,"reason":7,"other":"seal1p.zzz"}`,
		"a string that is not pending": `{"pending_subject":true,"reason":"seal1p.bad","other":"seal1p.zzz"}`,
	} {
		ev := decideEvent("alice", "x")
		ev.Data = json.RawMessage(data)
		out, err := s.Reseal(context.Background(), ev)
		if err != nil {
			t.Fatalf("%s: Reseal = %v", name, err)
		}
		if strings.Contains(string(out.Data), PendingMarker) {
			t.Errorf("%s: the pending marker survived: %s", name, out.Data)
		}
		want := strings.Replace(data, `"pending_subject":true,`, "", 1)
		if !jsonEqual(t, out.Data, want) {
			t.Errorf("%s: data = %s, want %s", name, out.Data, want)
		}
	}
	if mk.currCalls != 0 {
		t.Errorf("fields that are not pending strings asked for a key: %d", mk.currCalls)
	}
}

func TestW4CovResealOfARowWhoseDataIsNotAnObjectFails(t *testing.T) {
	s, _ := newSealer(t)
	ev := decideEvent("alice", "x")
	ev.Data = json.RawMessage(`["pending_subject","seal1p.x"]`)
	out, err := s.Reseal(context.Background(), ev)
	if err == nil || !strings.Contains(err.Error(), "data is not an object") {
		t.Fatalf("Reseal = %v, want the not-an-object refusal", err)
	}
	if string(out.Data) != string(ev.Data) {
		t.Errorf("a failed reseal changed the data: %s", out.Data)
	}
}

func TestW4CovResealFailuresKeepTheSpoolLine(t *testing.T) {
	pendingEv, pk := w4CovPendingEvent(t)
	ctx := context.Background()
	injected := errors.New("directory down")
	good := make([]byte, 32)

	for _, c := range []struct {
		name   string
		sealer *Sealer
		want   string
	}{
		{"the subject key cannot seal", &Sealer{Keys: &w4CovKeys{key: []byte("short")}, Pending: pk}, "audit reseal:"},
		{"the name cannot be resolved", &Sealer{Keys: &w4CovKeys{key: good}, Pending: pk,
			Resolve: func(context.Context, string) (string, error) { return "", injected }}, ""},
		{"the erasure lookup fails", &Sealer{Keys: &w4CovKeys{key: good}, Pending: pk,
			GoneSince: func(context.Context, string, time.Time) (bool, error) { return false, injected }}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := c.sealer.Reseal(ctx, pendingEv)
			if err == nil {
				t.Fatal("Reseal succeeded")
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to contain %q", err, c.want)
			}
			if c.want == "" && !errors.Is(err, injected) {
				t.Errorf("error = %v, want the injected error wrapped", err)
			}
			if !IsPending(out) || string(out.Data) != string(pendingEv.Data) {
				t.Error("a failed reseal did not return the row as it was")
			}
		})
	}
}
