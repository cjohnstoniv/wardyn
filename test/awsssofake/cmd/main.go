// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command awsssofake serves test/awsssofake's fake sso-oidc + sso portal (and
// its bedrock-runtime stub) on a fixed port, so the SAME fake every in-process
// test uses can run as a POD on the kind SSO cluster.
//
// It is a TEST BINARY, not a product one: it is never built by `make
// agent-images`, never published, and it impersonates AWS with no signing and
// no authentication beyond the bearer contract the real portal enforces. It
// belongs only on a throwaway cluster (deploy/kind/sso/awsssofake.yaml), and
// every credential it mints is a fixture string.
//
// Why a `main` around the existing mux rather than a second implementation: the
// whole value of this path is that the walk exercises the fake whose wire shape
// the docker-gated tests have already proven fools the REAL AWS CLI v2
// (test/awsssofake/docker_test.go). A cluster-only re-implementation would
// prove the walk against a fake nothing validated.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/test/awsssofake"
)

func main() {
	addr := flag.String("addr", envOr("AWSSSOFAKE_ADDR", ":8090"), "listen address")
	// accounts is the ENTITLEMENT fixture, as the JSON this flag's own shape
	// documents: [{"account_id":"222222222222","roles":["WardynDev"]}]. The walk
	// pins the roster to one of these pairs, so the fixture and the pin have to
	// be settable from the same place the manifest sets everything else.
	accounts := flag.String("accounts", envOr("AWSSSOFAKE_ACCOUNTS", ""),
		`entitlement fixture as JSON, e.g. [{"account_id":"222222222222","roles":["WardynDev"]}] (empty = the package default, one account 111111111111/AdministratorAccess)`)
	flag.Parse()

	s, h := awsssofake.NewHandler()
	if strings.TrimSpace(*accounts) != "" {
		var rows []struct {
			AccountID string   `json:"account_id"`
			Roles     []string `json:"roles"`
		}
		if err := json.Unmarshal([]byte(*accounts), &rows); err != nil {
			log.Fatalf("awsssofake: -accounts is not the documented JSON: %v", err)
		}
		if len(rows) == 0 {
			log.Fatalf("awsssofake: -accounts parsed to an empty list; omit the flag for the default fixture")
		}
		out := make([]awsssofake.Account, 0, len(rows))
		for _, r := range rows {
			out = append(out, awsssofake.Account{AccountID: r.AccountID, Roles: r.Roles})
		}
		s.SetAccounts(out)
	}

	// APPROVED UP FRONT, and permanently. The device-code flow normally waits
	// for a human to open verificationUriComplete; in-cluster that URL points at
	// this Service, which no browser on the host can reach (it is not
	// port-forwarded, and must not be — the whole point of the fourth
	// precondition is that the sandbox addresses this fake by its SERVICE name).
	// Nothing here is deciding anything security-relevant: an unsigned fake that
	// mints fixture credentials has no approval to withhold.
	s.Approve()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("awsssofake: serving sso-oidc + sso portal + bedrock-runtime stub on %s", *addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("awsssofake: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
