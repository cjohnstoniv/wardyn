// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	xhtml "golang.org/x/net/html"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// desktopBootData is the viewer page's data-wardyn-desktop attribute, read as a browser reads it.
func desktopBootData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	z := xhtml.NewTokenizer(rec.Body)
	for z.Next() != xhtml.ErrorToken {
		tok := z.Token()
		if tok.Type != xhtml.StartTagToken || tok.Data != "html" {
			continue
		}
		for _, a := range tok.Attr {
			if a.Key != "data-wardyn-desktop" {
				continue
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(a.Val), &got); err != nil {
				t.Fatalf("data-wardyn-desktop %q is not JSON: %v", a.Val, err)
			}
			return got
		}
	}
	t.Fatalf("no data-wardyn-desktop on the page: %d", rec.Code)
	return nil
}

// The open boot data carries watermark: who is the session's email, else its principal; run is the run id; required
// is the organisation's rule, and true when the site config cannot be read.
func TestDesktopBootWatermark(t *testing.T) {
	h, _ := newDesktopHarness(t)
	st := &desktopSiteStore{desktopClosedStore: h.srv.cfg.Store.(*desktopClosedStore)}
	h.srv.cfg.Store = st
	open := func(t *testing.T, email string) map[string]any {
		t.Helper()
		tok, err := mintAttachTicket(withOIDCEmail(context.Background(), email), h.store, h.run.ID, types.ActorHuman, h.owner, oidc.RoleUser, oidc.Issue{At: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
		c := desktopCookie(t, h.enterDesktop(tok))
		got := uiGet(h, desktopPath(h, "/"), c)
		if got.Code != http.StatusOK {
			t.Fatalf("viewer = %d %s", got.Code, got.Body.String())
		}
		return desktopBootData(t, got)
	}
	watermark := func(t *testing.T, boot map[string]any) map[string]any {
		t.Helper()
		w, ok := boot["watermark"].(map[string]any)
		if !ok {
			t.Fatalf("boot data has no watermark: %v", boot)
		}
		return w
	}
	t.Run("who is the session's email, run is the run id, required follows the rule", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			sc   types.SiteConfig
			err  error
			want bool
		}{
			{"no rule stored", types.SiteConfig{}, nil, false},
			{"an empty block", types.SiteConfig{DesktopWatermark: &types.SiteDesktopWatermark{}}, nil, false},
			{"required", types.SiteConfig{DesktopWatermark: &types.SiteDesktopWatermark{Required: true}}, nil, true},
			{"an unreadable site config reads as required", types.SiteConfig{}, errors.New("pool closed"), true},
		} {
			st.setSite(tc.sc, tc.err)
			w := watermark(t, open(t, "alex@example.com"))
			if w["who"] != "alex@example.com" || w["run"] != h.run.ID.String() || w["required"] != tc.want {
				t.Errorf("%s: watermark = %v, want who alex@example.com, run %s, required %v", tc.name, w, h.run.ID, tc.want)
			}
		}
	})
	t.Run("who is the principal when the session has no email", func(t *testing.T) {
		st.setSite(types.SiteConfig{}, nil)
		w := watermark(t, open(t, ""))
		if w["who"] != h.owner {
			t.Errorf("who = %v, want the principal %q", w["who"], h.owner)
		}
	})
	t.Run("the refusal page carries no watermark", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if !h.srv.serveDesktopRefusal(rec, httptest.NewRequest(http.MethodGet, desktopPath(h, "/"), nil), h.run.ID, "no") {
			t.Fatal("no viewer page was served")
		}
		if boot := desktopBootData(t, rec); boot["state"] != "refused" || boot["watermark"] != nil {
			t.Errorf("refusal boot data = %v, want state refused and no watermark", boot)
		}
	})
}
