// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// brokenAuth is an auth file that became unreadable after boot.
type brokenAuth struct{}

func (brokenAuth) Lookup(string) (RegistryAuth, bool, error) {
	return RegistryAuth{}, false, errors.New("registry auth file: permission denied")
}

// A registry auth file that cannot be read when a registry asks for a credential is a fault on this side, not
// evidence that the image is unsigned: every read path fails the check as unavailable (never kept), rather than
// skipping a tag or referrer and keeping a refusal.
func TestAuthFileFaultIsRegistryDown(t *testing.T) {
	subject := sum(imageBody)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/team/app/referrers/") { // anonymous, so the referrer list is read
			serveManifest(w, indexJSON(t, 1, r.URL.Query().Get("artifactType")), ociIndexType, "")
			return
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="reg"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := newTestClient(t, brokenAuth{}, testGuard(), nil, srv)
	repo := mustRef(t, host(srv)+"/team/app")
	ctx := context.Background()

	_, err := c.LegacyTag(ctx, repo, subject, ".sig")
	var u *Unavailable
	if !errors.As(err, &u) || u.Cause != CauseAuthFile || !registryDown(err) {
		t.Fatalf("legacy tag read: %v, want an auth-file Unavailable", err)
	}
	if got := unavailableDetail(repo, err); got != "WARDYN_REGISTRY_AUTH_FILE could not be read" {
		t.Errorf("detail = %q", got)
	}
	if _, err := ReadLegacy(ctx, c, repo, subject); !registryDown(err) {
		t.Errorf("ReadLegacy: %v, want the check to fail, not the tag skipped", err)
	}
	if _, err := NewAdmitter(c, AdmitterOptions{}).bundles(ctx, repo, subject); !registryDown(err) {
		t.Errorf("referrer read: %v, want the check to fail, not the referrer skipped", err)
	}
}
