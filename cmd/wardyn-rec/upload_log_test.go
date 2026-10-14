// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// logPut is what the fake control plane saw of one PUT.
type logPut struct {
	n      int64 // bytes received
	length int64 // declared Content-Length
	sum    [sha256.Size]byte
	head   []byte // first bytes of the body
	tail   []byte // last bytes of the body
	token  string
}

// logServer answers every PUT with status and records it. It reads the body
// in full, as the real handler does, and hashes it rather than keeping it.
func logServer(t *testing.T, status int) (*httptest.Server, *[]logPut) {
	t.Helper()
	var puts []logPut
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := sha256.New()
		p := logPut{length: r.ContentLength, token: r.Header.Get("Authorization")}
		var buf [4096]byte
		for {
			k, err := r.Body.Read(buf[:])
			if k > 0 {
				h.Write(buf[:k])
				if p.head == nil {
					p.head = append([]byte(nil), buf[:min(k, 16)]...)
				}
				p.n += int64(k)
				p.tail = append(p.tail, buf[:k]...)
				if len(p.tail) > 1024 {
					p.tail = p.tail[len(p.tail)-1024:]
				}
			}
			if err != nil {
				break
			}
		}
		copy(p.sum[:], h.Sum(nil))
		puts = append(puts, p)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &puts
}

// sparseLog makes a log of size bytes: zeros (sparse on disk) with a
// recognisable first and last line, and no other newline.
func sparseLog(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.log")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("FIRST-LINE\n"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("LAST-LINE"), size-9); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileSum(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	var s [sha256.Size]byte
	copy(s[:], h.Sum(nil))
	return s
}

// A small log reaches the server byte for byte, with its length declared.
func TestUploadLog_SmallParity(t *testing.T) {
	srv, puts := logServer(t, http.StatusNoContent)
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := uploadLog(path, srv.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	if len(*puts) != 1 {
		t.Fatalf("got %d PUTs, want 1", len(*puts))
	}
	p := (*puts)[0]
	if p.n != 12 || p.length != 12 || p.sum != fileSum(t, path) || p.token != "Bearer tok" {
		t.Errorf("small log: n=%d length=%d token=%q, want 12/12/Bearer tok with identical bytes", p.n, p.length, p.token)
	}
}

// Logs the single-upload contract accepts today, 33 MiB and exactly the cap,
// upload complete: no prefix, no marker.
func TestUploadLog_WithinCapUploadsComplete(t *testing.T) {
	for _, size := range []int64{33 << 20, maxLogUploadBytes} {
		srv, puts := logServer(t, http.StatusNoContent)
		path := sparseLog(t, size)
		if err := uploadLog(path, srv.URL, ""); err != nil {
			t.Fatalf("%d bytes: %v", size, err)
		}
		p := (*puts)[0]
		if p.n != size || p.length != size || p.sum != fileSum(t, path) {
			t.Errorf("%d bytes: server got n=%d length=%d, want the file complete", size, p.n, p.length)
		}
		if !bytes.HasSuffix(p.tail, []byte("LAST-LINE")) {
			t.Errorf("%d bytes: body does not end with the log's last line", size)
		}
	}
}

// Over the cap the server would refuse the lot, so a bounded prefix goes up
// instead, ends in a marker that says it is cut, and the file is untouched.
// The single 70 MiB line (no newline in it) takes the same path.
func TestUploadLog_OverCapSendsMarkedPrefix(t *testing.T) {
	for _, size := range []int64{maxLogUploadBytes + 1, 70 << 20} {
		srv, puts := logServer(t, http.StatusNoContent)
		path := sparseLog(t, size)
		before := fileSum(t, path)
		if err := uploadLog(path, srv.URL, ""); err != nil {
			t.Fatalf("%d bytes: %v", size, err)
		}
		p := (*puts)[0]
		if p.n >= maxLogUploadBytes || p.n != p.length {
			t.Errorf("%d bytes: body %d (declared %d), want under the %d cap", size, p.n, p.length, maxLogUploadBytes)
		}
		if !bytes.HasPrefix(p.head, []byte("FIRST-LINE\n")) {
			t.Errorf("%d bytes: body does not start with the log's start: %q", size, p.head)
		}
		tail := string(p.tail)
		if !strings.Contains(tail, "log truncated") || !strings.HasSuffix(tail, "the rest was not]\n") {
			t.Errorf("%d bytes: body does not end in the truncation marker: %q", size, tail[max(0, len(tail)-200):])
		}
		if strings.Contains(tail, "LAST-LINE") {
			t.Errorf("%d bytes: the cut tail must not be presented as sent", size)
		}
		if fileSum(t, path) != before {
			t.Errorf("%d bytes: the local log was modified", size)
		}
	}
}

// A refusal reaches the caller as an error, and delivery stays non-fatal with
// the local log intact.
func TestUploadLog_ServerRefusal(t *testing.T) {
	srv, puts := logServer(t, http.StatusRequestEntityTooLarge)
	path := sparseLog(t, maxLogUploadBytes+1)
	err := uploadLog(path, srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "413") {
		t.Fatalf("uploadLog err = %v, want the 413", err)
	}
	if len(*puts) != 1 {
		t.Errorf("got %d PUTs, want one (no retry)", len(*puts))
	}
	if err := deliverAndExit(path, "", func() error { return uploadLog(path, srv.URL, "") }, 0); err != nil {
		t.Errorf("deliverAndExit must stay non-fatal on a refusal, got %v", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != maxLogUploadBytes+1 {
		t.Errorf("local log must stay whole after a refusal: %v %v", fi, err)
	}
}

// A non-zero agent exit still uploads the log and is the process's exit code.
func TestRun_LogUploadKeepsAgentExit(t *testing.T) {
	srv, puts := logServer(t, http.StatusNoContent)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	args := []string{"-cast-dir", dir, "-run", "run-x", "-asciinema", "definitely-not-a-real-binary-xyz",
		"-upload-url", srv.URL, "--", "sh", "-c", "echo agent-output; exit 7"}
	c := exec.Command(exe, "-test.run=^TestRecHelper$") //nolint:gosec
	c.Env = append(os.Environ(), "WARDYN_REC_TEST_ARGS="+strings.Join(args, "\x1f"))
	_ = c.Run()
	if c.ProcessState.ExitCode() != 7 {
		t.Errorf("exit = %d, want the agent's 7", c.ProcessState.ExitCode())
	}
	srv.Close() // the upload came from another process: wait for the handler before reading what it stored
	if len(*puts) != 1 || !bytes.HasSuffix((*puts)[0].tail, []byte("agent-output\n")) {
		t.Errorf("the log of a failed agent must still upload: %+v", *puts)
	}
}
