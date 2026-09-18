// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	secretpg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const sourceDB = "wardyn_review_ops_source_20260918"
const restoreDB = "wardyn_review_ops_restore_20260918"
const artifactDir = "local/review-ops"
const syntheticSecret = "ops-review-synthetic-secret-not-a-real-credential"
const syntheticCast = "{\"version\":2,\"width\":80,\"height\":24}\n[0.1,\"o\",\"restore rehearsal\\r\\n\"]\n"

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func docker(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "docker", append([]string{"--host", "unix:///var/run/docker.sock", "exec", "-i", "wardyn-review-07x-pg"}, args...)...)
}

func connect(ctx context.Context, name string) *pgxpool.Pool {
	p, err := db.Connect(ctx, "postgres://wardyn:wardyn@localhost:58432/"+name+"?sslmode=disable")
	must(err)
	return p
}

func appendEvent(ctx context.Context, p *pgxpool.Pool, action string) types.AuditEvent {
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem,
		Actor: "ops-review", Action: action, Outcome: "success", Data: json.RawMessage(`{"z":1,"a":{"n":2}}`)}
	must(store.InsertAuditEvent(ctx, p, &ev))
	if ev.RowHash == "" {
		panic("audit insertion returned no chain hash")
	}
	return ev
}

func chain(ctx context.Context, p *pgxpool.Pool) store.AuditChainStatus {
	st, err := store.NewPG(p).VerifyAuditChain(ctx)
	must(err)
	if !st.OK {
		panic(fmt.Sprintf("chain failed: %+v", st))
	}
	return st
}

func migrationCount(ctx context.Context, p *pgxpool.Pool) int {
	var count int
	must(p.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count))
	return count
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, name := range []string{sourceDB, restoreDB} {
		cmd := docker(ctx, "createdb", "-U", "wardyn", name)
		cmd.Stderr = os.Stderr
		must(cmd.Run())
	}
	source := connect(ctx, sourceDB)
	defer source.Close()
	must(db.Migrate(ctx, source))
	count := migrationCount(ctx, source)
	must(db.Migrate(ctx, source))
	if migrationCount(ctx, source) != count {
		panic("repeat migration changed migration count")
	}
	fmt.Printf("fresh_migrations=%d idempotent=true\n", count)
	for _, action := range []string{"ops.seed", "ops.backup", "ops.verify"} {
		appendEvent(ctx, source, action)
	}
	before := chain(ctx, source)
	if before.Checked != 3 || before.Legacy != 0 {
		panic("unexpected seed chain size")
	}

	key, err := age.GenerateX25519Identity()
	must(err)
	secrets, err := secretpg.New(source, key)
	must(err)
	must(secrets.Put(ctx, "ops-review-secret", []byte(syntheticSecret)))
	runID := uuid.NewString()
	must(recording.NewPGStore(source).SaveCast(ctx, runID, strings.NewReader(syntheticCast)))

	dumpPath := artifactDir + "/ops-backup.sql"
	dump, err := os.OpenFile(dumpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	must(err)
	cmd := docker(ctx, "pg_dump", "-U", "wardyn", sourceDB)
	cmd.Stdout, cmd.Stderr = dump, os.Stderr
	must(cmd.Run())
	must(dump.Close())
	dump, err = os.Open(dumpPath)
	must(err)
	cmd = docker(ctx, "psql", "-U", "wardyn", "-v", "ON_ERROR_STOP=1", "-q", restoreDB)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = dump, os.Stdout, os.Stderr
	must(cmd.Run())
	must(dump.Close())
	restored := connect(ctx, restoreDB)
	defer restored.Close()
	after := chain(ctx, restored)
	if after != before {
		panic(fmt.Sprintf("restore changed chain: before=%+v after=%+v", before, after))
	}
	if migrationCount(ctx, restored) != count {
		panic("restore lost migration tracking")
	}
	must(db.Migrate(ctx, restored))
	if chain(ctx, restored) != before {
		panic("boot migrations altered restored chain")
	}
	readSecrets, err := secretpg.New(restored, key)
	must(err)
	plain, err := readSecrets.Get(ctx, "ops-review-secret")
	must(err)
	if string(plain) != syntheticSecret {
		panic("restored secret differs")
	}
	wrongKey, err := age.GenerateX25519Identity()
	must(err)
	wrongStore, err := secretpg.New(restored, wrongKey)
	must(err)
	if _, err := wrongStore.Get(ctx, "ops-review-secret"); err == nil {
		panic("wrong age key decrypted restored secret")
	}
	cast, err := recording.NewPGStore(restored).OpenCast(ctx, runID)
	must(err)
	payload, err := io.ReadAll(cast)
	must(err)
	must(cast.Close())
	if !bytes.Equal(payload, []byte(syntheticCast)) {
		panic("restored recording differs")
	}
	for _, sql := range []string{"UPDATE audit_events SET action='ops.tamper'", "DELETE FROM audit_events", "TRUNCATE audit_events"} {
		tx, err := restored.Begin(ctx)
		must(err)
		_, writeErr := tx.Exec(ctx, sql)
		must(tx.Rollback(ctx))
		if writeErr == nil || !strings.Contains(writeErr.Error(), "append-only") {
			panic(fmt.Sprintf("guard did not refuse %s: %v", sql, writeErr))
		}
	}
	next := appendEvent(ctx, restored, "ops.after_restore")
	if next.PrevHash != before.HeadHash {
		panic("new audit row did not link to restored head")
	}
	final := chain(ctx, restored)
	if final.Checked != before.Checked+1 {
		panic("post-restore audit row missing")
	}
	result := map[string]any{"source_db": sourceDB, "restored_db": restoreDB, "migration_count": count,
		"before": before, "restored": after, "after_append": final, "secret_decrypt": true,
		"wrong_key_refused": true, "pg_recording_identical": true, "append_only_guards": true,
		"restored_migrate": true, "source_unchanged": chain(ctx, source) == before}
	encoded, err := json.MarshalIndent(result, "", "  ")
	must(err)
	must(os.WriteFile(artifactDir+"/ops-results.json", append(encoded, '\n'), 0600))
	fmt.Println(string(encoded))
}
