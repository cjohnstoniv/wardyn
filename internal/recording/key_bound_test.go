// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// countingStore records every store call and finds nothing.
type countingStore struct{ calls []string }

func (c *countingStore) SaveCast(_ context.Context, key string, _ io.Reader) error {
	c.calls = append(c.calls, "save "+key)
	return recording.ErrNotFound
}

func (c *countingStore) SaveCastNamed(_ context.Context, run, suffix string, _ io.Reader) error {
	c.calls = append(c.calls, "save "+recording.CastKey(run, suffix))
	return recording.ErrNotFound
}

func (c *countingStore) OpenCast(_ context.Context, key string) (io.ReadCloser, error) {
	c.calls = append(c.calls, "open "+key)
	return nil, recording.ErrNotFound
}

func (c *countingStore) StatAndTail(_ context.Context, key string, _ int64) (int64, []byte, error) {
	c.calls = append(c.calls, "stat "+key)
	return 0, nil, recording.ErrNotFound
}

// maxKey pads run with "~a" segments to exactly n bytes: the most fence
// ancestors a key of that length can carry.
func maxKey(run string, n int) string {
	key := run + strings.Repeat("~a", (n-len(run))/2)
	return key + strings.Repeat("a", n-len(key))
}

func TestHandler_OverlongKeyNotFoundWithoutStoreCall(t *testing.T) {
	const run = "0a000000-0000-4000-8000-000000000011"
	for _, tc := range []struct {
		key       string
		storeCall bool
	}{
		{maxKey(run, 251), false},
		{run + "~" + strings.Repeat("~", 4096), false},
		{maxKey(run, 250), true}, // the bound itself still reaches the store
	} {
		store := &countingStore{}
		authorized := false
		authorize := func(*http.Request, string) bool { authorized = true; return true }
		srv := httptest.NewServer(newTestRouterWithAuth(store, authorize))
		resp, err := http.Get(srv.URL + "/api/v1/runs/" + run + "/recording/" + tc.key)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%d-byte key: status %d, want 404", len(tc.key), resp.StatusCode)
		}
		if got := len(store.calls) > 0; got != tc.storeCall || authorized != tc.storeCall {
			t.Errorf("%d-byte key: store calls %d, authorized %v; want reached=%v", len(tc.key), len(store.calls), authorized, tc.storeCall)
		}
	}
}

// keyBoundContract: every supported spelling, up to the bound, round-trips and
// is fenced by its run's erasure; one byte more is refused by every door.
func keyBoundContract(t *testing.T, s recording.Store, empty func() bool) {
	t.Helper()
	run := uuid.NewString()
	keys := []string{
		recording.CastKey(run, "ssh-"+uuid.NewString()), // SSH session: 77 bytes, the longest written
		recording.CastKey(run, uuid.NewString()),        // attach session
		recording.CastKey(run, recording.PartSuffix(2048)),
		maxKey(run, 250),
	}
	for _, key := range keys {
		if err := s.SaveCast(t.Context(), key, strings.NewReader("cast")); err != nil {
			t.Fatalf("save %d-byte key: %v", len(key), err)
		}
		if size, _, err := s.StatAndTail(t.Context(), key, 8); err != nil || size != 4 {
			t.Fatalf("stat %d-byte key = %d, %v", len(key), size, err)
		}
	}
	if _, err := s.(recording.RunDeleter).DeleteRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		assertErased(t, s, key)
	}

	other := uuid.NewString()
	long := maxKey(other, 251)
	if !empty() {
		t.Fatal("store not empty before the refused calls")
	}
	if err := s.SaveCast(t.Context(), long, strings.NewReader("x")); err == nil {
		t.Error("SaveCast accepted a 251-byte key")
	}
	if err := s.SaveCastNamed(t.Context(), other, strings.Repeat("s", 250-len(other)), strings.NewReader("x")); err == nil {
		t.Error("SaveCastNamed accepted a 251-byte key")
	}
	if _, err := s.OpenCast(t.Context(), long); err == nil || errors.Is(err, recording.ErrNotFound) {
		t.Errorf("OpenCast(251-byte key) = %v, want a refusal", err)
	}
	if _, _, err := s.StatAndTail(t.Context(), long, 8); err == nil || errors.Is(err, recording.ErrNotFound) {
		t.Errorf("StatAndTail(251-byte key) = %v, want a refusal", err)
	}
	if _, err := s.(recording.RunDeleter).DeleteRun(t.Context(), long); err == nil {
		t.Error("DeleteRun accepted a 251-byte key")
	}
	if !empty() {
		t.Error("a refused key left state behind")
	}
}

func TestFSStore_KeyBound(t *testing.T) {
	root := t.TempDir()
	s, _ := recording.NewFSStore(root)
	keyBoundContract(t, s, func() bool {
		// Only the first run's files exist: its casts went with the erasure;
		// its lock and fence stay by design.
		ents, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		return len(ents) == 2 // <run>.lock and .erased
	})
}

func TestPGStore_KeyBound(t *testing.T) {
	pool := pgPool(t)
	count := func() (n int) {
		if err := pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM recordings) + (SELECT count(*) FROM recording_erasures)`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := -1
	keyBoundContract(t, recording.NewPGStore(pool), func() bool {
		if before < 0 {
			before = count()
		}
		return count() == before
	})
}

// A PGStore with no pool shows the refusal comes before any query is built.
func TestPGStore_KeyBoundBeforePool(t *testing.T) {
	s := recording.NewPGStore(nil)
	long := maxKey("run", 251)
	for name, call := range map[string]func() error{
		"save": func() error { return s.SaveCast(t.Context(), long, strings.NewReader("x")) },
		"named": func() error {
			return s.SaveCastNamed(t.Context(), "run", strings.Repeat("s", 247), strings.NewReader("x"))
		},
		"open":  func() error { _, err := s.OpenCast(t.Context(), long); return err },
		"stat":  func() error { _, _, err := s.StatAndTail(t.Context(), long, 8); return err },
		"erase": func() error { _, err := s.DeleteRun(t.Context(), long); return err },
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s with a 251-byte key reached the pool: %v", name, r)
				}
			}()
			if err := call(); err == nil {
				t.Errorf("%s accepted a 251-byte key", name)
			}
		}()
	}
}
