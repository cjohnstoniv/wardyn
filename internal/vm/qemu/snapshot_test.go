// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package qemu

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSnapshotArgsNeverTouchTheRunsOverlay(t *testing.T) {
	id := uuid.MustParse("6f1c3a52-0c1e-4b8a-9d3e-2a5b7c9d1e0f")
	target := DiskDir + "/snap-" + id.String() + ".qcow2"
	plain, fork := SnapshotTargetArgs(id, false), SnapshotTargetArgs(id, true)
	if want := []string{"qemu-img", "create", "-q", "-f", "qcow2", "-F", "raw", "-b", BaseDir + "/" + RootFS, target}; !slices.Equal(plain, want) {
		t.Errorf("plain target = %q, want %q", plain, want)
	}
	if want := []string{"qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", SnapDir + "/" + SnapLayer, target}; !slices.Equal(fork, want) {
		t.Errorf("fork target = %q, want %q", fork, want)
	}
	if want := []string{"qemu-img", "rebase", "-q", "-f", "qcow2", "-F", "raw", "-b", BaseDir + "/" + RootFS, target}; !slices.Equal(FlattenArgs(id), want) {
		t.Errorf("flatten = %q, want %q", FlattenArgs(id), want)
	}
	if want := []string{"qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", SnapDir + "/" + SnapLayer, DiskDir + "/" + Overlay}; !slices.Equal(ForkOverlayArgs(), want) {
		t.Errorf("fork overlay = %q, want %q", ForkOverlayArgs(), want)
	}
	// The rebase (the one step that rewrites a backing reference) names the target, never the run's overlay.
	for _, a := range [][]string{plain, fork, FlattenArgs(id)} {
		if slices.Contains(a, DiskDir+"/"+Overlay) || slices.Contains(a, "-u") {
			t.Errorf("%q names the run's overlay or an unsafe rebase", a)
		}
	}
	if n := SnapshotNode(id); len(n) > 31 || n != "wsnap6f1c3a520c1e4b8a" {
		t.Errorf("node name %q: QEMU keeps node names under 32 bytes", n)
	}
}

type qmpCall struct {
	Execute   string         `json:"execute"`
	Arguments map[string]any `json:"arguments"`
}

// recordingQEMU answers QMP as fakeQEMU does and records every command with its arguments.
func recordingQEMU(t *testing.T, conn net.Conn, returns map[string]string) func() []qmpCall {
	t.Helper()
	var mu sync.Mutex
	var calls []qmpCall
	go func() {
		defer conn.Close()
		_, _ = conn.Write([]byte(`{"QMP": {"version": {}, "capabilities": []}}` + "\n"))
		sc := bufio.NewScanner(conn)
		for sc.Scan() {
			var req qmpCall
			_ = json.Unmarshal(sc.Bytes(), &req)
			mu.Lock()
			calls = append(calls, req)
			mu.Unlock()
			ret, ok := returns[req.Execute]
			if !ok {
				ret = `{}`
			}
			if strings.HasPrefix(ret, "ERR ") {
				_, _ = conn.Write([]byte(`{"error": {"class": "GenericError", "desc": "` + ret[4:] + `"}}` + "\n"))
				continue
			}
			_, _ = conn.Write([]byte(`{"return": ` + ret + "}\n"))
		}
	}()
	return func() []qmpCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

func TestQMPBackupCalls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	node := SnapshotNode(id)
	a, b := net.Pipe()
	calls := recordingQEMU(t, b, map[string]string{
		"query-block": `[{"device": "", "inserted": {"file": "/vm/base/rootfs.raw", "node-name": "#block0"}}, {"device": "virtio0", "inserted": {"file": "/vm/disk/overlay.qcow2", "node-name": "#block1"}}]`,
		"query-jobs":  `[{"id": "other", "status": "running"}, {"id": "` + node + `", "status": "concluded", "error": "Operation canceled"}]`,
	})
	q, err := NewQMP(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	dev, err := q.DiskDevice(ctx)
	if err != nil || dev != "virtio0" {
		t.Fatalf("disk device = %q, %v", dev, err)
	}
	if err := q.StartBackup(ctx, dev, id); err != nil {
		t.Fatal(err)
	}
	j, err := q.SnapshotJob(ctx, id)
	if err != nil || !j.Found || !j.Ended() || j.Error != "Operation canceled" {
		t.Fatalf("job = %+v, %v", j, err)
	}
	if err := q.CancelSnapshotJob(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := q.CloseSnapshot(ctx, id); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range calls() {
		got = append(got, c.Execute)
		if !slices.Contains(SnapshotCommands, c.Execute) && c.Execute != "qmp_capabilities" {
			t.Errorf("sent %s, which is not a snapshot command", c.Execute)
		}
		switch c.Execute {
		case "blockdev-add":
			file, _ := c.Arguments["file"].(map[string]any)
			if c.Arguments["node-name"] != node || file["filename"] != "/vm/disk/"+SnapshotTarget(id) {
				t.Errorf("blockdev-add = %v", c.Arguments)
			}
		case "blockdev-backup":
			if c.Arguments["device"] != "virtio0" || c.Arguments["target"] != node || c.Arguments["sync"] != "top" || c.Arguments["auto-dismiss"] != false {
				t.Errorf("blockdev-backup = %v", c.Arguments)
			}
		}
	}
	want := []string{"qmp_capabilities", "query-block", "blockdev-add", "blockdev-backup", "query-jobs", "job-cancel", "query-jobs", "job-dismiss", "blockdev-del"}
	if !slices.Equal(got, want) {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}

func TestQMPBackupFailureClosesItsNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	calls := recordingQEMU(t, b, map[string]string{"blockdev-backup": "ERR busy", "query-block": `[]`})
	q, err := NewQMP(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if _, err := q.DiskDevice(ctx); err == nil {
		t.Fatal("a VM with no overlay device answered one")
	}
	if err := q.StartBackup(ctx, "virtio0", uuid.New()); err == nil {
		t.Fatal("a refused backup answered success")
	}
	var got []string
	for _, c := range calls() {
		got = append(got, c.Execute)
	}
	if want := []string{"qmp_capabilities", "query-block", "blockdev-add", "blockdev-backup", "blockdev-del"}; !slices.Equal(got, want) {
		t.Fatalf("commands = %q, want %q", got, want)
	}
}
