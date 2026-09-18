// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
	"github.com/google/uuid"
)

func TestCLIExports_PreserveUnrelatedPartFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{\"version\":2}\n"))
	}))
	t.Cleanup(srv.Close)
	for _, export := range []struct {
		name  string
		write func(string) error
	}{
		{"recording", func(path string) error {
			return execCmd(t, "run", "recording", uuid.NewString(), "-o", path, "--url", srv.URL, "--token", "test")
		}},
		{"bundle", func(path string) error {
			return writeTarGz(path, map[string][]byte{"note.txt": []byte("synthetic diagnostic")})
		}},
	} {
		t.Run(export.name, func(t *testing.T) {
			for _, kind := range []string{"regular", "symlink", "rename_failure"} {
				t.Run(kind, func(t *testing.T) {
					dir := t.TempDir()
					path := filepath.Join(dir, "export")
					legacyPart := path + ".part"
					fixture := filepath.Join(dir, "unrelated.txt")
					const keep = "unrelated file must survive"
					writeFile(t, fixture, keep)
					if kind == "symlink" {
						if err := os.Symlink(fixture, legacyPart); err != nil {
							t.Fatal(err)
						}
					} else {
						writeFile(t, legacyPart, keep)
						if err := os.Chmod(legacyPart, 0o644); err != nil {
							t.Fatal(err)
						}
					}
					if kind == "rename_failure" {
						if err := os.Mkdir(path, 0o700); err != nil {
							t.Fatal(err)
						}
					}
					err := export.write(path)
					if (err != nil) != (kind == "rename_failure") {
						t.Fatalf("export error = %v for %s", err, kind)
					}
					for _, p := range []string{fixture, legacyPart} {
						if got, err := os.ReadFile(p); err != nil || string(got) != keep {
							t.Errorf("unrelated path changed: %s = %q, %v", p, got, err)
						}
					}
					if kind != "rename_failure" {
						info, err := os.Lstat(path)
						if err != nil {
							t.Fatal(err)
						}
						if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
							t.Errorf("export must be a private regular file, mode = %v", info.Mode())
						}
					}
					entries, err := os.ReadDir(dir)
					if err != nil || len(entries) != 3 {
						t.Errorf("expected only export and the two preserved paths, entries=%v, err=%v", entries, err)
					}
				})
			}
		})
	}
}

func TestRunRecording_ConcurrentExportsKeepIndependentTempFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.cast")
	write := func(body io.ReadCloser) error {
		cmd := runRecordingCmd(func() *sdk.Client {
			return &sdk.Client{BaseURL: "http://recording.test", HTTPClient: &http.Client{
				Transport: exportResponseTransport{body},
			}}
		})
		cmd.SetArgs([]string{uuid.NewString(), "-o", path})
		return cmd.Execute()
	}
	reader, writer := io.Pipe()
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	done := make(chan error, 1)
	go func() { done <- write(reader) }()
	// The first read starts only after the download has opened its temp file.
	if _, err := writer.Write([]byte("first-")); err != nil {
		t.Fatal(err)
	}
	if err := write(io.NopCloser(strings.NewReader("second"))); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("complete")); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("first download interfered with second: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("first download did not finish")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "first-complete" {
		t.Fatalf("last completed download = %q, %v", got, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files remain after downloads: %v, %v", entries, err)
	}
}

type exportResponseTransport struct{ body io.ReadCloser }

func (t exportResponseTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: t.body}, nil
}
