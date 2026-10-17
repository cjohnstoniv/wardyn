// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

const runnerSpoolTarget = "/var/lib/wardyn/decision-spool"

// proxyImageUser is the proxy image's own user and group (distroless nonroot).
const proxyImageUser = 65532

// processUID and chownSpool are this process's uid and the spool hand-over, swapped in tests, which
// cannot be root.
var (
	processUID = os.Getuid
	chownSpool = chownSpoolTree
)

// chownSpoolTree gives the spool directory and every entry in it (the spool holds no subdirectory) to
// uid:gid, never following a link and never a file that has a second link.
func chownSpoolTree(dir string, uid, gid int) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, name := range append(names, ".") {
		// A file with another link is left as it is (spoolowner.Keep's rule): a file outside the spool
		// is never handed over through a link to it.
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok && !info.IsDir() && st.Nlink != 1 {
			continue
		}
		if err := root.Lchown(name, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

// runnerSpoolConfig is the one place a proxy's config gets decision_spool_dir, and it adds the bind mount
// that directory needs in the same step: a proxy never has the one without the other. A received
// decision_spool_dir never survives; without a state directory (the organisation's own Docker substrate)
// the proxy gets neither.
func (d *Driver) runnerSpoolConfig(ctx context.Context, id uuid.UUID, b []byte, cfg *container.Config, host *container.HostConfig) ([]byte, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, errors.New("runner proxy config is empty")
	}
	if d.cfg.RunnerStateDir == "" {
		if _, ok := data["decision_spool_dir"]; !ok {
			return b, nil
		}
		delete(data, "decision_spool_dir")
		return json.Marshal(data)
	}
	if !filepath.IsAbs(d.cfg.RunnerStateDir) || id == uuid.Nil {
		return nil, fmt.Errorf("runner spool: the state directory %q is not an absolute path, or the run has no id", d.cfg.RunnerStateDir)
	}
	dir := filepath.Join(d.cfg.RunnerStateDir, "spools", id.String())
	spool, err := runnerspool.Open(dir, id)
	if err != nil {
		return nil, fmt.Errorf("runner spool: %w", err)
	}
	if err := spool.Close(); err != nil {
		return nil, err
	}
	data["decision_spool_dir"], err = json.Marshal(runnerSpoolTarget)
	if err != nil {
		return nil, err
	}
	b, err = json.Marshal(data)
	if err != nil {
		return nil, err
	}
	info, err := d.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return nil, err
	}
	// Rootless Docker maps its container root to the daemon owner. A rootful daemon needs the caller's
	// uid/gid to create files runnerd can read at 0600, unless runnerd is root: root reads them anyway,
	// so the proxy keeps its image's non-root user and the spool is handed to that user.
	switch {
	case slices.Contains(info.Info.SecurityOptions, "name=rootless"):
		cfg.User = "0:0"
	case processUID() == 0:
		if err := chownSpool(dir, proxyImageUser, proxyImageUser); err != nil {
			return nil, fmt.Errorf("runner spool: hand to the proxy's user: %w", err)
		}
		cfg.User = fmt.Sprintf("%d:%d", proxyImageUser, proxyImageUser)
	default:
		cfg.User = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	host.Mounts = append(host.Mounts, mount.Mount{Type: mount.TypeBind, Source: dir, Target: runnerSpoolTarget, BindOptions: &mount.BindOptions{NonRecursive: true}})
	return b, nil
}

// stateDirCheckBound bounds CheckStateDir: its container create, stat, copy and removal.
const stateDirCheckBound = 30 * time.Second

const (
	componentStateDirCheck  = "state-dir-check"
	stateDirCheckNamePrefix = "wardyn-state-dir-check-"
	stateDirCheckTarget     = "/var/lib/wardyn/state-dir-check"
)

// CheckStateDir proves the engine sees RunnerStateDir as this process does, the way each run's proxy will
// use its spool there: a file written here is in a container's bind mount of the directory, and a file the
// engine writes through that mount lands here. A VM-backed engine that does not share the directory can
// make up an empty one for the mount instead of refusing it, and every run's proxy would then start without
// its spool. The container is created, never started: the engine's archive API reads and writes through
// its mounts.
func (d *Driver) CheckStateDir(ctx context.Context) error {
	if d.cfg.RunnerStateDir == "" {
		return nil
	}
	if !filepath.IsAbs(d.cfg.RunnerStateDir) {
		return fmt.Errorf("the runner state directory %q is not an absolute path: run serve with --state-dir set to an absolute directory the Docker engine shares", d.cfg.RunnerStateDir)
	}
	ctx, cancel := context.WithTimeout(ctx, stateDirCheckBound)
	defer cancel()
	state := d.cfg.RunnerStateDir
	spools := filepath.Join(state, "spools")
	if err := os.MkdirAll(spools, 0o700); err != nil {
		return fmt.Errorf("the runner state directory %s cannot hold run spools: %w", state, err)
	}
	dir, err := os.MkdirTemp(spools, ".engine-check-")
	if err != nil {
		return fmt.Errorf("the runner state directory %s cannot hold run spools: %w", state, err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "from-runner"), []byte("wardyn\n"), 0o600); err != nil {
		return fmt.Errorf("the runner state directory %s cannot hold run spools: %w", state, err)
	}
	unseen := func(why string) error {
		return fmt.Errorf("the Docker engine does not see the runner state directory %s as wardyn-runnerd does: %s. "+
			"Each run's proxy keeps its audit spool there and refuses every request without it. Share the directory with the engine "+
			"(Docker Desktop: Settings, Resources, File sharing; Colima or Lima: a writable mount), or run serve with --state-dir set to a directory the engine shares", state, why)
	}
	labels := map[string]string{labelManaged: "true", labelComponent: componentStateDirCheck}
	if d.cfg.RunnerID != "" {
		labels[runner.LabelRunnerID] = d.cfg.RunnerID // lets a sweeper claim a stray
	}
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:   stateDirCheckNamePrefix + uuid.NewString(),
		Config: &container.Config{Image: d.proxyImage(), Labels: labels},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode("none"),
			Mounts: []mount.Mount{{Type: mount.TypeBind, Source: dir, Target: stateDirCheckTarget}}},
	})
	if err != nil {
		return fmt.Errorf("docker: check the runner state directory %s: create a container that mounts it: %w", state, err)
	}
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), relayProbeCleanup)
		defer rcancel()
		_, _ = d.cli.ContainerRemove(rctx, created.ID, client.ContainerRemoveOptions{Force: true})
	}()
	if _, err := d.cli.ContainerStatPath(ctx, created.ID, client.ContainerStatPathOptions{Path: stateDirCheckTarget + "/from-runner"}); isNotFound(err) {
		return unseen("a file written there is not in the engine's view of it")
	} else if err != nil {
		return fmt.Errorf("docker: check the runner state directory %s: %w", state, err)
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	if err := tw.WriteHeader(&tar.Header{Name: "from-engine", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1, Uid: os.Getuid(), Gid: os.Getgid()}); err != nil {
		return err
	}
	if _, err := tw.Write([]byte("\n")); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if _, err := d.cli.CopyToContainer(ctx, created.ID, client.CopyToContainerOptions{DestinationPath: stateDirCheckTarget, Content: &archive}); err != nil {
		return unseen(fmt.Sprintf("the engine cannot write there (%v)", err))
	}
	if _, err := os.Stat(filepath.Join(dir, "from-engine")); err != nil {
		return unseen("a file the engine writes there does not appear here")
	}
	return nil
}
