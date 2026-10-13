// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// cancelOnReadApproval cancels the request's context on the first approval
// read and answers pending, so the client "hangs up" mid-hold.
type cancelOnReadApproval struct{ cancel context.CancelFunc }

func (c cancelOnReadApproval) readApproval(context.Context, uuid.UUID) (types.ApprovalState, int, error) {
	c.cancel()
	return types.ApprovalPending, http.StatusOK, nil
}

// TestADOHold_ClientHangUpMidHoldLeavesNoDeniedRow: a client that disconnects
// while its write is parked was never refused, so the gate emits no
// brokered:ado:denied decision and writes no body.
func TestADOHold_ClientHangUpMidHoldLeavesNoDeniedRow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newADOHoldHarness(t, newCapControlPlane(t), cancelOnReadApproval{cancel})

	raw := "PATCH /acme/proj/_apis/wit/workitems/1?api-version=7.1 HTTP/1.1\r\nHost: " + adoHost +
		"\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(adoWorkItemPatch)) + "\r\n\r\n" + adoWorkItemPatch
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.p.serveMITMRequest(rec, req.WithContext(ctx), adoHost, 443)

	if ctx.Err() == nil {
		t.Fatal("the request was never held: the approval was not read")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a hung-up client got a body: %q", rec.Body.String())
	}
	if log := h.log(); strings.Contains(log, ruleSourceADODenied) {
		t.Errorf("a hung-up client has a %s row:\n%s", ruleSourceADODenied, log)
	}
}
