// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// brokenSiteStore is a store whose site config cannot be read.
type brokenSiteStore struct{ store.Store }

func (brokenSiteStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, errors.New("site config unavailable")
}

// TestAzureFoundrySignIn_EveryUnusableSetupIsRefusedBeforeAnyRedirect: each
// way the row or the console application can be unusable ends the start leg in
// a JSON refusal, with no cookie set and no redirect to an authority.
func TestAzureFoundrySignIn_EveryUnusableSetupIsRefusedBeforeAnyRedirect(t *testing.T) {
	for _, tc := range []struct {
		name       string
		break_     func(f *azureFixture)
		wantStatus int
		wantReason string
	}{
		{"no store", func(f *azureFixture) { f.srv.cfg.Store = nil }, http.StatusNotFound, reasonAzureSignInUnconfigured},
		{"no by-uid source", func(f *azureFixture) { f.srv.cfg.AzureFoundryEntra = nil }, http.StatusNotFound, reasonAzureSignInUnconfigured},
		{"site config unreadable", func(f *azureFixture) { f.srv.cfg.Store = brokenSiteStore{} }, http.StatusInternalServerError, reasonInternalError},
		{"by-uid source fails", func(f *azureFixture) {
			f.srv.cfg.AzureFoundryEntra = func(context.Context, string) (ADOEntraConfig, bool, error) {
				return ADOEntraConfig{}, false, errors.New("boom")
			}
		}, http.StatusInternalServerError, reasonInternalError},
		{"by-uid source knows no such row", func(f *azureFixture) {
			f.srv.cfg.AzureFoundryEntra = func(context.Context, string) (ADOEntraConfig, bool, error) {
				return ADOEntraConfig{}, false, nil
			}
		}, http.StatusNotFound, reasonAzureSignInUnknownRow},
		{"application has no redirect URL", func(f *azureFixture) { f.app.RedirectURL = "" }, http.StatusInternalServerError, reasonInternalError},
		{"application is not the console login application", func(f *azureFixture) { f.app.LoginClientID = "" }, http.StatusNotFound, reasonAzureSignInUnconfigured},
		{"authority override without the test hatch", func(f *azureFixture) { f.app.AllowTestEndpoints = false }, http.StatusInternalServerError, reasonInternalError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAzureFixture(t)
			tc.break_(f)
			w := f.start(f.fake.Subject(), azUIDAnthropic)
			if w.Code != tc.wantStatus || !strings.Contains(w.Body.String(), tc.wantReason) {
				t.Fatalf("status %d body %q; want %d with reason %q", w.Code, w.Body.String(), tc.wantStatus, tc.wantReason)
			}
			if w.Header().Get("Location") != "" || len(w.Result().Cookies()) != 0 {
				t.Fatalf("a refused start redirected (%q) or set cookies %v", w.Header().Get("Location"), w.Result().Cookies())
			}
		})
	}
}

// TestAzureFoundryCallback_FailuresAfterTheStartLegRedirectWithAFixedCode:
// whatever goes wrong between the start leg and the write, the browser lands on
// the console's error page with a closed reason, one failure row is written,
// and nothing is stored.
func TestAzureFoundryCallback_FailuresAfterTheStartLegRedirectWithAFixedCode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(f *azureFixture)
		want   string
	}{
		{"store gone", func(f *azureFixture) { f.srv.cfg.Store = nil }, azureCaptureRowChanged},
		{"by-uid source gone", func(f *azureFixture) { f.srv.cfg.AzureFoundryEntra = nil }, azureCaptureRowChanged},
		{"site config unreadable", func(f *azureFixture) { f.srv.cfg.Store = brokenSiteStore{} }, reasonStoreError},
		{"by-uid source fails", func(f *azureFixture) {
			f.srv.cfg.AzureFoundryEntra = func(context.Context, string) (ADOEntraConfig, bool, error) {
				return ADOEntraConfig{}, false, errors.New("boom")
			}
		}, reasonStoreError},
		{"by-uid source no longer knows the row", func(f *azureFixture) {
			f.srv.cfg.AzureFoundryEntra = func(context.Context, string) (ADOEntraConfig, bool, error) {
				return ADOEntraConfig{}, false, nil
			}
		}, azureCaptureRowChanged},
		{"application stopped being the login application", func(f *azureFixture) { f.app.LoginTenantID = "" }, azureCaptureRowChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAzureFixture(t)
			subject := f.fake.Subject()
			q, cookies, _ := f.begin(t, subject, azUIDAnthropic)
			tc.break_(f)
			w := f.callback(subject, q, cookies)
			if w.Code != http.StatusFound || w.Header().Get("Location") != azureErrorRedirect(tc.want) {
				t.Fatalf("status %d location %q; want a redirect to %s", w.Code, w.Header().Get("Location"), azureErrorRedirect(tc.want))
			}
			rows := f.audit.find(azureSignInCapturedAction)
			if len(rows) != 1 || rows[0].Outcome != "failure" || !strings.Contains(string(rows[0].Data), tc.want) {
				t.Fatalf("audit rows = %+v; want one failure row naming %s", rows, tc.want)
			}
			if names := f.storedNames(t, subject); len(names) != 0 {
				t.Fatalf("a failed capture stored %v", names)
			}
		})
	}
}

func TestAzureFoundryCallback_RefusesInBandWhatIsNotACompleteReturn(t *testing.T) {
	f := newAzureFixture(t)
	subject := f.fake.Subject()

	t.Run("missing pkce cookie spends nothing", func(t *testing.T) {
		q, cookies, _ := f.begin(t, subject, azUIDAnthropic)
		var kept []*http.Cookie
		for _, c := range cookies {
			if !strings.HasSuffix(c.Name, azurePKCECookieName) {
				kept = append(kept, c)
			}
		}
		w := f.callback(subject, q, kept)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonAzureCallbackCookiesInvalid) {
			t.Fatalf("status %d body %q", w.Code, w.Body.String())
		}
		for _, c := range w.Result().Cookies() {
			if c.MaxAge < 0 {
				t.Errorf("cookie %s was cleared although the request never reached the exchange", c.Name)
			}
		}
	})

	t.Run("no code", func(t *testing.T) {
		q, cookies, _ := f.begin(t, subject, azUIDAnthropic)
		q.Del("code")
		w := f.callback(subject, q, cookies)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonAzureCallbackMissingCode) {
			t.Fatalf("status %d body %q", w.Code, w.Body.String())
		}
	})

	t.Run("a code the authority does not honour", func(t *testing.T) {
		q, cookies, _ := f.begin(t, subject, azUIDAnthropic)
		w := f.callback(subject, url.Values{"state": {q.Get("state")}, "code": {"not-a-real-code"}}, cookies)
		if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), azureFoundrySignInErrorPath) {
			t.Fatalf("status %d location %q; want a redirect to the console error page", w.Code, w.Header().Get("Location"))
		}
		if names := f.storedNames(t, subject); len(names) != 0 {
			t.Fatalf("a refused exchange stored %v", names)
		}
	})
}
