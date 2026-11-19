// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A use-policy subject can name a unit, by its canonical lowercase uuid and nothing else.
func TestRunnerPoolUseSubjectsUnit(t *testing.T) {
	id := uuid.New().String()
	unit := func(s string) []RunnerPoolSubject {
		return []RunnerPoolSubject{{SubjectType: CapabilitySubjectType("unit"), Subject: s}}
	}
	for _, tc := range []struct {
		name string
		in   []RunnerPoolSubject
		ok   bool
	}{
		{"a unit id", unit(id), true},
		{"a unit beside a person", append(unit(id), RunnerPoolSubject{SubjectType: CapabilitySubjectUser, Subject: "alice"}), true},
		{"a unit name", unit("platform"), false},
		{"an upper-case id", unit(strings.ToUpper(id)), false},
		{"a braced id", unit("{" + id + "}"), false},
		{"a blank unit", unit(""), false},
		{"the same unit twice", append(unit(id), unit(id)...), false},
	} {
		if err := ValidateRunnerPoolUseSubjects(tc.in); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
