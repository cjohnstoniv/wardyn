// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// attachLogSubstrate's Attach hands out a session whose reads fail with err at once, as one does after a
// detach closed it.
type attachLogSubstrate struct {
	contextSubstrate
	err error
}

func (f attachLogSubstrate) Attach(context.Context, string, runner.AttachOptions) (runner.Session, error) {
	return attachLogSession{f.err}, nil
}

type attachLogSession struct{ err error }

func (s attachLogSession) Read([]byte) (int, error)                   { return 0, s.err }
func (attachLogSession) Write(p []byte) (int, error)                  { return len(p), nil }
func (attachLogSession) Resize(context.Context, uint16, uint16) error { return nil }
func (attachLogSession) Close() error                                 { return nil }

// attachLogBuffer is a log sink the copy goroutine and the test both touch.
type attachLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *attachLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *attachLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The attach copy warns when its session fails, but not for the closed pipe or connection a detach leaves
// behind: that is the ordinary end of an attach. Either way the stream is reset, and the reset follows the
// log call, so the client seeing it means the log decision has been made.
func TestAttachCopyWarnsOnlyForAFailureNotADetach(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		warn bool
	}{
		{"closed pipe", io.ErrClosedPipe, false},
		{"wrapped closed pipe", fmt.Errorf("read: %w", io.ErrClosedPipe), false},
		{"closed connection", &net.OpError{Op: "read", Err: net.ErrClosed}, false},
		{"a real failure", errors.New("guest exec failed"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := &attachLogBuffer{}
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			_, sub, _, _, held := streamRigWith(t, func(f *runnertest.Fake) substrate.Substrate {
				return attachLogSubstrate{contextSubstrate{f}, c.err}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			a, err := sub.Attach(ctx, held, runner.AttachOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if _, err := a.Read(make([]byte, 1)); err == nil || errors.Is(err, io.EOF) {
				t.Fatalf("read after the session failed = %v, want the stream's reset", err)
			}
			got := strings.Contains(logs.String(), "session read failed")
			if got != c.warn {
				t.Fatalf("warned = %v, want %v:\n%s", got, c.warn, logs.String())
			}
		})
	}
}
