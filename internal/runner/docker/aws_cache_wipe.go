// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// awsCacheWipeScript deletes the two caches where AWS clients in the sandbox keep what they
// derive from the person's sign-in: ~/.aws/cli/cache (the CLI's role credentials) and
// ~/.aws/sso/cache (the SSO token cache Wardyn writes and the CLI rewrites). The glob first
// empties a cache that is a link elsewhere, then the link or directory itself goes.
const awsCacheWipeScript = `h="${HOME:-/home/agent}"; rm -rf "$h/.aws/cli/cache/"* "$h/.aws/cli/cache" "$h/.aws/sso/cache"`

// awsCacheWipeArgv runs the wipe. The stop follows at once; an AWS client still running could
// write a cache back in the moment between the two.
var awsCacheWipeArgv = []string{"sh", "-c", awsCacheWipeScript}

// stagedAWSProfile reports whether cfg's agent was dispatched the ~/.aws Wardyn stages for an AWS
// sign-in lane (runner.AWSProfileRecordEnv): the runs whose caches the wipe is for.
func stagedAWSProfile(cfg *container.Config) bool {
	if cfg == nil {
		return false
	}
	if cfg.Labels[labelAWSProfile] == "1" {
		return true // a Linux VM's QEMU container: the run's env went to the guest, not to it
	}
	return slices.ContainsFunc(cfg.Env, func(kv string) bool {
		v, ok := strings.CutPrefix(kv, runner.AWSProfileRecordEnv+"=")
		return ok && v != ""
	})
}

// wipeAWSCaches runs awsCacheWipeArgv in the agent ref as its own user before a kept end stops
// it: the one terminal path whose container (writable layer and all) outlives the run. Every
// other path removes the container, and Kubernetes has no kept end. Only a run Wardyn staged an
// AWS profile for is wiped; any other runs no exec at all. An inspect that fails other than
// not-found cannot rule a staged profile out, so the wipe still runs. A wipe that cannot run or
// fails (a stopped or paused sandbox, a non-zero exit) is answered as runner.ErrAWSCacheWipeFailed
// with its reason, and the stop goes ahead, since containment outranks the wipe.
func (d *Driver) wipeAWSCaches(ctx context.Context, ref string) error {
	insp, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	switch {
	case isNotFound(err):
		return nil
	case err == nil && !stagedAWSProfile(insp.Container.Config):
		return nil
	case err == nil && (insp.Container.State == nil || !insp.Container.State.Running):
		// Nothing runs to rewrite a cache, and no exec can run to delete one: an end
		// re-asserted on a kept sandbox lands here every pass, so this is not logged.
		return fmt.Errorf("%w: the sandbox was not running", runner.ErrAWSCacheWipeFailed)
	}
	// A ref the engine cannot classify is never taken for a container: the wipe would exec in the QEMU container.
	v, lerr := d.vmLookup(ctx, ref)
	switch {
	case lerr != nil:
		err = lerr
	case v != nil:
		err = d.vmAWSCacheWipe(ctx, v)
	default:
		err = d.containerAWSCacheWipe(ctx, ref)
	}
	switch {
	case err == nil:
		return nil
	case isNotFound(err):
		return nil // the sandbox is gone, and its caches with it
	}
	slog.WarnContext(ctx, "wardynd: could not wipe the AWS credential caches before keeping an ended sandbox",
		slog.String("ref", ref), slog.Any("err", err))
	return fmt.Errorf("%w: %w", runner.ErrAWSCacheWipeFailed, err)
}

// containerAWSCacheWipe runs the wipe in a container's agent.
func (d *Driver) containerAWSCacheWipe(ctx context.Context, ref string) error {
	created, err := d.cli.ExecCreate(ctx, ref, client.ExecCreateOptions{Cmd: awsCacheWipeArgv})
	if err == nil {
		_, err = d.cli.ExecStart(ctx, created.ID, client.ExecStartOptions{})
	}
	if err == nil {
		err = d.awsCacheWipeExit(ctx, created.ID)
	}
	return err
}

// vmAWSCacheWipe runs the wipe in a Linux VM's guest, as the guest agent's user, through the guest
// agent: the QEMU container holds QEMU, not the sandbox. It is bounded by stopTimeout, since the
// power-down that follows must not wait on a guest that does not answer.
func (d *Driver) vmAWSCacheWipe(ctx context.Context, v *vmRun) error {
	ctx, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	s, err := d.vmExecStream(ctx, v, runner.ExecSpec{Argv: awsCacheWipeArgv, TTY: true})
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	go func() { _, _ = io.Copy(io.Discard, s.Stdout) }()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, werr := s.Wait()
		done <- result{code, werr}
	}()
	select {
	case r := <-done:
		switch {
		case r.err != nil:
			return r.err
		case r.code != 0:
			return fmt.Errorf("the wipe exited %d", r.code)
		}
		return nil
	case <-ctx.Done():
		return errors.New("the wipe did not finish before the stop")
	}
}

// awsCacheWipeExit waits for the wipe exec and reports one that did not finish or exited non-zero.
func (d *Driver) awsCacheWipeExit(ctx context.Context, execID string) error {
	d.waitExec(ctx, execID)
	insp, err := d.cli.ExecInspectRaw(ctx, execID)
	if err != nil {
		return err
	}
	switch code, exited := insp.exited(); {
	case !exited:
		return errors.New("the wipe did not finish before the stop")
	case code != 0:
		return fmt.Errorf("the wipe exited %d", code)
	}
	return nil
}
