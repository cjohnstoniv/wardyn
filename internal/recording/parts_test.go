// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

const (
	partHeader = `{"version":2,"width":80,"height":24,"timestamp":1700000000}` + "\n"
	partEv1    = `[0.5,"o","one"]` + "\n"
	partEv2    = `[1.5,"o","two"]` + "\n"
	partEv3    = `[2.5,"o","three"]` + "\n"
)

// saveParts stores a tail-uploaded cast the way wardyn-rec delivers it: part 1
// under the bare run id, part n >= 2 under the part-n suffix, each carrying the
// cast's own header line again.
func saveParts(t *testing.T, s recording.Store, runID string, parts ...string) {
	t.Helper()
	ctx := context.Background()
	for i, p := range parts {
		var err error
		if i == 0 {
			err = s.SaveCast(ctx, runID, strings.NewReader(p))
		} else {
			err = s.SaveCastNamed(ctx, runID, recording.PartSuffix(i+1), strings.NewReader(p))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func getRecording(t *testing.T, s recording.Store, key string) string {
	t.Helper()
	srv := httptest.NewServer(newTestRouter(s))
	defer srv.Close()
	runID, _, _ := strings.Cut(key, "~")
	resp, err := http.Get(srv.URL + "/api/v1/runs/" + runID + "/recording/" + key)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", key, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestHandler_JoinsTailUploadedParts pins RL-12's server-side join: the replay
// of a bare run id is ONE asciicast document — part 1 as stored, then every
// later part with its re-prefixed header line dropped.
func TestHandler_JoinsTailUploadedParts(t *testing.T) {
	s, _ := recording.NewFSStore(t.TempDir())
	saveParts(t, s, "run-j", partHeader+partEv1, partHeader+partEv2, partHeader+partEv3)

	if got, want := getRecording(t, s, "run-j"), partHeader+partEv1+partEv2+partEv3; got != want {
		t.Fatalf("joined cast =\n%q\nwant\n%q", got, want)
	}
}

// TestHandler_JoinStopsAtAForeignHeader: a part whose header is not part 1's
// belongs to an earlier cast of the same run (a revive starts a new cast and
// replaces part 1), so the join ends there instead of splicing two recordings.
func TestHandler_JoinStopsAtAForeignHeader(t *testing.T) {
	s, _ := recording.NewFSStore(t.TempDir())
	older := strings.Replace(partHeader, "1700000000", "1600000000", 1)
	saveParts(t, s, "run-f", partHeader+partEv1, older+partEv2, partHeader+partEv3)

	if got, want := getRecording(t, s, "run-f"), partHeader+partEv1; got != want {
		t.Fatalf("joined cast = %q, want only part 1 %q", got, want)
	}
}

// TestHandler_SessionKeyIsNotJoined: an attach session's composite key is its
// own cast; the join applies to the bare run id only.
func TestHandler_SessionKeyIsNotJoined(t *testing.T) {
	s, _ := recording.NewFSStore(t.TempDir())
	saveParts(t, s, "run-s", partHeader+partEv1, partHeader+partEv2)
	if err := s.SaveCastNamed(context.Background(), "run-s", "sess", strings.NewReader(partHeader+partEv3)); err != nil {
		t.Fatal(err)
	}
	if got := getRecording(t, s, "run-s~sess"); got != partHeader+partEv3 {
		t.Fatalf("session cast = %q", got)
	}
}

// TestStatJoined_SumsPartsAndTailsTheLast: the Recordings list's size counts
// every stored part, and its duration comes from the LAST part's tail — event
// times run on across parts because every part is cut from one cast file.
func TestStatJoined_SumsPartsAndTailsTheLast(t *testing.T) {
	s, _ := recording.NewFSStore(t.TempDir())
	p1, p2 := partHeader+partEv1, partHeader+partEv2+partEv3
	saveParts(t, s, "run-t", p1, p2)

	size, tail, err := recording.StatJoined(context.Background(), s, "run-t", 64)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(p1)+len(p2)) {
		t.Errorf("size = %d, want %d", size, len(p1)+len(p2))
	}
	if d, ok := recording.LastOutputElapsed(tail); !ok || d != 2.5 {
		t.Errorf("duration = %v (ok=%v), want 2.5 from the last part", d, ok)
	}
	if _, _, err := recording.StatJoined(context.Background(), s, "absent", 64); err != recording.ErrNotFound {
		t.Errorf("absent cast: err = %v, want ErrNotFound", err)
	}
}

// TestPGStore_JoinsParts: the join holds on the default store too, where every
// part is its own row.
func TestPGStore_JoinsParts(t *testing.T) {
	s := recording.NewPGStore(pgPool(t))
	runID := "run-pg-" + uuid.NewString()
	p1, p2 := partHeader+partEv1, partHeader+partEv2
	saveParts(t, s, runID, p1, p2)

	rc, err := recording.OpenJoined(context.Background(), s, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if got, _ := io.ReadAll(rc); string(got) != partHeader+partEv1+partEv2 {
		t.Fatalf("joined cast = %q", got)
	}
	if size, _, err := recording.StatJoined(context.Background(), s, runID, 64); err != nil || size != int64(len(p1)+len(p2)) {
		t.Fatalf("StatJoined = %d, %v", size, err)
	}
}
