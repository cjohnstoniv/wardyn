// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestOrgRunnerMethodsHitTheirRoutes(t *testing.T) {
	id := uuid.New()
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+string(b))
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	c := client.New(srv.URL, "tok")
	ctx := context.Background()
	if _, err := c.MintOrgRunnerToken(ctx, "cluster-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ClaimOrgRunner(ctx, id, "fp"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetRunnerTrust(ctx, id, types.RunnerTrustManagedDevice, "enrolled in MDM"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`POST /api/v1/runners/org-tokens {"name":"cluster-a"}`,
		`POST /api/v1/runners/` + id.String() + `/claim-org {"fingerprint":"fp"}`,
		`PUT /api/v1/runners/` + id.String() + `/trust {"trust_class":"managed_device","reason":"enrolled in MDM"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("requests %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %q, want %q", i, got[i], want[i])
		}
	}
}
