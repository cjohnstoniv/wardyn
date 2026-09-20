// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

func TestFSStore_ReadsStayWithinRoot(t *testing.T) {
	for _, relative := range []bool{false, true} {
		for _, key := range []string{"run", "run~session"} {
			name := "absolute/" + key
			if relative {
				name = "relative/" + key
			}
			t.Run(name, func(t *testing.T) {
				parent := t.TempDir()
				root := filepath.Join(parent, "recordings")
				store, err := recording.NewFSStore(root)
				if err != nil {
					t.Fatal(err)
				}
				const fixture = "synthetic non-recording fixture"
				target := filepath.Join(parent, "fixture.txt")
				if err := os.WriteFile(target, []byte(fixture), 0o600); err != nil {
					t.Fatal(err)
				}
				if relative {
					target = filepath.Join("..", "fixture.txt")
				}
				if err := os.Symlink(target, filepath.Join(root, key+".cast")); err != nil {
					t.Fatal(err)
				}

				rc, err := store.OpenCast(t.Context(), key)
				if rc != nil {
					_ = rc.Close()
				}
				if err == nil {
					t.Error("OpenCast accepted a symlink outside the recording root")
				}
				if _, tail, err := store.StatAndTail(t.Context(), key, 128); err == nil || len(tail) != 0 {
					t.Errorf("StatAndTail must refuse the path without returning bytes: tail=%q, err=%v", tail, err)
				}
				res := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run/recording/"+key, nil)
				newTestRouter(store).ServeHTTP(res, req)
				if res.Code != http.StatusInternalServerError || strings.Contains(res.Body.String(), fixture) {
					t.Errorf("replay must fail closed: status=%d body=%q", res.Code, res.Body.String())
				}
			})
		}
	}
}

func TestFSStore_RootedReadCompatibility(t *testing.T) {
	root := t.TempDir()
	store, err := recording.NewFSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	const cast = "{\"version\":2}\n"
	if err := store.SaveCastNamed(t.Context(), "run", "session", strings.NewReader(cast)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("run~session.cast", filepath.Join(root, "alias.cast")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"run~session", "alias"} {
		t.Run(key, func(t *testing.T) { assertRootedCast(t, store, key, cast) })
	}
	if _, err := store.OpenCast(t.Context(), "missing"); !errors.Is(err, recording.ErrNotFound) {
		t.Errorf("missing OpenCast error = %v", err)
	}
	if _, _, err := store.StatAndTail(t.Context(), "missing", 128); !errors.Is(err, recording.ErrNotFound) {
		t.Errorf("missing StatAndTail error = %v", err)
	}
}

func assertRootedCast(t *testing.T, store *recording.FSStore, key, want string) {
	t.Helper()
	rc, err := store.OpenCast(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil || string(got) != want {
		t.Fatalf("OpenCast = %q, %v; want %q", got, err, want)
	}
	size, tail, err := store.StatAndTail(t.Context(), key, 128)
	if err != nil || size != int64(len(want)) || string(tail) != want {
		t.Fatalf("StatAndTail = %d, %q, %v; want %q", size, tail, err, want)
	}
}
