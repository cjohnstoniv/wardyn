// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

func replay(h http.Handler, outer, key string) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+url.PathEscape(outer)+"/recording/"+url.PathEscape(key), nil))
	return w.Code
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// Every spelling uuid.Parse accepts other than the canonical one is refused
// before the store, so the fs store takes no lock file for it; the outer id
// still binds case-insensitively.
func TestHandler_NonCanonicalRunIDNotFoundWithoutLock(t *testing.T) {
	const run = "0a0000ab-0000-4000-8000-0000000000cd"
	upper := strings.ToUpper(run)
	session, ssh := uuid.NewString(), "ssh-"+uuid.NewString()
	root := t.TempDir()
	fs, err := recording.NewFSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	const cast = `{"version":2,"width":80,"height":24}` + "\n"
	canonical := []string{run, recording.CastKey(run, session), recording.CastKey(run, ssh), recording.CastKey(run, recording.PartSuffix(2))}
	for _, key := range canonical {
		if err := fs.SaveCast(t.Context(), key, strings.NewReader(cast)); err != nil {
			t.Fatal(err)
		}
	}
	before := dirNames(t, root)

	counting := &countingStore{}
	for _, alias := range []string{upper, "urn:uuid:" + run, "URN:UUID:" + upper, "{" + run + "}", strings.ReplaceAll(run, "-", "")} {
		for _, key := range []string{alias, alias + "~" + session, alias + "~" + ssh, alias + "~part-2"} {
			for name, h := range map[string]http.Handler{"fs": newTestRouter(fs), "double": newTestRouter(counting)} {
				if code := replay(h, alias, key); code != http.StatusNotFound {
					t.Errorf("%s: GET %q under outer %q = %d, want 404", name, key, alias, code)
				}
			}
		}
	}
	if len(counting.calls) != 0 {
		t.Errorf("aliases reached the store: %q", counting.calls)
	}
	if after := dirNames(t, root); !slices.Equal(after, before) {
		t.Errorf("aliases changed the recording root:\nbefore %q\nafter  %q", before, after)
	}

	h := newTestRouter(fs)
	for _, key := range canonical {
		for _, outer := range []string{run, upper} {
			if code := replay(h, outer, key); code != http.StatusOK {
				t.Errorf("GET canonical %q under outer %q = %d, want 200", key, outer, code)
			}
		}
	}
	var locks []string
	for _, name := range dirNames(t, root) {
		if strings.HasSuffix(name, ".lock") {
			locks = append(locks, name)
		}
	}
	if !slices.Equal(locks, []string{run + ".lock"}) {
		t.Errorf("lock files = %q, want only the canonical one", locks)
	}
}
