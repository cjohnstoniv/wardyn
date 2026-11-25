// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package reqcheck

import (
	"errors"
	"testing"
)

// stub is a checker whose every answer a test sets, and which counts the
// calls Decide makes of it.
type stub struct {
	guard        string
	classify     func(req Request) (string, error)
	peek         []byte
	peekSentence string
	classifyMsg  string
	extra        string
	permits      bool
	refusal      string
	liftable     bool

	order               []string
	guardCalls, peeks   int
	classifies, extras  int
	permitCalls, refuse int
	bodies              [][]byte
	withheld            []bool
}

func (s *stub) Guard(Request) string {
	s.guardCalls++
	s.order = append(s.order, "guard")
	return s.guard
}

func (s *stub) Classify(req Request) (string, error) {
	s.classifies++
	s.order = append(s.order, "classify")
	s.bodies = append(s.bodies, req.Body)
	s.withheld = append(s.withheld, req.BodyWithheld)
	return s.classify(req)
}

func (s *stub) PeekBody() ([]byte, string) {
	s.peeks++
	s.order = append(s.order, "peek")
	return s.peek, s.peekSentence
}

func (s *stub) ClassifyRefusal(error) string { return s.classifyMsg }

func (s *stub) Extra(string) string { s.extras++; s.order = append(s.order, "extra"); return s.extra }

func (s *stub) Permits(string) bool {
	s.permitCalls++
	s.order = append(s.order, "permits")
	return s.permits
}

func (s *stub) Refusal(string) (string, bool) {
	s.refuse++
	s.order = append(s.order, "refusal")
	return s.refusal, s.liftable
}

func classifiesTo(v string) func(Request) (string, error) {
	return func(Request) (string, error) { return v, nil }
}

func newStub() *stub {
	return &stub{classify: classifiesTo("v"), classifyMsg: "could not classify", refusal: "not granted", permits: true}
}

// TestDecideZeroEffectRefuses: the zero value of Effect, and of a Decision, is a refusal.
func TestDecideZeroEffectRefuses(t *testing.T) {
	var e Effect
	if e != Refuse {
		t.Fatalf("the zero Effect = %d, want Refuse (%d)", e, Refuse)
	}
	var d Decision[string]
	if d.Effect != Refuse || d.Effect == Allow || d.Effect == Hold {
		t.Fatalf("a zero Decision has Effect %d, want Refuse", d.Effect)
	}
	if Allow == Refuse || Allow == Hold || Hold == Refuse {
		t.Fatal("the three effects are not distinct")
	}
}

// TestDecideGuardRefusesBeforeClassify: a guard sentence refuses, and nothing
// else is asked.
func TestDecideGuardRefusesBeforeClassify(t *testing.T) {
	s := newStub()
	s.guard = "guarded"
	d := Decide[string](s, Request{Path: "/x"})
	if d.Effect != Refuse || d.Sentence != "guarded" || d.Verdict != "" {
		t.Fatalf("Decide() = %+v, want a refusal with the guard's sentence and no verdict", d)
	}
	if s.classifies+s.peeks+s.extras+s.permitCalls+s.refuse != 0 {
		t.Fatalf("calls after a guard refusal: %v", s.order)
	}
}

// TestDecideAllowsOnlyFromPermits: Allow comes from Permits alone, after the
// guard, the classify and the extra rule, in that order.
func TestDecideAllowsOnlyFromPermits(t *testing.T) {
	s := newStub()
	d := Decide[string](s, Request{})
	if d.Effect != Allow || d.Verdict != "v" || d.Sentence != "" {
		t.Fatalf("Decide() = %+v, want Allow of v", d)
	}
	want := []string{"guard", "classify", "extra", "permits"}
	if len(s.order) != len(want) {
		t.Fatalf("calls = %v, want %v (Refusal is never asked of a permitted request)", s.order, want)
	}
	for i := range want {
		if s.order[i] != want[i] {
			t.Fatalf("calls = %v, want %v", s.order, want)
		}
	}
	s.permits = false
	if d := Decide[string](s, Request{}); d.Effect == Allow {
		t.Fatalf("Decide() = %+v with Permits false, want no Allow", d)
	}
}

// TestDecideNeverAllowsWithoutEveryStep walks every combination of the
// checker's answers: Allow comes out exactly when the guard passes, Classify
// succeeds (after a peek that succeeds when one is asked), Extra passes and
// Permits says yes.
func TestDecideNeverAllowsWithoutEveryStep(t *testing.T) {
	allows := 0
	for mask := 0; mask < 1<<7; mask++ {
		bit := func(i int) bool { return mask&(1<<i) != 0 }
		guardRefuses, needsBody, peekRefuses, classifyErr, extraRefuses, permits, liftable := bit(0), bit(1), bit(2), bit(3), bit(4), bit(5), bit(6)
		s := newStub()
		if guardRefuses {
			s.guard = "g"
		}
		if peekRefuses {
			s.peekSentence = "p"
		}
		if extraRefuses {
			s.extra = "e"
		}
		s.permits, s.liftable = permits, liftable
		s.classify = func(req Request) (string, error) {
			if needsBody && req.BodyWithheld {
				return "", ErrNeedsBody
			}
			if classifyErr {
				return "", errors.New("bad")
			}
			return "v", nil
		}
		d := Decide[string](s, Request{})
		wantAllow := !guardRefuses && !(needsBody && peekRefuses) && !classifyErr && !extraRefuses && permits
		if (d.Effect == Allow) != wantAllow {
			t.Fatalf("mask %07b: Effect %d, want Allow=%v", mask, d.Effect, wantAllow)
		}
		if d.Effect != Allow && d.Sentence == "" {
			t.Fatalf("mask %07b: a non-Allow decision has no sentence", mask)
		}
		if d.Effect == Hold && (!liftable || extraRefuses || classifyErr || guardRefuses) {
			t.Fatalf("mask %07b: Hold from a step that cannot hold", mask)
		}
		if wantAllow {
			allows++
		}
	}
	if allows == 0 {
		t.Fatal("no combination allowed anything: the walk proved nothing")
	}
}

// TestDecideNeedsBodyPeeksOnce: a route classified on its body is classified
// withheld, peeked once, and classified again with the whole body.
func TestDecideNeedsBodyPeeksOnce(t *testing.T) {
	s := newStub()
	s.peek = []byte(`{"a":1}`)
	s.classify = func(req Request) (string, error) {
		if req.BodyWithheld {
			return "", ErrNeedsBody
		}
		return "v:" + string(req.Body), nil
	}
	d := Decide[string](s, Request{Body: []byte("caller-supplied")})
	if d.Effect != Allow || d.Verdict != `v:{"a":1}` {
		t.Fatalf("Decide() = %+v, want Allow of the verdict read from the peeked body", d)
	}
	if s.peeks != 1 || s.classifies != 2 {
		t.Fatalf("peeks %d classifies %d, want 1 and 2", s.peeks, s.classifies)
	}
	if !s.withheld[0] || s.withheld[1] || s.bodies[0] != nil {
		t.Fatalf("first classify withheld=%v body=%q, second withheld=%v — want withheld with no body, then the body", s.withheld[0], s.bodies[0], s.withheld[1])
	}
	// A second ErrNeedsBody is an error: the body is not asked for twice.
	s = newStub()
	s.classify = func(Request) (string, error) { return "", ErrNeedsBody }
	d = Decide[string](s, Request{})
	if d.Effect != Refuse || d.Sentence != "could not classify" || s.peeks != 1 || s.classifies != 2 {
		t.Fatalf("Decide() = %+v after peeks %d classifies %d, want one peek and a classify refusal", d, s.peeks, s.classifies)
	}
}

// TestDecidePeekRefusal: the peek's own sentence refuses, and nothing is classified again.
func TestDecidePeekRefusal(t *testing.T) {
	s := newStub()
	s.peekSentence = "too large"
	s.classify = func(Request) (string, error) { return "", ErrNeedsBody }
	d := Decide[string](s, Request{})
	if d.Effect != Refuse || d.Sentence != "too large" || d.Verdict != "" || s.classifies != 1 {
		t.Fatalf("Decide() = %+v after %d classifies, want a refusal with the peek's sentence", d, s.classifies)
	}
}

// TestDecideClassifyErrorRefusesWithTheZeroVerdict: a Classify error refuses
// with ClassifyRefusal's sentence, whatever verdict came back with it.
func TestDecideClassifyErrorRefusesWithTheZeroVerdict(t *testing.T) {
	s := newStub()
	s.classify = func(Request) (string, error) { return "partial", errors.New("bad") }
	d := Decide[string](s, Request{})
	if d.Effect != Refuse || d.Sentence != "could not classify" || d.Verdict != "" {
		t.Fatalf("Decide() = %+v, want a refusal carrying the zero verdict", d)
	}
	if s.extras+s.permitCalls != 0 {
		t.Fatalf("calls after a classify error: %v", s.order)
	}
}

// TestDecideExtraRefusalIsNeverHeld: the run's own rule refuses even when the
// forge would let a person lift the capability refusal.
func TestDecideExtraRefusalIsNeverHeld(t *testing.T) {
	s := newStub()
	s.extra, s.permits, s.liftable = "branch rule", false, true
	d := Decide[string](s, Request{})
	if d.Effect != Refuse || d.Sentence != "branch rule" || d.Verdict != "v" {
		t.Fatalf("Decide() = %+v, want a refusal with the extra sentence and the classified verdict", d)
	}
	if s.permitCalls != 0 || s.refuse != 0 {
		t.Fatalf("Permits and Refusal were asked after an extra refusal: %v", s.order)
	}
	// And a permitted request an extra rule refuses is still refused.
	s = newStub()
	s.extra = "branch rule"
	if d := Decide[string](s, Request{}); d.Effect != Refuse {
		t.Fatalf("Decide() = %+v, want the extra rule to refuse a permitted request", d)
	}
}

// TestDecideRefusalIsHoldOnlyWhenLiftable: after Permits says no, a liftable
// refusal holds and any other refuses, each with the forge's sentence.
func TestDecideRefusalIsHoldOnlyWhenLiftable(t *testing.T) {
	for _, liftable := range []bool{true, false} {
		s := newStub()
		s.permits, s.liftable = false, liftable
		d := Decide[string](s, Request{})
		want := Refuse
		if liftable {
			want = Hold
		}
		if d.Effect != want || d.Sentence != "not granted" || d.Verdict != "v" {
			t.Fatalf("liftable=%v: Decide() = %+v, want %d with the forge's sentence", liftable, d, want)
		}
	}
}
