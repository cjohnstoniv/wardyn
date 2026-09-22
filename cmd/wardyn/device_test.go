// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// enrol-token posts exactly {name} and prints the token alone on stdout, so
// `TOKEN=$(wardyn device enrol-token --name x)` is the whole MDM integration.
func TestDeviceEnrolTokenCmd_PrintsOnlyTheToken(t *testing.T) {
	srv := newCmdServer(t, http.StatusCreated, types.DeviceEnrolmentToken{
		ID: uuid.New(), DeviceName: "alices-laptop", ExpiresAt: time.Now().Add(72 * time.Hour), Token: "wde_abc123",
	})
	root := rootCmd()
	var stdout, stderr strings.Builder
	root.SetArgs([]string{"device", "enrol-token", "--name", "alices-laptop", "--url", srv.URL, "--token", "tok"})
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "wde_abc123\n" {
		t.Fatalf("stdout = %q, want the token alone", stdout.String())
	}
	if !strings.Contains(stderr.String(), "alices-laptop") {
		t.Fatalf("stderr = %q, want the human note naming the device", stderr.String())
	}
	req := srv.last()
	var body map[string]any
	if err := json.Unmarshal(req.body, &body); err != nil || len(body) != 1 || body["name"] != "alices-laptop" {
		t.Fatalf("%s %s body %s, want exactly {name}", req.method, req.path, req.body)
	}
	if req.method != http.MethodPost || req.path != "/api/v1/admin/devices/enrolment-tokens" {
		t.Fatalf("request = %s %s", req.method, req.path)
	}
	if err := execCmd(t, "device", "enrol-token", "--url", srv.URL, "--token", "tok"); err == nil {
		t.Fatal("enrol-token without --name succeeded, want the required-flag error")
	}
}

func TestDeviceListCmd_PrintsTheFullID(t *testing.T) {
	id := uuid.New()
	srv := newCmdServer(t, http.StatusOK, []types.Device{{ID: id, Name: "alices-laptop", LastSeq: 7}})
	var execErr error
	out := captureStdout(t, func() {
		execErr = execCmd(t, "device", "list", "--url", srv.URL, "--token", "tok")
	})
	if execErr != nil {
		t.Fatal(execErr)
	}
	if !strings.Contains(out, id.String()) || !strings.Contains(out, "alices-laptop") || !strings.Contains(out, "active") {
		t.Fatalf("list output lacks the full id, name or status:\n%s", out)
	}
	if req := srv.last(); req.method != http.MethodGet || req.path != "/api/v1/admin/devices" {
		t.Fatalf("request = %s %s", req.method, req.path)
	}
}

func TestDeviceRevokeCmd(t *testing.T) {
	id := uuid.New()
	srv := newCmdServer(t, http.StatusNoContent, nil)
	if err := execCmd(t, "device", "revoke", id.String(), "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatal(err)
	}
	if req := srv.last(); req.method != http.MethodDelete || req.path != "/api/v1/admin/devices/"+id.String() {
		t.Fatalf("request = %s %s", req.method, req.path)
	}
	if err := execCmd(t, "device", "revoke", "790047a8", "--url", srv.URL, "--token", "tok"); err == nil {
		t.Fatal("revoke with a truncated id succeeded, want a parse error before any request")
	}
}
