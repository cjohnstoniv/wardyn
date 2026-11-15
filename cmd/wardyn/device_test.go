// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
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
	root := rootCmd()
	out := &strings.Builder{}
	root.SetArgs([]string{"device", "list", "--url", srv.URL, "--token", "tok"})
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), id.String()) || !strings.Contains(out.String(), "alices-laptop") || !strings.Contains(out.String(), "active") {
		t.Fatalf("list output lacks the full id, name or status:\n%s", out.String())
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

func TestDeviceEnrolTokenListAndRevokeCmds(t *testing.T) {
	id := uuid.New()
	srv := newCmdServer(t, http.StatusOK, []types.DeviceEnrolmentToken{{ID: id, DeviceName: "alices-laptop", MintedBy: "admin"}})
	root := rootCmd()
	out := &strings.Builder{}
	root.SetArgs([]string{"device", "enrol-token-list", "--url", srv.URL, "--token", "tok"})
	root.SetOut(out)
	root.SetErr(&strings.Builder{})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), id.String()) || !strings.Contains(out.String(), "alices-laptop") {
		t.Fatalf("list output lacks the full id or device name:\n%s", out.String())
	}
	if req := srv.last(); req.method != http.MethodGet || req.path != "/api/v1/admin/devices/enrolment-tokens" {
		t.Fatalf("request = %s %s", req.method, req.path)
	}

	srv = newCmdServer(t, http.StatusNoContent, nil)
	if err := execCmd(t, "device", "enrol-token-revoke", id.String(), "--url", srv.URL, "--token", "tok"); err != nil {
		t.Fatal(err)
	}
	if req := srv.last(); req.method != http.MethodDelete || req.path != "/api/v1/admin/devices/enrolment-tokens/"+id.String() {
		t.Fatalf("request = %s %s", req.method, req.path)
	}
	if err := execCmd(t, "device", "enrol-token-revoke", "790047a8", "--url", srv.URL, "--token", "tok"); err == nil {
		t.Fatal("revoke with a truncated id succeeded, want a parse error before any request")
	}
}

// A one-time token that cannot be written out exists on the server and nowhere else: each mint command
// fails after its one mint, names the token's id and how to revoke it, and prints the token nowhere else.
func TestMintCommandsFailWhenTheTokenCannotBeWritten(t *testing.T) {
	id := uuid.New()
	exp := time.Now().Add(72 * time.Hour)
	for _, tc := range []struct {
		args   []string
		body   any
		revoke string
	}{
		{[]string{"device", "enrol-token", "--name", "laptop"}, types.DeviceEnrolmentToken{ID: id, DeviceName: "laptop", ExpiresAt: exp, Token: "wde_secret"}, "wardyn device enrol-token-revoke " + id.String()},
		{[]string{"runner", "token"}, types.RunnerRegistrationToken{ID: id, Token: "wdr_secret", ExpiresAt: exp}, "wardyn runner tokens revoke " + id.String()},
		{[]string{"runner", "org-token", "mint", "--name", "cluster-a"}, types.RunnerRegistrationToken{ID: id, Token: "wdr_secret", OwnerKind: types.RunnerOwnerOrg, ExpiresAt: exp}, "wardyn runner tokens revoke " + id.String()},
	} {
		srv := newCmdServer(t, http.StatusCreated, tc.body)
		root := rootCmd()
		var stderr strings.Builder
		root.SetArgs(append(tc.args, "--url", srv.URL, "--token", "tok"))
		root.SetOut(failingWriter{})
		root.SetErr(&stderr)
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "was minted but could not be written out") || !strings.Contains(err.Error(), tc.revoke) {
			t.Errorf("%v: err = %v, want the mint named with %q", tc.args, err, tc.revoke)
			continue
		}
		if strings.Contains(err.Error(), "_secret") || strings.Contains(stderr.String(), "_secret") {
			t.Errorf("%v: the token reached another stream: err %v, stderr %q", tc.args, err, stderr.String())
		}
		if n := len(srv.reqs); n != 1 || srv.last().method != http.MethodPost {
			t.Errorf("%v: %d requests, want exactly one mint", tc.args, n)
		}
	}
}

// failingWriter is a stdout that cannot be written, as a full disk or a closed pipe is.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }
