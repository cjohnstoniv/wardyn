// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"
)

// A cached Azure DevOps access token is served only while the person's stored
// sign-in still exists. A disconnect served by another replica deletes the row
// and forgets only that replica's cache; this one must stop serving the token
// it holds rather than keep it until it expires.
func TestADOEntraAccess_CacheHitNeedsTheStoredSignIn(t *testing.T) {
	f := newADOFixture(t)
	subject := f.fake.Subject()
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}
	f.srv.cfg.Secrets = revisionedSecrets{f.srv.cfg.Secrets} // as the Postgres store keeps them
	ctx := context.Background()
	scopes := f.srv.adoRequestScopes(ctx, f.cfg, subject)
	first, err := f.srv.adoEntraAccessFor(ctx, f.cfg, subject, scopes, false)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := f.srv.adoEntraAccessFor(ctx, f.cfg, subject, scopes, false); err != nil || again.AccessToken != first.AccessToken {
		t.Fatalf("second resolve = %q, %v; want the cached token", again.AccessToken, err)
	}

	// Another replica's disconnect: the row goes, this replica's cache is not told.
	if err := f.srv.cfg.Secrets.For(subject).Delete(ctx, adoCapture(f.cfg).secretName); err != nil {
		t.Fatal(err)
	}
	if got, err := f.srv.adoEntraAccessFor(ctx, f.cfg, subject, scopes, false); err == nil || got.AccessToken != "" {
		t.Fatalf("after the sign-in was deleted elsewhere = %q, %v; want no token and an error", got.AccessToken, err)
	}
}
