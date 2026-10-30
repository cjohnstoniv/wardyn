// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// The runner-exits stage fails when the organisation parked any of this computer's runner's exit
// receipts, and says who can look further; none parked is ok, and a runner it does not show is skipped.
func TestDoctorRunnerExitsStageSaysWhenReceiptsAreParked(t *testing.T) {
	id := uuid.New()
	for parked, want := range map[int]doctor.Status{0: doctor.StatusOK, 2: doctor.StatusFail} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/me/runners/"+id.String() {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"id":%q,"name":"laptop","state":"claimed","parked_exits":%d}`, id, parked)
		}))
		st := parkedExitStage(t.Context(), sdk.New(srv.URL, doctorToken), id)
		srv.Close()
		if st.Name != "runner-exits" || st.Status != want {
			t.Errorf("%d parked: stage %+v, want %s", parked, st, want)
		}
		if parked > 0 && (!strings.Contains(st.Detail, "parked 2 of this runner's exit receipts") || !strings.Contains(st.Fix, "runner.exit.park")) {
			t.Errorf("the parked stage does not say how many or where to look: %+v", st)
		}
	}
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	st := parkedExitStage(t.Context(), sdk.New(srv.URL, doctorToken), id)
	if st.Status != doctor.StatusSkip {
		t.Errorf("a runner the organisation does not show: %+v, want skip", st)
	}
	if !strings.Contains(st.Detail, "revoked") || !strings.Contains(st.Detail, "runner.exit.park") {
		t.Errorf("the skip does not point a revoked runner's owner at the administrator's audit rows: %+v", st)
	}
}
