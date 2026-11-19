// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

var errPartUnreadable = errors.New("part 2: input/output error")

// laterPartFailStore holds a run's part 1 and a part 2 whose first read fails, as a disk error would.
type laterPartFailStore struct{ recording.Store }

func (laterPartFailStore) OpenCast(_ context.Context, key string) (io.ReadCloser, error) {
	runID, part, _ := strings.Cut(key, "~")
	switch part {
	case "":
		return io.NopCloser(strings.NewReader(partHeader + partEv1)), nil
	case recording.PartSuffix(2):
		return io.NopCloser(readerFunc(func([]byte) (int, error) { return 0, errPartUnreadable })), nil
	}
	return nil, fmt.Errorf("%w: %s", recording.ErrNotFound, runID)
}

// A later part that cannot be read fails the join, as part 1 does, instead of ending it as if the cast
// stopped there: OpenJoined's reader returns the error, and a replay aborts its transfer.
func TestJoinFailsOnALaterPartThatCannotBeRead(t *testing.T) {
	rc, err := recording.OpenJoined(context.Background(), laterPartFailStore{}, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if got, err := io.ReadAll(rc); !errors.Is(err, errPartUnreadable) {
		t.Fatalf("OpenJoined read %q, err %v; want part 2's error", got, err)
	}

	srv := httptest.NewServer(newTestRouter(laterPartFailStore{}))
	defer srv.Close()
	id := uuid.New().String()
	resp, err := http.Get(srv.URL + "/api/v1/runs/" + id + "/recording/" + id)
	if err != nil {
		return // aborted before the buffered status line was flushed: failed, as wanted
	}
	defer resp.Body.Close()
	if body, err := io.ReadAll(resp.Body); err == nil {
		t.Errorf("status %d and a body %q that ended cleanly; want the transfer to fail", resp.StatusCode, body)
	}
}
