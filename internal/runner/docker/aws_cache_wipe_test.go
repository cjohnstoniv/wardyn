// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// awsSpec is testSpec for a run Wardyn staged an AWS profile for.
func awsSpec() runner.SandboxSpec {
	spec := testSpec()
	spec.Env = map[string]string{"FOO": "bar", runner.AWSProfileRecordEnv: "LmF3cy9jb25maWcJWlhoaGJYQnNaUT09"}
	return spec
}

// keptEnd creates spec's sandbox, lets f's execs finish with exit, and ends it.
func keptEnd(t *testing.T, f *fakeDocker, spec runner.SandboxSpec) (string, error) {
	t.Helper()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	f.execExited = true
	return sb.Ref, d.EndSandbox(context.Background(), sb.Ref)
}

// A kept end wipes the AWS credential caches while the agent still runs, as the sandbox's own
// user, before the stop that keeps its writable layer, and kills nothing first.
func TestEndSandbox_WipesAWSCachesBeforeKeeping(t *testing.T) {
	f := newFakeDocker()
	ref, err := keptEnd(t, f, awsSpec())
	if err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
	i := slices.IndexFunc(f.execLog, func(e fakeExec) bool { return slices.Equal(e.cmd, awsCacheWipeArgv) })
	if i < 0 {
		t.Fatalf("no AWS cache wipe exec at the end: %+v", f.execLog)
	}
	if e := f.execLog[i]; !e.running || e.user != "" {
		t.Errorf("wipe exec = %+v, want it run in the running agent as the sandbox's own user", e)
	}
	if strings.Contains(strings.Join(awsCacheWipeArgv, " "), "kill") {
		t.Errorf("the wipe signals the sandbox's processes: %q", awsCacheWipeArgv)
	}
	if c := f.containers[ref]; c.removed || c.state.Running {
		t.Errorf("agent after the end = %+v, want stopped and kept", c)
	}
}

// A run Wardyn staged no AWS profile for runs no exec at all at its kept end.
func TestEndSandbox_NoExecWithoutAnAWSProfile(t *testing.T) {
	f := newFakeDocker()
	f.execCreateErr = errors.New("fake: an exec was attempted")
	ref, err := keptEnd(t, f, testSpec())
	if err != nil {
		t.Fatalf("EndSandbox: %v (an exec ran)", err)
	}
	if len(f.execLog) != 0 {
		t.Fatalf("execs at a kept end without an AWS profile: %+v", f.execLog)
	}
	if c := f.containers[ref]; c.removed || c.state.Running {
		t.Errorf("agent after the end = %+v, want stopped and kept", c)
	}
}

// The wipe is best effort: a sandbox the exec cannot reach, or a wipe that fails, is still
// stopped and its proxy removed, and the end answers runner.ErrAWSCacheWipeFailed with the reason.
func TestEndSandbox_StopsWhenTheWipeFails(t *testing.T) {
	for name, set := range map[string]func(*fakeDocker){
		"exec refused":    func(f *fakeDocker) { f.execCreateErr = errors.New("Error response from daemon: container is paused") },
		"non-zero exit":   func(f *fakeDocker) { f.execExitCode = 1 },
		"did not finish":  func(f *fakeDocker) { f.execExited = false },
		"was not running": nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			d := newTestDriver(f)
			sb, err := d.CreateSandbox(context.Background(), awsSpec())
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			f.execExited = true
			if set != nil {
				set(f)
			} else {
				f.containers[sb.Ref].state.Running = false
			}
			err = d.EndSandbox(context.Background(), sb.Ref)
			if !errors.Is(err, runner.ErrAWSCacheWipeFailed) {
				t.Fatalf("EndSandbox = %v, want ErrAWSCacheWipeFailed", err)
			}
			if c := f.containers[sb.Ref]; c.removed || c.state.Running || !f.containers[proxyContainerName(testSpec().RunID)].removed {
				t.Error("a failed wipe must not stop the end: the agent stops, is kept, and the proxy goes")
			}
		})
	}
}

// Stop and kill remove the agent container, writable layer and caches with it, so only the kept
// end needs the wipe.
func TestStopAndKill_RemoveTheAgentLayer(t *testing.T) {
	for name, end := range map[string]func(*Driver, string) error{
		"stop": func(d *Driver, ref string) error { return d.StopSandbox(context.Background(), ref) },
		"kill": func(d *Driver, ref string) error { return d.KillSandbox(context.Background(), ref) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDocker()
			f.images["busybox:latest"] = true
			d := newTestDriver(f)
			sb, err := d.CreateSandbox(context.Background(), testSpec())
			if err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}
			if err := end(d, sb.Ref); err != nil {
				t.Fatal(err)
			}
			if c := f.containers[sb.Ref]; !c.removed || !c.forceRemoved {
				t.Errorf("agent after %s = %+v, want force-removed", name, c)
			}
		})
	}
}

// The wipe script deletes both caches, through a cache that is a link elsewhere too, and leaves
// the rest of ~/.aws (the profile a revive starts from) alone.
func TestAWSCacheWipeScript_DeletesBothCaches(t *testing.T) {
	home, elsewhere := t.TempDir(), t.TempDir()
	write := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(`{"Credentials":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	linked := filepath.Join(elsewhere, "cli-cache")
	write(filepath.Join(linked, "role.json"))
	write(filepath.Join(home, ".aws", "sso", "cache", "token.json"))
	write(filepath.Join(home, ".aws", "config"))
	if err := os.MkdirAll(filepath.Join(home, ".aws", "cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(linked, filepath.Join(home, ".aws", "cli", "cache")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", awsCacheWipeScript) //nolint:gosec // fixed in-package script
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wipe: %v: %s", err, out)
	}
	for _, gone := range []string{filepath.Join(home, ".aws", "cli", "cache"), filepath.Join(home, ".aws", "sso", "cache"), filepath.Join(linked, "role.json")} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Errorf("%s survived the wipe (%v)", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".aws", "config")); err != nil {
		t.Errorf("the wipe took ~/.aws/config too: %v", err)
	}
}
