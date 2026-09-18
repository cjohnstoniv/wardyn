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
	"strconv"
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
	// The 0.7.6 re-auth knobs. Durations, so a manifest can say "12m" rather
	// than a second unit nobody can read back.
	tokenTTL := flag.Duration("token-ttl", envDuration("AWSSSOFAKE_TOKEN_TTL"),
		"lifetime CreateToken advertises for the access token (0 = the 3600s default). 12m is what the mid-run re-auth walk uses: the hold is reachable only when the proxy re-resolves, inside injectRefreshMargin of expiry")
	roleCredTTL := flag.Duration("role-cred-ttl", envDuration("AWSSSOFAKE_ROLE_CRED_TTL"),
		"lifetime of each GetRoleCredentials answer, stamped PER CALL (0 = one absolute expiry fixed at construction). 3m is what the walk uses: it makes the sandbox SDK re-call portal.sso at roughly T+3/6/9")
	reauthAfter := flag.Int("reauth-after", envInt("AWSSSOFAKE_REAUTH_AFTER"),
		"retire the session on the Nth REFRESH redemption: CreateToken then answers invalid_grant, the shape a consumed grant really has (0 = never). Also settable at runtime: POST /_control/reauth?after=N")
	tlsCert := flag.String("tls-cert", envOr("AWSSSOFAKE_TLS_CERT", ""),
		"serve HTTPS with this certificate (PEM). With -tls-key, it makes the fake's lane PRODUCTION-SHAPED: a TLS CONNECT the proxy terminates, so header injection, CA trust and the timeout body are exercised instead of simulated")
	tlsKey := flag.String("tls-key", envOr("AWSSSOFAKE_TLS_KEY", ""), "private key (PEM) for -tls-cert")
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
	s.SetTokenTTL(*tokenTTL)
	s.SetRoleCredTTL(*roleCredTTL)
	s.SetReauthAfter(*reauthAfter)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	// TLS when the operator supplied a pair, because the PLAINTEXT lane cannot
	// prove the production shape: injection errors are swallowed on the plain
	// forward lane (the SDK sees the fake's own 401), so the timeout's 401
	// UnauthorizedException body and the MITM path that writes it are only
	// exercised over a TLS CONNECT the proxy terminates.
	if *tlsCert != "" || *tlsKey != "" {
		if *tlsCert == "" || *tlsKey == "" {
			log.Fatalf("awsssofake: -tls-cert and -tls-key must be given together")
		}
		log.Printf("awsssofake: serving sso-oidc + sso portal + bedrock-runtime stub over TLS on %s", *addr)
		if err := srv.ListenAndServeTLS(*tlsCert, *tlsKey); err != nil {
			log.Fatalf("awsssofake: %v", err)
		}
		return
	}
	log.Printf("awsssofake: serving sso-oidc + sso portal + bedrock-runtime stub on %s", *addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("awsssofake: %v", err)
	}
}

// envDuration reads a duration knob; an unparseable value is 0 (the default),
// because a test fake must not refuse to start over a typo in a manifest.
func envDuration(key string) time.Duration {
	d, err := time.ParseDuration(os.Getenv(key))
	if err != nil || d < 0 {
		return 0
	}
	return d
}

func envInt(key string) int {
	n, err := strconv.Atoi(os.Getenv(key))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
