// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

type tokenListFails struct{ *directoryFake }

func (tokenListFails) ListAPITokens(context.Context) ([]types.APIToken, error) {
	return nil, errors.New("tokens unreadable: secret-dsn")
}

// A create-person that cannot read the directory of known principals cannot check for a collision, so it
// answers 500 and writes no person row.
func TestPeopleCreateFailsWhenTheDirectoryCannotBeRead(t *testing.T) {
	st := newDirectoryFake()
	srv, _ := peopleServer(t, tokenListFails{st}, nil)

	w := do(t, srv, http.MethodPost, "/api/v1/people", adminToken, `{"principal":"fresh","email":"fresh@corp.example"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("POST /people = %d %s, want 500", w.Code, w.Body.String())
	}
	if len(st.people) != 0 {
		t.Errorf("a create that could not check collisions stored people: %v", st.people)
	}
}
