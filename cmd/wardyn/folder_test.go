// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func folderBringBack(t *testing.T, status int, body string) (string, string, error) { // stdout, stderr, err
	t.Helper()
	id := uuid.New()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path + " " + r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	root := rootCmd()
	var out, errOut strings.Builder
	root.SetArgs([]string{"folder", "bring-back", "--url", srv.URL, "--token", "tok", id.String()})
	root.SetOut(&out)
	root.SetErr(&errOut)
	err := root.Execute()
	if want := "POST /api/v1/runs/" + id.String() + "/computer-folder/bring-back Bearer tok"; got != want {
		t.Errorf("request = %q, want %q", got, want)
	}
	return out.String(), errOut.String(), err
}

func TestFolderBringBack_PrintsWhatCameBack(t *testing.T) {
	out, _, err := folderBringBack(t, http.StatusOK,
		`{"state":"STOPPED","sandbox_removed":true,"back":{"at":"2026-10-22T14:32:00Z","files":12,"bytes":900,"refused":[{"path":"secrets.env","reason":"denied"}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"left out: secrets.env (denied)\n", "12 files came back to you.\n", folderCopy.deleted} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q lacks %q", out, want)
		}
	}
}

func TestFolderBringBack_SaysWhenTheSandboxWasNotDeleted(t *testing.T) {
	out, errOut, err := folderBringBack(t, http.StatusOK,
		`{"state":"STOPPED","sandbox_removed":false,"back":{"at":"2026-10-22T14:32:00Z","files":3,"refused":[]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "3 files came back to you.\n") || strings.Contains(out, folderCopy.deleted) || errOut != folderCopy.progress+folderCopy.notDeleted {
		t.Fatalf("stdout %q, stderr %q: want the folder's line, and the sandbox not claimed deleted", out, errOut)
	}
}

func TestFolderBringBack_AFailedPassIsAnError(t *testing.T) {
	out, _, err := folderBringBack(t, http.StatusOK, `{"state":"STOPPED","back":{"at":"2026-10-22T14:32:00Z","files":0,"refused":[],"error":"the runner went offline"}}`)
	if err == nil || !strings.Contains(err.Error(), "the runner went offline") || strings.Contains(out, "came back to you") {
		t.Fatalf("err %v, output %q: want the pass's error and no success line", err, out)
	}
}

func TestFolderBringBack_ShowsTheServersRefusal(t *testing.T) {
	_, _, err := folderBringBack(t, http.StatusConflict,
		`{"error":"This run is no longer running, so its folder can't come back. Killing a run doesn't bring the folder back.","reason":"computer_folder_not_running"}`)
	if err == nil || !strings.Contains(err.Error(), "Killing a run doesn't bring the folder back.") {
		t.Fatalf("err = %v, want the server's refusal", err)
	}
}
