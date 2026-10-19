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
	"testing"
)

// A kept end wipes the AWS credential caches while the agent still runs, as the sandbox's own
// user, before the stop that keeps its writable layer.
func TestEndSandbox_WipesAWSCachesBeforeKeeping(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := d.EndSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
	i := slices.IndexFunc(f.execLog, func(e fakeExec) bool { return slices.Equal(e.cmd, awsCacheWipeArgv) })
	if i < 0 {
		t.Fatalf("no AWS cache wipe exec at the end: %+v", f.execLog)
	}
	if e := f.execLog[i]; !e.running || e.user != "" {
		t.Errorf("wipe exec = %+v, want it run in the running agent as the sandbox's own user", e)
	}
	if c := f.containers[sb.Ref]; c.removed || c.state.Running {
		t.Errorf("agent after the end = %+v, want stopped and kept", c)
	}
}

// The wipe is best effort: a sandbox the exec cannot reach is still stopped and its proxy removed.
func TestEndSandbox_StopsWhenTheWipeCannotRun(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)
	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	f.execCreateErr = errors.New("Error response from daemon: container is paused")
	if err := d.EndSandbox(context.Background(), sb.Ref); err != nil {
		t.Fatalf("EndSandbox: %v", err)
	}
	if c := f.containers[sb.Ref]; c.state.Running || !f.containers[proxyContainerName(testSpec().RunID)].removed {
		t.Error("a failed wipe must not stop the end: the agent stops and the proxy goes")
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
