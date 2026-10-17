// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/client"
)

// TestCheckStateDir_Live runs the state directory check on the engine at DOCKER_HOST against a directory
// this process made, which that engine shares: the check passes and leaves no container and no file. It
// needs WARDYN_TEST_DOCKER=1 and an image on that engine, WARDYN_TEST_PROXY_IMAGE or else busybox:latest
// (the container is created, never started).
func TestCheckStateDir_Live(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the live state directory check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	state, runnerID := t.TempDir(), "state-dir-check-live-"+uuid.NewString()
	d, err := New(Config{ProxyImage: cmp.Or(os.Getenv("WARDYN_TEST_PROXY_IMAGE"), "busybox:latest"), RunnerStateDir: state, RunnerID: runnerID})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckStateDir(ctx); err != nil {
		t.Fatalf("an engine that shares the state directory: %v", err)
	}
	cli, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	left, err := cli.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label", "wardyn.runner-id="+runnerID)})
	if err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 0 {
		t.Fatalf("the check left %d containers", len(left.Items))
	}
	if files, _ := os.ReadDir(filepath.Join(state, "spools")); len(files) != 0 {
		t.Fatalf("the check left %v in the state directory", files)
	}
}
