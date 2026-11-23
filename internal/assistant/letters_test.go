// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistant

import (
	"strings"
	"testing"
)

func TestLettersAreStableAndFirstSeenOrdered(t *testing.T) {
	l := NewLetters()
	a := l.Add(Person{Email: "ada@corp.example", Name: "Ada Lovelace"})
	b := l.Add(Person{Email: "bob@corp.example"})
	if a != "Person A" || b != "Person B" {
		t.Fatalf("labels = %q, %q", a, b)
	}
	if again := l.Add(Person{Name: "ada lovelace"}); again != a {
		t.Fatalf("the same person twice got %q, want %q", again, a)
	}
	if got := len(l.Map()); got != 2 {
		t.Fatalf("map has %d people, want 2", got)
	}
	if l.Add(Person{}) != "" {
		t.Fatal("a person with no name or address got a letter")
	}
}

func TestLettersReplaceWholeAddressesAndNames(t *testing.T) {
	l := NewLetters()
	l.Add(Person{Email: "ada@corp.example", Name: "Ada Lovelace"})
	l.Add(Person{Email: "api@corp.example"})
	in := "Owner ada@corp.example (Ada Lovelace) asked; host api.github.com and mail api@corp.example, not ada@corp.example.com"
	got := l.Apply(in)
	for _, want := range []string{"Owner Person A (Person A) asked", "host api.github.com", "mail Person B,", "not ada@corp.example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("Apply = %q, missing %q", got, want)
		}
	}
	if back := l.Restore("Person A and Person B"); back != "ada@corp.example and api@corp.example" {
		t.Errorf("Restore = %q", back)
	}
}

func TestLettersBoundaries(t *testing.T) {
	l := NewLetters()
	l.Add(Person{Name: "Ann"})
	if got := l.Apply("Anna met Ann. Joann too; Ann's, Ann"); got != "Anna met Person A. Joann too; Person A's, Person A" {
		t.Errorf("Apply = %q", got)
	}
}

func TestLettersPastZ(t *testing.T) {
	l := NewLetters()
	var last string
	for i := 0; i < 28; i++ {
		last = l.Add(Person{Name: "name" + string(rune('a'+i%26)) + string(rune('a'+i/26))})
	}
	if last != "Person AB" {
		t.Errorf("28th label = %q, want Person AB", last)
	}
}

func TestLettersFromMapKeepsLetters(t *testing.T) {
	l := NewLetters()
	l.Add(Person{Email: "ada@corp.example"})
	l.Add(Person{Name: "Bob Barker"})
	r := LettersFromMap(l.Map())
	if got, want := r.Apply("ada@corp.example, Bob Barker"), "Person A, Person B"; got != want {
		t.Errorf("rebuilt Apply = %q, want %q", got, want)
	}
}
