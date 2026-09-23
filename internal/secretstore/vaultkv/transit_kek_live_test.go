// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vaultkv

// The KEK contract against a real Vault or OpenBao dev server (T-33),
// alongside TestLive_Transit. Besides WARDYN_TEST_VAULT and its admin token:
// WARDYN_TEST_VAULT_CONTAINER names the server's container, which the outage
// cases pause with `docker pause`; WARDYN_TEST_VAULT_AUDIT_FILE is the log of
// a file audit device on that server, readable here. scripts/kek-conformance.sh
// starts both servers with all of it and sets the variables.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek/kektest"
)

// outageBound is how long a call against a paused server may take to come
// back transient: the client's default timeout and retries, well inside the
// sink's 15-minute grace. A hang past it would stall the sink instead.
const outageBound = 60 * time.Second

// on is the admin failing t, for use inside a subtest.
func (a liveAdmin) on(t *testing.T) liveAdmin {
	a.t = t
	return a
}

// liveTransitKey mounts a Transit engine of the test's own with an
// aes256-gcm96 key "wardyn", and returns a config logging in as a child token
// that holds only the documented policy: update on encrypt/ and decrypt/.
func liveTransitKey(t *testing.T, a liveAdmin) (string, Config) {
	t.Helper()
	mount := "wardyn-kek-" + uuid.NewString()[:8]
	a.must(http.MethodPost, "sys/mounts/"+mount, map[string]any{"type": "transit"})
	t.Cleanup(func() { a.do(http.MethodDelete, "sys/mounts/"+mount, nil) })
	a.must(http.MethodPost, mount+"/keys/wardyn", map[string]any{"type": "aes256-gcm96"})
	policy := fmt.Sprintf(`path "%[1]s/encrypt/wardyn" { capabilities = ["update"] }
path "%[1]s/decrypt/wardyn" { capabilities = ["update"] }
`, mount)
	a.must(http.MethodPut, "sys/policies/acl/"+mount, map[string]any{"policy": policy})
	t.Cleanup(func() { a.do(http.MethodDelete, "sys/policies/acl/"+mount, nil) })
	return mount, Config{Addr: a.addr, Auth: AuthTokenFile, TokenFile: writeFile(t, a.childToken(mount, "1h"))}
}

func openLiveTransit(t *testing.T, cfg Config, mount string) *Transit {
	t.Helper()
	tr, err := NewTransit(t.Context(), cfg, mount, "wardyn")
	if err != nil {
		t.Fatalf("NewTransit against the live server: %v", err)
	}
	return tr
}

func liveContainer(t *testing.T) string {
	c := os.Getenv("WARDYN_TEST_VAULT_CONTAINER")
	if c == "" {
		t.Skip("WARDYN_TEST_VAULT_CONTAINER not set; skipping the live outage case")
	}
	return c
}

// pause freezes the server's container and returns the func that thaws it;
// the test's cleanup thaws it too, so a failure never leaves it frozen.
func pause(t *testing.T, container string) func() {
	t.Helper()
	if out, err := exec.Command("docker", "pause", container).CombinedOutput(); err != nil {
		t.Fatalf("docker pause %s: %v: %s", container, err, out)
	}
	var once sync.Once
	restore := func() {
		once.Do(func() {
			if out, err := exec.Command("docker", "unpause", container).CombinedOutput(); err != nil {
				t.Errorf("docker unpause %s: %v: %s", container, err, out)
			}
		})
	}
	t.Cleanup(restore)
	return restore
}

// TestLive_TransitKEKConformance holds live Transit to the kektest contract:
// the associated_data binding, a rotation and min_decryption_version, a paused
// server (transient) and a deleted key (definitive).
func TestLive_TransitKEKConformance(t *testing.T) {
	a := liveAdminEnv(t)
	mount, cfg := liveTransitKey(t, a)
	hooks := kektest.Hooks{
		Rotate: func(t *testing.T) { a.on(t).must(http.MethodPost, mount+"/keys/wardyn/rotate", nil) },
		Retire: func(t *testing.T, n int) {
			a.on(t).must(http.MethodPost, mount+"/keys/wardyn/config", map[string]any{"min_decryption_version": n})
		},
		Disable: func(t *testing.T) {
			a := a.on(t)
			a.must(http.MethodPost, mount+"/keys/wardyn/config", map[string]any{"deletion_allowed": true})
			a.must(http.MethodDelete, mount+"/keys/wardyn", nil)
		},
	}
	if c := os.Getenv("WARDYN_TEST_VAULT_CONTAINER"); c != "" {
		hooks.Unreachable = func(t *testing.T) func() { return pause(t, c) }
	}
	kektest.Run(t, func(t *testing.T) kek.KEK { return openLiveTransit(t, cfg, mount) }, hooks)
}

// Boot refuses while the key service is unreachable, within outageBound, and
// comes up once it answers again.
func TestLive_TransitBootRefusedWhileUnreachable(t *testing.T) {
	a := liveAdminEnv(t)
	c := liveContainer(t)
	mount, cfg := liveTransitKey(t, a)
	restore := pause(t, c)
	start := time.Now()
	_, err := NewTransit(t.Context(), cfg, mount, "wardyn")
	took := time.Since(start)
	restore()
	if err == nil {
		t.Fatal("NewTransit booted with the key service paused")
	}
	if took > outageBound {
		t.Fatalf("the boot refusal took %s; want it within %s", took, outageBound)
	}
	openLiveTransit(t, cfg, mount)
}

// A read of a stored credential while the key service is paused is transient
// (the sink's 503, SINK.KEK_UNREACHABLE), never not-found or a refusal, and
// arrives within outageBound.
func TestLive_TransitStoreOutageIsTransient(t *testing.T) {
	a := liveAdminEnv(t)
	c := liveContainer(t)
	pool := throwawayDB(t)
	mount, cfg := liveTransitKey(t, a)
	s := pgStore(t, pool, nil, openLiveTransit(t, cfg, mount), true)
	ctx := t.Context()
	if err := s.For("alice@example.com").Put(ctx, "pat", []byte("v")); err != nil {
		t.Fatal(err)
	}
	restore := pause(t, c)
	start := time.Now()
	_, err := s.For("alice@example.com").Get(ctx, "pat")
	took := time.Since(start)
	restore()
	if !errors.Is(err, secretstore.ErrUnavailable) || errors.Is(err, secretstore.ErrNotFound) {
		t.Fatalf("Get with the key service paused = %v; want ErrUnavailable", err)
	}
	if took > outageBound {
		t.Fatalf("the transient answer took %s; want it within %s", took, outageBound)
	}
	if v, err := s.For("alice@example.com").Get(ctx, "pat"); err != nil || string(v) != "v" {
		t.Fatalf("Get once the key service is back = (%q, %v)", v, err)
	}
}

// -rewrap against live Transit: a run that aborts at an unreadable row keeps
// the rows before it, a re-run moves only the rest, and a third moves
// nothing. Then min_decryption_version retires v1 with every row readable.
func TestLive_TransitRewrapIsResumableAndIdempotent(t *testing.T) {
	a := liveAdminEnv(t)
	pool := throwawayDB(t)
	mount, cfg := liveTransitKey(t, a)
	tr := openLiveTransit(t, cfg, mount)
	s := pgStore(t, pool, nil, tr, true)
	ctx := t.Context()
	for _, n := range []string{"a", "b", "c"} {
		if err := s.Put(ctx, n, []byte("v-"+n)); err != nil {
			t.Fatal(err)
		}
	}
	a.must(http.MethodPost, mount+"/keys/wardyn/rotate", nil)

	_, good := rowKEK(t, pool, "", "b")
	bad := flipPayload(good) // Transit refuses it: the GCM tag no longer matches
	setWrap := func(w []byte) {
		if _, err := pool.Exec(ctx, `UPDATE secrets SET wrapped_dek=$1 WHERE owned_by='' AND name='b'`, w); err != nil {
			t.Fatal(err)
		}
	}
	setWrap(bad)
	res, err := s.Rewrap(ctx)
	if err == nil || !strings.Contains(err.Error(), `name="b"`) || res.Rewrapped != 1 {
		t.Fatalf("Rewrap over an unreadable row = (%+v, %v); want an abort naming b after 1 row", res, err)
	}
	setWrap(good)
	if res, err = s.Rewrap(ctx); err != nil || res.Rewrapped != 2 || res.KeyVersion != 2 {
		t.Fatalf("re-run = (%+v, %v); want the 2 rows left, to v2", res, err)
	}
	if res, err = s.Rewrap(ctx); err != nil || res.Rewrapped != 0 {
		t.Fatalf("third run = (%+v, %v); want nothing to move", res, err)
	}
	a.must(http.MethodPost, mount+"/keys/wardyn/config", map[string]any{"min_decryption_version": 2})
	for _, n := range []string{"a", "b", "c"} {
		_, w := rowKEK(t, pool, "", n)
		if v, _ := tr.WrapVersion(w); v != 2 {
			t.Errorf("row %s is under v%d after the rewrap, want v2", n, v)
		}
		if v, err := s.Get(ctx, n); err != nil || string(v) != "v-"+n {
			t.Fatalf("Get(%s) with v1 retired = (%q, %v)", n, v, err)
		}
	}
}

// Every read of a stored credential is one Transit decrypt in the server's
// audit log: nothing caches a data key and hides a read from it.
func TestLive_TransitAuditsEveryUnwrap(t *testing.T) {
	a := liveAdminEnv(t)
	log := os.Getenv("WARDYN_TEST_VAULT_AUDIT_FILE")
	if log == "" {
		t.Skip("WARDYN_TEST_VAULT_AUDIT_FILE not set; skipping the live audit case")
	}
	pool := throwawayDB(t)
	mount, cfg := liveTransitKey(t, a)
	s := pgStore(t, pool, nil, openLiveTransit(t, cfg, mount), true)
	ctx := t.Context()
	if err := s.Put(ctx, "k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	before := auditedDecrypts(t, log, mount)
	const reads = 3
	for range reads {
		if _, err := s.Get(ctx, "k"); err != nil {
			t.Fatal(err)
		}
	}
	if got := auditedDecrypts(t, log, mount) - before; got != reads {
		t.Fatalf("%d reads left %d decrypts in the audit log; want one each", reads, got)
	}
}

// auditedDecrypts counts the successful decrypt responses under mount in a
// file audit device's log.
func auditedDecrypts(t *testing.T, path, mount string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	n := 0
	for sc.Scan() {
		var e struct {
			Type    string `json:"type"`
			Error   string `json:"error"`
			Request struct {
				Path string `json:"path"`
			} `json:"request"`
		}
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Type == "response" && e.Error == "" && e.Request.Path == mount+"/decrypt/wardyn" {
			n++
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

// flipPayload changes the first byte of a "vault:vN:<base64>" wrap's payload.
func flipPayload(w []byte) []byte {
	i := strings.LastIndexByte(string(w), ':') + 1
	out := []byte(string(w))
	if out[i] == 'A' {
		out[i] = 'B'
	} else {
		out[i] = 'A'
	}
	return out
}
