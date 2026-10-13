// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The cast the #1508 probes share: the real routes over a real Postgres store, a target person, two sponsors (one
// holding a service_attach grant), three admin tiers, a service identity per sponsor, and one bearer of each
// shape for the people who can hold one.

const (
	plusBot = "plus-bot"
	plusPrn = "service:plus-bot"
)

// probeEnv is sponsorEnv plus the target person, the second sponsor's identity and the bearers.
type probeEnv struct {
	sponsorEnv
	pat *http.Cookie // the target: personSub, signed in as a plain member
	// patWdn and patWdg are the target's own api token and a portal's token for them; sponsorWdn and sponsorWdg
	// are the first sponsor's. svcTok is a token of ci-bot, minted by its sponsor.
	patWdn, patWdg, sponsorWdn, sponsorWdg, svcTok string
}

func newProbeEnv(t *testing.T) probeEnv {
	t.Helper()
	e := probeEnv{sponsorEnv: newSponsorEnv(t)}
	ctx := context.Background()
	e.h.srv.cfg.RunnerOrgURL = "https://wardyn.example.com" // runner registration tokens are bound to the organisation URL
	e.h.srv.runEvents.hold = time.Nanosecond                // the events route otherwise holds its connection for the stream
	if _, err := e.st.PutSiteConfig(ctx, types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if w := e.createPerson(t, e.sec, personSub, personEmail); w.Code != http.StatusCreated {
		t.Fatalf("create the target: %d %s", w.Code, w.Body.String())
	}
	e.pat = e.login(t, personSub, personEmail, oidc.RoleUser)
	// The three admin tiers and the plain member can sponsor or be invited like anyone: bind their identities.
	for _, sub := range []string{"root", "sec", "mem"} {
		e.bind(t, sub, "https://idp.example", "", "")
	}
	e.mustCreate(t, plusBot, svcSponsor2)
	e.grant(t, "user", "sponsor2@corp.example", "allow", featureServiceAttach)

	e.patWdn = e.seedLegacyToken(t, personSub, oidc.RoleUser, "").Token
	e.sponsorWdn = e.seedLegacyToken(t, svcSponsor, oidc.RoleUser, "").Token
	portal, err := e.st.CreateDelegate(ctx, types.Delegate{ID: uuid.New(), Name: "portal", IdPClientID: "portal-client", Group: "portal-users"},
		newBearer(delegateCredentialPrefix))
	if err != nil {
		t.Fatal(err)
	}
	delegated := func(principal, email string) string {
		raw := newBearer(delegatedTokenPrefix)
		tok := types.DelegatedToken{
			ID: uuid.New(), DelegateID: portal.ID, Principal: principal, Email: email,
			UserType: types.UserTypeStandard, Groups: []string{"portal-users"},
			CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(delegatedTokenTTL),
		}
		if _, err := e.st.MintDelegatedToken(ctx, tok, raw, time.Now()); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	e.patWdg = delegated(personSub, personEmail)
	e.sponsorWdg = delegated(svcSponsor, "sponsor@corp.example")
	e.svcTok = e.mustMint(t, e.sponsor, svcName).Token
	e.h.srv.router = e.h.srv.routes() // routes that mount on the wired stores
	return e
}

// ask sends one request as a caller: a cookie session when cookie is set, else a bearer ("" is no credential).
// A request that does not finish in five seconds is cut off, so a streaming route cannot hold a walk.
func (e probeEnv) ask(t *testing.T, cookie *http.Cookie, bearer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if cookie != nil {
		return doSSOCtx(t, e.h.srv, ctx, method, path, cookie, body)
	}
	r := httptest.NewRequestWithContext(ctx, method, path, nil)
	if body != "" {
		r = httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body))
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	r.Host = "127.0.0.1"
	r.RemoteAddr = "127.0.0.1:40000"
	w := httptest.NewRecorder()
	panicFails(t, e.h.srv.Handler()).ServeHTTP(w, r)
	return w
}
