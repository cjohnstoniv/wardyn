// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker && unix

package docker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/macvm/admission"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// A macOS VM's folders are checked here, in runnerd, before the run directory or network exists: a
// refusal from the helper could only surface as a helper that did not start.

// macShareProbes bounds every share's filesystem work on this runner: inside a folder macOS
// protects, a resolve or an open can wait on a privacy prompt, an evicted iCloud file or a stalled
// volume.
var macShareProbes = &macShareProbe{slots: make(chan struct{}, 4), wait: 10 * time.Second}

// macShareProbe runs check for one share on a worker of its own. A worker holds a slot until it
// actually returns, so stuck workers never exceed the pool. The create waits, for a slot and then
// the result, until the result, its own cancellation, or wait, whichever is first; a result that
// comes later is dropped, and check made no state the create would have to undo.
type macShareProbe struct {
	slots chan struct{}
	wait  time.Duration
}

// errShareUnreadable is a share the runner could not read, or not in time.
var errShareUnreadable = errors.New("docker: the shared folder could not be read")

func (p *macShareProbe) run(ctx context.Context, check func() (string, error)) (string, error) {
	timer := time.NewTimer(p.wait)
	defer timer.Stop()
	select {
	case p.slots <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", errShareUnreadable
	}
	type result struct {
		real string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-p.slots }()
		real, err := check()
		done <- result{real, err}
	}()
	select {
	case r := <-done:
		return r.real, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", errShareUnreadable
	}
}

// macShareTarget is target's place in a macOS guest: /home/agent/<rest> is <rest> under the agent's
// home there. The agent's home itself, its .wardyn folder (the run's CA) and anything outside
// /home/agent are refused; the guest is case-insensitive, so .WARDYN is .wardyn.
func macShareTarget(target string) (string, error) {
	rest, ok := strings.CutPrefix(target, guestagent.AgentHome+"/")
	if !ok || rest == "" || path.Clean(target) != target {
		return "", errors.New(runner.MacVMShareTarget)
	}
	if first, _, _ := strings.Cut(rest, "/"); strings.EqualFold(first, ".wardyn") {
		return "", errors.New(runner.MacVMShareTarget)
	}
	return guestagent.MacAgentHome + "/" + rest, nil
}

// macShares checks every mount of a macOS VM run and returns the shares the helper builds and the
// guest mounts. Each source is resolved once, on a probe worker, and every rule, and the privacy
// probe, decides about that one canonical path, which alone is recorded.
func (d *Driver) macShares(ctx context.Context, spec runner.SandboxSpec) ([]macvm.Share, []guestagent.Mount, error) {
	if len(spec.Mounts) == 0 {
		return nil, nil, nil
	}
	c := d.macVMConfig()
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil, fmt.Errorf("docker: the runner login's home: %w", err)
	}
	protected := runner.MacProtectedPaths(home, c.Layout.Root, cmp.Or(c.LedgerDir, admission.DefaultDir),
		cmp.Or(c.SockRoot, admission.DefaultSockDir))
	var shares []macvm.Share
	var mounts []guestagent.Mount
	for i, m := range spec.Mounts {
		member := spec.UserMountRoots != nil && m.MemberAuthored
		denied := func(err error) error {
			if member {
				return fmt.Errorf("docker: denied member workspace mount %q -> %q: %w", m.Source, m.Target, err)
			}
			return fmt.Errorf("docker: denied workspace mount %q -> %q: %w", m.Source, m.Target, err)
		}
		guest, err := macShareTarget(m.Target)
		if err != nil {
			return nil, nil, err
		}
		if err := runner.ValidateAuthoredTarget(m.Target); err != nil {
			return nil, nil, denied(err)
		}
		if err := runner.ValidateMountSourceLexical(m.Source); err != nil {
			return nil, nil, denied(err)
		}
		var writable *runner.UserWritableCeiling
		if !m.ReadOnly {
			writable = spec.UserWritable
		}
		real, err := macShareProbes.run(ctx, func() (string, error) {
			return checkMacShare(m.Source, member, spec.UserMountRoots, writable, protected)
		})
		switch {
		case errors.Is(err, errShareUnreadable):
			return nil, nil, errors.New(runner.MacVMFolderUnreadable(filepath.Base(m.Source)))
		case err != nil && ctx.Err() != nil:
			return nil, nil, err
		case err != nil:
			return nil, nil, denied(err)
		}
		tag := macvm.ShareTag(i)
		shares = append(shares, macvm.Share{Tag: tag, HostPath: real, Target: guest, ReadOnly: m.ReadOnly})
		mounts = append(mounts, guestagent.Mount{Tag: tag, Target: guest, ReadOnly: m.ReadOnly})
	}
	return shares, mounts, nil
}

// checkMacShare is one share's filesystem work, run on a probe worker: resolve the source once,
// require a folder, run the rules on that path (runner.CheckMacShareSource), then open it and read
// one entry, as the helper will. A privacy refusal is errShareUnreadable; the descriptor is closed
// here, however late.
func checkMacShare(src string, member bool, roots []string, writable *runner.UserWritableCeiling, protected []string) (string, error) {
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return "", errShareUnreadable
		}
		return "", fmt.Errorf("mount source %q could not be resolved: %w", src, err)
	}
	if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("mount source %q is not a folder", src)
	}
	if err := runner.CheckMacShareSource(real, member, roots, writable, protected); err != nil {
		return "", err
	}
	f, err := os.OpenFile(real, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return "", errShareUnreadable
		}
		return "", err
	}
	defer f.Close()
	if _, err := f.ReadDir(1); err != nil && !errors.Is(err, io.EOF) {
		if errors.Is(err, os.ErrPermission) {
			return "", errShareUnreadable
		}
		return "", err
	}
	return real, nil
}
