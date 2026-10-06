// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

// TestExecInspectRaw_KeepsTheNullExitCode drives the real client against the
// wire shapes a daemon sends: the null exit code of an exec that has not
// exited must survive (the client's own inspect flattens it to 0), on the
// negotiated API version, and a 404 must stay a not-found.
func TestExecInspectRaw_KeepsTheNullExitCode(t *testing.T) {
	bodies := map[string]string{
		"unstarted":  `{"ID":"unstarted","Running":false,"ExitCode":null,"Pid":0}`,
		"running":    `{"ID":"running","Running":true,"ExitCode":null,"Pid":77}`,
		"exited0":    `{"ID":"exited0","Running":false,"ExitCode":0,"Pid":77}`,
		"exited42":   `{"ID":"exited42","Running":false,"ExitCode":42,"Pid":77}`,
		"podmanExit": `{"ID":"podmanExit","Running":false,"ExitCode":0,"Pid":0,"DetachKeys":""}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("Api-Version", "1.44")
			return
		}
		id, ok := strings.CutPrefix(r.URL.Path, "/v1.44/exec/")
		id, ok2 := strings.CutSuffix(id, "/json")
		body, known := bodies[id]
		if !ok || !ok2 || !known {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"No such exec instance: `+r.URL.Path+`"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	cli, err := client.New(client.WithHost("tcp://" + strings.TrimPrefix(srv.URL, "http://")))
	if err != nil {
		t.Fatalf("client.New: %v", err)
	}
	defer cli.Close()
	c := newEngineClient(cli)
	ctx := context.Background()

	for id, want := range map[string]struct {
		code   int
		exited bool
	}{
		"unstarted":  {0, false},
		"running":    {0, false},
		"exited0":    {0, true},
		"exited42":   {42, true},
		"podmanExit": {0, true},
	} {
		insp, err := c.ExecInspectRaw(ctx, id)
		if err != nil {
			t.Errorf("%s: %v", id, err)
			continue
		}
		if code, exited := insp.exited(); code != want.code || exited != want.exited {
			t.Errorf("%s: exited() = %d, %v; want %d, %v (inspect %+v)", id, code, exited, want.code, want.exited, insp)
		}
	}
	if _, err := c.ExecInspectRaw(ctx, "gone"); !isNotFound(err) {
		t.Errorf("a missing exec = %v, want a not-found", err)
	}
}
