// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// forwardPGStore keeps the attach tickets on Postgres and everything else where the rig has it.
type forwardPGStore struct {
	store.Store
	pg store.PG
}

func (s forwardPGStore) MintAttachTicket(ctx context.Context, token string, t store.AttachTicket, issued, expires time.Time) error {
	return s.pg.MintAttachTicket(ctx, token, t, issued, expires)
}

func (s forwardPGStore) ConsumeAttachTicket(ctx context.Context, token string, now time.Time) (store.AttachTicket, bool, error) {
	return s.pg.ConsumeAttachTicket(ctx, token, now)
}

// TestAttachForward_TicketIsSingleUse_PG: a ticket is burned by the attempt that spends it, whether the route
// refuses it (the switch is off) or opens a forward with it. Guarded by WARDYN_TEST_PG.
func TestAttachForward_TicketIsSingleUse_PG(t *testing.T) {
	pool := throwawayPGPool(t)
	wrap := func(s store.Store) store.Store { return forwardPGStore{Store: s, pg: store.NewPG(pool)} }

	t.Run("a ticket the off switch refused cannot be redeemed again", func(t *testing.T) {
		fr := newForwardRigOver(t, false, nil, wrap)
		tk := fr.mint(t)
		fr.wantReason(t, fr.get(t, fr.runID, "port=3000&ticket="+tk), http.StatusConflict, reasonRunForwardOff)
		w := fr.get(t, fr.runID, "port=3000&ticket="+tk)
		if w.Code < 400 || w.Code >= 500 || wireReason(t, w) == reasonRunForwardOff {
			t.Errorf("the spent ticket's second use = %d %s, want the invalid-ticket refusal", w.Code, w.Body)
		}
	})
	t.Run("a ticket that opened one forward cannot open a second", func(t *testing.T) {
		fr := newForwardRigOver(t, true, nil, wrap)
		tk := fr.mint(t)
		c, _, err := fr.dialQuery(t, "port=3000&ticket="+tk)
		if err != nil {
			t.Fatalf("first dial: %v", err)
		}
		defer c.CloseNow()
		if _, resp, err := fr.dialQuery(t, "port=3000&ticket="+tk); err == nil {
			t.Error("the ticket opened a second forward")
		} else if resp == nil || resp.StatusCode < 400 || resp.StatusCode >= 500 {
			t.Errorf("second dial: %v (response %v), want a 4xx refusal", err, resp)
		}
	})
}
