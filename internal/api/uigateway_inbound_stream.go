// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The inbound arm's hold on an upgraded stream an inbound token opened: re-admitted every half
// inboundResolveTTL. Split out of uigateway_inbound.go for scripts/check-file-size.sh.

// watchInboundStream holds an upgraded stream an inbound token opened to the authority that admitted it:
// every half inboundResolveTTL (a tick; uiInboundStreamEvery in tests) the token is admitted again, uncached,
// by admitInboundToken, so the token's revoke or expiry, its service identity or sponsor going, its owner
// losing the run or the app no longer naming its caller all reach the stream as they reach a request, as
// does the run's profile coming to deny UI apps or interactive sessions (uiInboundUnitDenies). A
// refusal closes the stream and then writes the inbound.deny row a refused request writes. A lookup or read
// that cannot be made keeps the stream and asks again next tick, so one store blip between two good checks
// ends no stream; but a stream whose authority no check has confirmed for inboundResolveTTL since the last
// one that did, the staleness the design allows, is closed with an inbound.deny row naming what was
// unavailable (token_lookup_unavailable, or run_unreadable for the run, its policy or its profile): fail
// closed, never a stream held open on a store that stays down. Each check runs off the watch, on the
// stream's own context and within inboundReadmitTimeout, so a check that never answers holds nothing up:
// the tick closes the stream past the bound with the check still in flight (as token_lookup_unavailable,
// unless an earlier check named what failed). So a stream ends at most inboundResolveTTL plus one tick
// after its last good check, hung store or not. The first good check is the lookup of the session the
// stream opened on, a cached session's included (inboundReadmission.checked), and the first re-check is
// due one tick after it, or at once when the session is older than that. Past that bound the stream also waits
// out the site's session budget (session_budget.go), counted from the first check that had no answer, and asks
// again every few seconds meanwhile; a budget of 0 keeps the bound alone. The close comes first; its row is written after it, off the watch and within
// inboundReadmitTimeout, so an audit write that hangs delays no close. A browser session's stream carries
// no re-admission and is left as it was (TestUIGateway_EstablishedConnectionOutlivesTheRevoke).
func (s *Server) watchInboundStream(resp *http.Response) {
	a, ok := resp.Request.Context().Value(inboundReadmitCtxKey{}).(inboundReadmission)
	if !ok {
		return
	}
	body, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		return // ReverseProxy refuses a 101 without a writable body itself
	}
	stream := &inboundStream{ReadWriteCloser: body, done: make(chan struct{})}
	resp.Body = stream
	go func() {
		// ctx is the stream's: its end cancels a check still in flight.
		ctx, cancel := context.WithCancel(a.r.Context())
		defer cancel()
		actor := types.ActorHuman // as auditInboundRefused writes it
		if isServicePrincipal(a.principal) {
			actor = serviceActorType
		}
		b := s.newSessionBudget(ctx, sessionWatchID{Run: a.runID, Actor: actor, Principal: a.principal, Surface: sessionSurfaceInbound})
		defer b.leave()
		every := cmp.Or(s.uiInboundStreamEvery, inboundResolveTTL/2)
		tick := time.NewTimer(max(0, every-s.cfg.Now().Sub(a.checked)))
		defer tick.Stop()
		type readmitted struct {
			at          time.Time
			ok          bool
			unavailable string
			refusal     func(context.Context)
		}
		results := make(chan readmitted, 1) // one check at a time, so its send never blocks
		checking := false
		confirmed := a.checked // when the last check that admitted the stream began
		failing := ""          // what the last check to answer could not read, "" when it admitted
		stale := func() bool { return s.cfg.Now().Sub(confirmed) >= inboundResolveTTL }
		// end closes the stream, then writes refusal's row, if any, off the watch and within the timeout.
		end := func(refusal func(context.Context)) {
			_ = stream.Close() // the relay's copy ends, and the caller's connection with it
			if refusal != nil {
				go func() {
					actx, acancel := context.WithTimeout(context.WithoutCancel(ctx), inboundReadmitTimeout)
					defer acancel()
					refusal(actx)
				}()
			}
		}
		unconfirmedRow := func(reason string) func(context.Context) {
			return func(ctx context.Context) {
				s.auditInboundRefused(ctx, a.r, a.runID, a.app, a.principal, a.tokenID, reason)
			}
		}
		startCheck := func() {
			if checking {
				return
			}
			checking = true
			go func() {
				at := s.cfg.Now()
				cctx, ccancel := context.WithTimeout(ctx, inboundReadmitTimeout)
				defer ccancel()
				ok, unavailable, refusal := s.readmitInbound(cctx, a)
				results <- readmitted{at: at, ok: ok, unavailable: unavailable, refusal: refusal}
			}()
		}
		for {
			select {
			case <-stream.done:
				return
			case <-tick.C:
				tick.Reset(every)
				if (checking || failing != "") && stale() && b.observe("readmit", unconfirmed(errors.New(cmp.Or(failing, reasonTokenLookupUnavailable)))) == budgetSpent {
					b.end()
					end(unconfirmedRow(cmp.Or(failing, reasonTokenLookupUnavailable)))
					return
				}
				startCheck()
			case <-b.retryC():
				startCheck()
			case res := <-results:
				checking = false
				switch {
				case res.ok:
					confirmed, failing = res.at, ""
					b.observe("readmit", nil)
				case res.unavailable == "":
					b.observe("readmit", errSessionRefused)
					end(res.refusal)
					return
				default:
					failing = res.unavailable
					if v := b.observe("readmit", unconfirmed(errors.New(failing))); stale() && v == budgetSpent {
						b.end()
						end(unconfirmedRow(failing))
						return
					}
				}
			}
		}
	}()
}

// inboundStream is the sandbox side of an inbound-token stream: its first Close also ends the watch.
type inboundStream struct {
	io.ReadWriteCloser
	once sync.Once
	done chan struct{}
	err  error
}

func (c *inboundStream) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.err = c.ReadWriteCloser.Close()
	})
	return c.err
}
