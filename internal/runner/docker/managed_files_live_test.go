// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

// Real-daemon checks of managed-file delivery. What they pin is what the
// daemon does to a filesystem, which no fake can show. Skipped unless
// WARDYN_TEST_DOCKER=1.

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The agent must not be able to replace a managed file by any route the
// sandbox leaves open. Each path below is one such route, tried as uid 1000
// against a real sandbox: renaming the file's directory aside when its parent
// is the agent's own (/work in this image), finding the file hidden under the
// tmpfs mounted on /tmp at start, and deleting it once recording setup has run
// chmod 0777 on its directory. The consumer's path must deliver and hold; any
// other path passes only by being refused, which leaves no file and no run.
func TestManagedFiles_AgentCannotReplaceTheFile_RealDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker managed-file tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d, err := New(Config{ProxyImage: "busybox:latest", ProxyCmd: []string{"sleep", "infinity"}, Record: true})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	ensureNetwork(t, d, d.cfg.InternalNetwork)
	image := agentImageOwningWork(ctx, t, d)

	const body = "operator-ceiling"
	consumer := runner.ManagedFileDir + "/managed-settings.json"
	for _, p := range []string{consumer, "/work/.claude/settings.json", "/tmp/wardyn/policy.json", "/var/log/wardyn/p.json"} {
		t.Run(p, func(t *testing.T) {
			sb, err := d.CreateSandbox(ctx, runner.SandboxSpec{
				RunID:            uuid.New(),
				Image:            image,
				ConfinementClass: types.CC1,
				ProxyConfig:      runner.ProxyConfig{RunToken: "test", ControlPlaneURL: "http://127.0.0.1:0"},
				ManagedFiles:     []runner.ManagedFile{{Path: p, Content: []byte(body)}},
			})
			if err != nil {
				if p == consumer || !strings.Contains(err.Error(), "must sit directly in") {
					t.Fatalf("CreateSandbox: %v", err)
				}
				t.Logf("refused: %v", err)
				return
			}
			t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })

			dir := path.Dir(p)
			execInSandbox(ctx, t, d, sb.Ref, []string{"sh", "-c", fmt.Sprintf(
				"rm -f %[1]s; chmod 0666 %[1]s; mv %[2]s %[2]s.aside; mkdir -p %[2]s; echo AGENT > %[1]s; true", p, dir)})
			got := execInSandbox(ctx, t, d, sb.Ref, []string{"sh", "-c", fmt.Sprintf("id -u; stat -c %%u:%%g %[1]s; cat %[1]s", p)})
			if want := "1000\n0:0\n" + body; got != want {
				t.Errorf("the agent replaced the managed file %s — as the agent, `id -u; stat; cat` now reads %q, want %q", p, got, want)
			}
		})
	}
}

// agentImageOwningWork commits busybox:latest as an image-contract agent image:
// USER 1000:1000, plus a top-level /work that user owns. Removed at cleanup.
func agentImageOwningWork(ctx context.Context, t *testing.T, d *Driver) string {
	t.Helper()
	if err := d.ensureImage(ctx, "busybox:latest", func() {}); err != nil {
		t.Fatalf("ensure busybox: %v", err)
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	suffix := uuid.NewString()[:8]
	created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   "wardyn-managed-image-" + suffix,
		Config: &container.Config{Image: "busybox:latest", Cmd: []string{"sh", "-c", "mkdir -p /work && chown 1000:1000 /work"}},
	})
	if err != nil {
		t.Fatalf("create image builder: %v", err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatalf("start image builder: %v", err)
	}
	wait := cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			t.Fatalf("image builder exited %d", res.StatusCode)
		}
	case err := <-wait.Error:
		t.Fatalf("wait image builder: %v", err)
	}
	ref := "wardyn-managed-agent:" + suffix
	if _, err := cli.ContainerCommit(ctx, created.ID, client.ContainerCommitOptions{Reference: ref, Changes: []string{"USER 1000:1000"}}); err != nil {
		t.Fatalf("commit agent image: %v", err)
	}
	t.Cleanup(func() {
		_, _ = cli.ImageRemove(context.Background(), ref, client.ImageRemoveOptions{Force: true, PruneChildren: true})
	})
	return ref
}

// A delivery must never change a directory it did not create. A bind mount is
// the sharpest case: the daemon mounts binds for the copy, so a directory entry
// in the archive re-owned the HOST directory to root and its owner could no
// longer write to it. The bind sits on the managed-file directory itself — a
// target no mount rule allows — so this holds the delivery to that property
// whatever paths the contract accepts.
func TestManagedFiles_LeavesAMountedDirectoryAlone_RealDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker managed-file tests")
	}
	d := newNetworkTestDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := d.ensureImage(ctx, "busybox:latest", func() {}); err != nil {
		t.Fatalf("ensure busybox: %v", err)
	}

	hostDir := t.TempDir()
	before := hostDirState(t, hostDir)
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       "wardyn-managed-bind-" + uuid.NewString()[:8],
		Config:     &container.Config{Image: "busybox:latest", Cmd: []string{"true"}},
		HostConfig: &container.HostConfig{Binds: []string{hostDir + ":/etc/claude-code"}},
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})

	err = d.deliverManagedFiles(ctx, created.ID, []runner.ManagedFile{
		{Path: "/etc/claude-code/managed-settings.json", Content: []byte(`{"managed":true}`)},
	})
	if after := hostDirState(t, hostDir); after != before {
		t.Errorf("delivery changed the HOST directory bound at /etc/claude-code: %s -> %s", before, after)
	}
	if err == nil {
		t.Error("delivery into a directory that already existed (a bind mount) succeeded; it must be refused")
	} else {
		t.Logf("refused: %v", err)
	}
}

// hostDirState is dir's owner, mode and entries as one comparable string.
func hostDirState(t *testing.T, dir string) string {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: no owner information on this platform", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return fmt.Sprintf("owner %d:%d mode %04o entries %v", st.Uid, st.Gid, fi.Mode().Perm(), names)
}
