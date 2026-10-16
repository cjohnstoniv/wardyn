// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The org execs a BYOI image's contract check before its task
// (internal/api/runs_dispatch_byoi.go) and answers it by the exit code alone.
// Its output is not the run's: the run's output ends when its copy ends, so a
// selftest copied there would end the run's output before the task's starts.
// runner.SelftestArgv names it for both sides.

// selftestLogMax bounds how much of a selftest's output the runner logs.
const selftestLogMax = 4 << 10

// execerTo is a substrate that can send one exec's output to its own writer
// (the docker driver).
type execerTo interface {
	ExecTo(ctx context.Context, ref string, argv []string, out io.Writer) (string, error)
}

// awaitSelftest marks a BYOI sandbox whose run output this runner keeps: its
// first selftest exec goes to the runner's log instead.
func (d *Dispatcher) awaitSelftest(ref, image string) {
	if strings.HasPrefix(image, "wardyn-byoi/") {
		d.selftests.Store(ref, struct{}{})
	}
}

// execSelftest runs ref's awaited selftest with its output in the runner's log,
// and reports false when argv is not that selftest.
func (d *Dispatcher) execSelftest(ctx context.Context, ref string, argv []string) (string, bool, error) {
	to, ok := d.cfg.Substrate.(execerTo)
	if !ok || !slices.Equal(argv, runner.SelftestArgv()) {
		return "", false, nil
	}
	if _, awaited := d.selftests.LoadAndDelete(ref); !awaited {
		return "", false, nil
	}
	id, err := to.ExecTo(ctx, ref, argv, &selftestLog{ref: ref})
	return id, true, err
}

// selftestLog keeps the last selftestLogMax bytes of a selftest's output and
// logs them when the copy ends.
type selftestLog struct {
	ref  string
	tail []byte
}

func (l *selftestLog) Write(p []byte) (int, error) {
	l.tail = append(l.tail, p...)
	if over := len(l.tail) - selftestLogMax; over > 0 {
		l.tail = l.tail[over:]
	}
	return len(p), nil
}

func (l *selftestLog) Close() error {
	slog.Debug("BYOI selftest output", slog.String("ref", l.ref), slog.String("output", string(l.tail)))
	return nil
}
