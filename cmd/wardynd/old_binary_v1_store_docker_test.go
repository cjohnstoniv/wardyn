// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

const (
	// oldWardynd is the last published release that predates envelope v1.
	oldWardynd = "ghcr.io/cjohnstoniv/wardynd:0.7.11@sha256:c3e40037a1518e5990dd66429af5ea7445e5915dc7db0d5c7a137f687d2c3249"
	// oldBinaryRefusal is how it fails over a v1 row: age finds no header
	// because there is none. age 1.3.1 then says "file is empty" when the
	// ciphertext holds no newline byte, or "unexpected intro:" and quotes the
	// bytes before the first one — which it is depends on the random nonce.
	oldBinaryRefusal = `load secret "wardyn-signing-key": pg secretstore: decrypt wardyn-signing-key: age decrypt: failed to read header: parsing age header:`
)

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestPG_OldBinaryAgainstV1Store: an operator who rolls back to 0.7.11 over a
// database this version converted gets a boot that FAILS CLOSED — the old
// binary reads no row, mints nothing over the boot keys, and exits — and the
// line it fails with is the one docs/OPERATIONS.md tells them means "restore
// the pre-upgrade dump", not "your age key is wrong". The old binary cannot
// name the envelope itself; the runbook is where that line is explained.
//
// Self-contained on the default docker daemon: its own postgres:17 and the
// published 0.7.11 image on a private network, so it needs no host networking.
func TestPG_OldBinaryAgainstV1Store(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping the published 0.7.11 wardynd case")
	}
	suffix := uuid.NewString()[:8]
	network, pgName := "t14-net-"+suffix, "t14-pg-"+suffix
	dockerCLI(t, "network", "create", network)
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	dockerCLI(t, "run", "-d", "--name", pgName, "--network", network,
		"-e", "POSTGRES_PASSWORD=wardyn", "-p", "127.0.0.1::5432", "postgres:17")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", pgName).Run() })
	hostPort := dockerCLI(t, "port", pgName, "5432/tcp")
	hostPort = strings.Split(hostPort, "\n")[0]

	ctx := t.Context()
	pool := waitForPG(t, "postgres://postgres:wardyn@"+hostPort+"/postgres?sslmode=disable")
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	id, _ := age.GenerateX25519Identity()
	secrets, err := buildSecretStore(ctx, pool, id.String(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSigningKey(ctx, secrets); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateSessionKey(ctx, secrets); err != nil {
		t.Fatal(err)
	}
	before := sealedRows(t, pool)

	dsn := url.URL{Scheme: "postgres", User: url.UserPassword("postgres", "wardyn"), Host: pgName + ":5432", Path: "/postgres", RawQuery: "sslmode=disable"}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(rctx, "docker", "run", "--rm", "--network", network,
		"-e", "WARDYN_PG_DSN="+dsn.String(), "-e", "WARDYN_AGE_KEY="+id.String(), oldWardynd).CombinedOutput()
	if rctx.Err() != nil {
		t.Fatalf("0.7.11 wardynd was still running after 3m over a v1 store; it must refuse to boot:\n%s", out)
	}
	if err == nil {
		t.Fatalf("0.7.11 wardynd exited 0 over a v1 store:\n%s", out)
	}
	_, fatal, _ := strings.Cut(string(out), "ERROR wardynd: fatal err=")
	fatal, _, _ = strings.Cut(fatal, "\n")
	msg, err := strconv.Unquote(fatal)
	tail, ok := strings.CutPrefix(msg, oldBinaryRefusal)
	if err != nil || !ok || (tail != " file is empty" && !strings.HasPrefix(tail, " unexpected intro: ")) {
		t.Errorf("0.7.11 wardynd did not fail with %q:\n%s", oldBinaryRefusal, out)
	}
	ops, err := os.ReadFile("../../docs/OPERATIONS.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{oldBinaryRefusal, "`file is empty`", "`unexpected intro: \"…\"`"} {
		if !strings.Contains(string(ops), want) {
			t.Errorf("docs/OPERATIONS.md does not carry %q; an operator who rolls back reads the refusal as a wrong age key", want)
		}
	}
	if got := sealedRows(t, pool); !maps.Equal(got, before) {
		t.Fatal("0.7.11 wardynd changed stored secrets while refusing to boot")
	}
}

func waitForPG(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		pool, err := db.Connect(t.Context(), dsn)
		if err == nil {
			t.Cleanup(pool.Close)
			return pool
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres never came up: %v", err)
		}
		time.Sleep(time.Second)
	}
}
