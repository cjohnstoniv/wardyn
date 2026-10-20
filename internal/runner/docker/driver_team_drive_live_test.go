// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

// Real-daemon checks of what a drive's mounts put into its volume. Docker's
// copy into an empty volume happens in the daemon, so no fake can show it.
// Skipped unless WARDYN_TEST_DOCKER=1.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// driveLiveImageScript builds an agent-shaped image whose drive directory, and the team
// drive's own shared path, hold a file owned by the agent: what a member's
// image could carry into a volume Docker copies it into.
const driveLiveImageScript = "mkdir -p /home/agent/drive " + runner.SharedRoot + "/eng && " +
	"echo IMAGE > /home/agent/drive/from-image && echo IMAGE > " + runner.SharedRoot + "/eng/from-image && " +
	"chown -R 1000:1000 /home/agent"

type driveLive struct {
	t     *testing.T
	ctx   context.Context
	d     *Driver
	cli   *client.Client
	image string
}

func newDriveLive(t *testing.T) *driveLive {
	t.Helper()
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker drive tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	d, err := New(Config{ProxyImage: "busybox:latest"})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &driveLive{t: t, ctx: ctx, d: d, cli: cli, image: commitImage(ctx, t, d, driveLiveImageScript, "1000")}
}

// drive is a fresh docker volume drive at target, removed at cleanup.
func (l *driveLive) drive(target string, team, readOnly bool) *types.DriveMount {
	name := "wardyn-test-teamseed-" + uuid.NewString()[:8]
	l.t.Cleanup(func() { _, _ = l.cli.VolumeRemove(context.Background(), name, client.VolumeRemoveOptions{Force: true}) })
	return &types.DriveMount{DriveID: uuid.New(), Backend: types.DriveBackendDockerVolume, ObjectName: name,
		HomeName: "t-test", Target: target, ReadOnly: readOnly, Team: team}
}

// run runs cmd in image as user with mounts and returns its exit code and output.
func (l *driveLive) run(image, user string, mounts []mount.Mount, cmd string) (int64, string) {
	l.t.Helper()
	created, err := l.cli.ContainerCreate(l.ctx, client.ContainerCreateOptions{
		Name:       "wardyn-test-teamseed-run-" + uuid.NewString()[:8],
		Config:     &container.Config{Image: image, User: user, Cmd: []string{"sh", "-c", cmd}, NetworkDisabled: true},
		HostConfig: &container.HostConfig{NetworkMode: "none", Mounts: mounts},
	})
	if err != nil {
		l.t.Fatalf("create: %v", err)
	}
	defer func() {
		_, _ = l.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := l.cli.ContainerStart(l.ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		l.t.Fatalf("start: %v", err)
	}
	wait := l.cli.ContainerWait(l.ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	var code int64
	select {
	case res := <-wait.Result:
		code = res.StatusCode
	case err := <-wait.Error:
		l.t.Fatalf("wait: %v", err)
	}
	logs, err := l.cli.ContainerLogs(l.ctx, created.ID, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		l.t.Fatalf("logs: %v", err)
	}
	defer logs.Close()
	var out bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &out, logs)
	return code, strings.TrimSpace(out.String())
}

// volume is the volume's top directory as uid:gid:mode, then each entry and
// its content, read as root through a mount that copies nothing.
func (l *driveLive) volume(name string) string {
	l.t.Helper()
	_, out := l.run("busybox:latest", "0", []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: "/v",
		ReadOnly: true, VolumeOptions: &mount.VolumeOptions{NoCopy: true}}},
		`stat -c %u:%g:%a /v; for f in $(ls -A /v); do echo "$f=$(cat /v/$f)"; done`)
	return out
}

// mounts is what driveMount gives the run, with the run's image.
func (l *driveLive) mounts(drive *types.DriveMount) []mount.Mount {
	l.t.Helper()
	ms, err := l.d.driveMount(l.ctx, drive, uuid.New(), l.image, false)
	if err != nil {
		l.t.Fatalf("driveMount: %v", err)
	}
	return ms
}

// A read-only team allocation copies nothing from the run's image into an
// empty volume, at its seed or at its mount, and the agent cannot write it.
func TestTeamDrive_ReadOnlyLeavesAnEmptyVolumeEmpty_RealDocker(t *testing.T) {
	l := newDriveLive(t)
	drive := l.drive(runner.SharedRoot+"/eng", true, true)
	ms := l.mounts(drive)
	if code, out := l.run(l.image, "1000", ms, "touch "+drive.Target+"/w"); code == 0 {
		t.Errorf("the agent wrote a read-only team drive (%s)", out)
	}
	if got := l.volume(drive.ObjectName); got != "0:0:755" {
		t.Errorf("read-only team volume after a run = %q, want it empty (0:0:755)", got)
	}
}

// A writable team allocation copies nothing from the run's image into an empty
// volume either; its top directory is the agent's, so the agent writes it.
func TestTeamDrive_WritableLeavesAnEmptyVolumeEmpty_RealDocker(t *testing.T) {
	l := newDriveLive(t)
	drive := l.drive(runner.SharedRoot+"/eng", true, false)
	ms := l.mounts(drive)
	if got := l.volume(drive.ObjectName); got != "1000:1000:755" {
		t.Errorf("writable team volume after its seed = %q, want it empty and the agent's (1000:1000:755)", got)
	}
	if code, out := l.run(l.image, "1000", ms, "echo AGENT > "+drive.Target+"/w"); code != 0 {
		t.Errorf("the agent could not write a writable team drive: exit %d (%s)", code, out)
	}
	if got := l.volume(drive.ObjectName); got != "1000:1000:755\nw=AGENT" {
		t.Errorf("writable team volume after a run = %q, want only the agent's own file", got)
	}
}

// A team volume that already holds files is left as it is: no ownership
// change, no copy.
func TestTeamDrive_VolumeHoldingFilesIsUntouched_RealDocker(t *testing.T) {
	l := newDriveLive(t)
	drive := l.drive(runner.SharedRoot+"/eng", true, false)
	if err := ensureDriveVolume(l.ctx, l.d.cli, drive); err != nil {
		t.Fatal(err)
	}
	if code, out := l.run("busybox:latest", "0", []mount.Mount{{Type: mount.TypeVolume, Source: drive.ObjectName, Target: "/v",
		VolumeOptions: &mount.VolumeOptions{NoCopy: true}}}, "echo KEEP > /v/keep && chmod 0750 /v"); code != 0 {
		t.Fatalf("populate: exit %d (%s)", code, out)
	}
	const want = "0:0:750\nkeep=KEEP"
	if got := l.volume(drive.ObjectName); got != want {
		t.Fatalf("populated volume = %q, want %q", got, want)
	}
	ms := l.mounts(drive)
	if code, out := l.run(l.image, "1000", ms, "true"); code != 0 {
		t.Fatalf("run: exit %d (%s)", code, out)
	}
	if got := l.volume(drive.ObjectName); got != want {
		t.Errorf("team volume holding files after a run = %q, want it untouched (%q)", got, want)
	}
}

// A person's managed drive is still seeded from the image's drive directory,
// as before: its contents and the agent's ownership.
func TestPersonalDrive_StillSeededFromTheImage_RealDocker(t *testing.T) {
	l := newDriveLive(t)
	drive := l.drive(runner.DrivesRoot+"/eng", false, false)
	ms := l.mounts(drive)
	if got := l.volume(drive.ObjectName); got != "1000:1000:755\nfrom-image=IMAGE" {
		t.Errorf("personal volume after its seed = %q, want the image's drive directory", got)
	}
	if code, out := l.run(l.image, "1000", ms, "touch "+drive.Target+"/w"); code != 0 {
		t.Errorf("the agent could not write its own drive: exit %d (%s)", code, out)
	}
}

// An image whose agent is not agentUID is refused a writable team drive, by
// name, and the volume is left as Docker made it.
func TestTeamDrive_AnotherAgentUIDIsRefused_RealDocker(t *testing.T) {
	l := newDriveLive(t)
	other := commitImage(l.ctx, t, l.d, driveLiveImageScript, "1001")
	drive := l.drive(runner.SharedRoot+"/eng", true, false)
	_, err := l.d.driveMount(l.ctx, drive, uuid.New(), other, false)
	if err == nil || !strings.Contains(err.Error(), "this image runs the agent as uid 1001 (USER \"1001\")") {
		t.Fatalf("driveMount = %v, want the uid refusal", err)
	}
	if got := l.volume(drive.ObjectName); got != "0:0:755" {
		t.Errorf("team volume after the refusal = %q, want it empty and untouched (0:0:755)", got)
	}
}

// Runs seeding one empty team volume at the same moment all succeed, and the
// volume ends the agent's and empty, whatever order the daemon took them in.
// The interleaving is the daemon's; the end state may not depend on it.
func TestTeamDrive_ConcurrentSeedsAreIdempotent_RealDocker(t *testing.T) {
	l := newDriveLive(t)
	for round := range 3 {
		drive := l.drive(runner.SharedRoot+"/eng", true, false)
		start := make(chan struct{})
		errs := make([]error, 4)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = l.d.driveMount(l.ctx, drive, uuid.New(), l.image, false)
			}()
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("round %d, run %d: %v", round, i, err)
			}
		}
		if got := l.volume(drive.ObjectName); got != "1000:1000:755" {
			t.Errorf("round %d: team volume after concurrent seeds = %q, want it empty and the agent's (1000:1000:755)", round, got)
		}
	}
}
