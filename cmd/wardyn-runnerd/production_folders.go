// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/folders"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// foldersRunnerHooks is --substrate folders: a runner that shares folders
// under its roots for runs on other machines and holds no sandbox. It needs no
// Docker engine and advertises no confinement class and no capacity, so
// placement never puts a run on it.
func foldersRunnerHooks(ctx context.Context, _ runneridentity.Identity, stateDir string, opts serveOptions) (runnerRuntime, error) {
	roots := opts.folderRoots
	if len(roots) == 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return runnerRuntime{}, errors.New("--substrate folders: no --folder-root and no home folder to share")
		}
		roots = []string{home}
	}
	life, cancel := context.WithCancel(ctx)
	srv, err := folders.New(life, roots, stateDir)
	if err != nil {
		cancel()
		return runnerRuntime{}, err
	}
	hooks := runnerwire.RunnerHooks{
		Call: srv.Call,
		Open: func(*runnerwire.Peer, *runnerwire.Stream, runnerwire.Open) error {
			return runnerwire.Refuse("unsupported runner stream")
		},
		Clock: func(runnerwire.Hello, time.Time) error { return nil },
		// The org queues sandbox work only for a runner that held a sandbox: this
		// identity did, under another substrate, and must finish it there.
		Pending: func(_ context.Context, _ *runnerwire.Peer, actions []runnerwire.PendingAction) error {
			if len(actions) > 0 {
				return errors.New("this runner still has sandbox work from before; serve it with --substrate docker until it is done, or register a new runner for folders")
			}
			return nil
		},
		StatePages: func(_ context.Context, send func(runnerwire.StatePage) error) error {
			return send(runnerwire.StatePage{InventoryID: uuid.New(), Runs: []runnerwire.RunState{}, Final: true})
		},
		Replay: func(ctx context.Context, peer *runnerwire.Peer) error {
			machine, _ := os.Hostname()
			caps := runnerwire.Caps{Version: version.String(), MachineName: machine, Folders: true}
			report := connectReport{log: opts.offline}
			report.fill(ctx, &caps, false)
			data, err := runnerwire.Marshal(caps)
			if err != nil {
				return err
			}
			if err := peer.EventAck(ctx, runnerwire.Event{Kind: runnerwire.EventCaps, Data: data}); err != nil {
				return err
			}
			report.sent(caps)
			return nil
		},
		BeforeReady: func(context.Context, *runnerwire.Peer, bool) error { return nil },
		Lease:       func(context.Context, runnerwire.Lease) error { return nil },
		Revoked:     func(context.Context) error { return nil },
		Offline:     func(context.Context) {},
	}
	return runnerRuntime{hooks: hooks, run: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}, close: func() error {
		cancel()
		return nil
	}}, nil
}
