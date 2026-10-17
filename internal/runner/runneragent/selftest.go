// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runneragent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// The org execs a BYOI image's contract check before its task
// (internal/api/runs_dispatch_byoi.go) and answers it by the exit code alone.
// Its output is not the run's: the run's output ends when its copy ends, so a
// selftest copied there would end the run's output before the task's starts.
var selftestArgv = []string{"/usr/local/bin/agent-run", "--selftest"}

// selftestLogMax bounds how much of a selftest's output the runner logs.
const selftestLogMax = 4 << 10

// execerTo is a substrate that can send one exec's output to its own writer
// (the docker driver).
type execerTo interface {
	ExecTo(ctx context.Context, ref string, argv []string, out io.Writer) (string, error)
}

// selftestTimeout bounds a selftest on the runner, as the org bounds its call.
var selftestTimeout = 2 * time.Minute

// awaitSelftest marks a BYOI sandbox as owed its one selftest (selftest).
func (d *Dispatcher) awaitSelftest(ref, image string) {
	if strings.HasPrefix(image, "wardyn-byoi/") {
		d.selftests.Store(ref, struct{}{})
	}
}

// Selftests reports whether this runner serves runnerwire.MethodSelftest, for
// its caps' BYOISelftest.
func (d *Dispatcher) Selftests() bool {
	_, ok := d.cfg.Substrate.(execerTo)
	return ok && d.cfg.CheckSelftest != nil
}

// selftest runs ref's one awaited selftest, under the task's unattempted
// generation, with its output in the runner's log and its exit code in the
// reply. It takes no argv and is not an Exec: it spends no generation, and its
// exit is never recorded as the run's.
func (d *Dispatcher) selftest(ctx context.Context, args json.RawMessage) (any, error) {
	var a runnerwire.SelftestArgs
	if err := runnerwire.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	ref, err := d.local(a.Ref)
	if err != nil {
		return nil, err
	}
	if err := d.owned(ctx, ref); err != nil {
		return nil, err
	}
	to, ok := d.cfg.Substrate.(execerTo)
	if !ok || d.cfg.CheckSelftest == nil {
		return nil, runnerwire.Refuse("this runner cannot run the BYOI selftest")
	}
	if err := d.cfg.CheckSelftest(ctx, ref, a.ExecutionGeneration); err != nil {
		return nil, err
	}
	if _, awaited := d.selftests.LoadAndDelete(ref); !awaited {
		return nil, runnerwire.Refuse("this sandbox owes no BYOI selftest")
	}
	ctx, cancel := context.WithTimeout(ctx, selftestTimeout)
	defer cancel()
	if _, err := to.ExecTo(ctx, ref, selftestArgv, &selftestLog{ref: ref}); err != nil {
		return nil, err
	}
	code, err := d.cfg.Substrate.Wait(ctx, ref)
	if err != nil {
		return nil, err
	}
	return runnerwire.SelftestResult{ExitCode: code}, nil
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
	slog.Info("BYOI selftest output", slog.String("ref", l.ref), slog.String("output", string(l.tail)))
	return nil
}
