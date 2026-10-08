// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A delivered secret is the agent's and no other uid's in the sandbox, on a
// real daemon — the only thing that can show the archive header's owner and
// mode survive extraction: owned by the uid the image's USER runs as, 0400, in
// root-owned 0755 directories the agent cannot add to; a second uid in the
// same sandbox cannot read it. Skipped unless WARDYN_TEST_DOCKER=1.
func TestManagedFiles_ASecretIsReadableByTheAgentAlone_RealDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker managed-file tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	d := newNetworkTestDriver(t)
	image := commitImage(ctx, t, d, "adduser -D -u 1000 agent && adduser -D -u 1001 other", "1000:1000")

	const value = "real-docker-file-secret-0001"
	p := runner.ComponentSecretDir + "/api-token"
	sb, err := d.CreateSandbox(ctx, runner.SandboxSpec{
		RunID:            uuid.New(),
		Image:            image,
		ConfinementClass: types.CC1,
		ProxyConfig:      runner.ProxyConfig{RunToken: "test", ControlPlaneURL: "http://127.0.0.1:0"},
		ManagedFiles:     []runner.ManagedFile{{Path: p, Mode: runner.ComponentSecretFileMode, AgentOwned: true, Content: []byte(value)}},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _ = d.KillSandbox(context.Background(), sb.Ref) })

	want := "1000\n1000:0 400\n0:0 755\n0:0 755\n" + value
	if got := execInSandbox(ctx, t, d, sb.Ref, []string{"sh", "-c",
		"id -u; stat -c '%u:%g %a' " + p + " " + runner.ComponentSecretDir + " /run/wardyn; cat " + p}); got != want {
		t.Errorf("as the agent, `id; stat; cat` = %q, want %q", got, want)
	}
	if got := execAsUser(ctx, t, d, sb.Ref, "1001:1001", []string{"sh", "-c", "cat " + p + " 2>/dev/null || echo REFUSED"}); strings.Contains(got, value) || !strings.Contains(got, "REFUSED") {
		t.Errorf("another uid in the sandbox read the secret: %q", got)
	}
	if got := execInSandbox(ctx, t, d, sb.Ref, []string{"sh", "-c",
		"touch " + runner.ComponentSecretDir + "/x 2>/dev/null && echo ADDED || echo REFUSED"}); !strings.Contains(got, "REFUSED") {
		t.Errorf("the agent added a file beside the secret: %q", got)
	}
}

// execAsUser is execInSandbox run as user rather than the image's USER.
func execAsUser(ctx context.Context, t *testing.T, d *Driver, ref, user string, argv []string) string {
	t.Helper()
	created, err := d.cli.ExecCreate(ctx, ref, client.ExecCreateOptions{User: user, AttachStdout: true, AttachStderr: true, Cmd: argv})
	if err != nil {
		t.Fatalf("exec create %v as %s: %v", argv, user, err)
	}
	res, err := d.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		t.Fatalf("exec attach %v as %s: %v", argv, user, err)
	}
	defer res.HijackedResponse.Close()
	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, res.HijackedResponse.Reader); err != nil && err != io.EOF {
		t.Logf("exec %v as %s: stdcopy warning: %v", argv, user, err)
	}
	return stdout.String() + stderr.String()
}
