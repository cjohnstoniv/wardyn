// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/google/uuid"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// outputServer answers GET /runs/{id}/output with status and the JSON body.
func outputServer(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runOutputCLI(t *testing.T, srv *httptest.Server, out io.Writer, args ...string) (stderr string, err error) {
	t.Helper()
	root := rootCmd()
	root.SetArgs(append([]string{"run", "output", uuid.NewString(), "--url", srv.URL, "--token", "tok"}, args...))
	errBuf := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errBuf)
	err = root.Execute()
	return errBuf.String(), err
}

func TestRunOutput_BytesUnchangedAndNotice(t *testing.T) {
	// A tail that starts mid UTF-8 sequence (JSON carries the cut bytes as U+FFFD) and has
	// no trailing newline: written back byte for byte.
	tail := "\ufffd\ufffd tail \x1b[31mred\x1b[0m"
	for _, tc := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"truncated", map[string]any{"truncated": true, "complete": true, "source": "stdout"}, "truncated"},
		{"incomplete", map[string]any{"incomplete": true, "complete": true, "source": "stdout"}, "incomplete"},
		{"capture gap", map[string]any{"capture_gap": true, "complete": true, "source": "stdout"}, "capture gap"},
		{"pane snapshot", map[string]any{"complete": true, "source": "pane_snapshot"}, "pane snapshot"},
		{"globals only", map[string]any{"complete": true, "source": "stdout", "mask_scope": "globals_only"}, "global secrets only"},
		{"clean", map[string]any{"complete": true, "source": "stdout", "mask_scope": "run"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.body["output"] = tail
			var out bytes.Buffer
			stderr, err := runOutputCLI(t, outputServer(t, 200, tc.body), &out)
			if err != nil {
				t.Fatal(err)
			}
			if out.String() != tail {
				t.Errorf("stdout = %q, want %q", out.String(), tail)
			}
			if tc.want == "" {
				if stderr != "" {
					t.Errorf("stderr = %q, want none", stderr)
				}
			} else if !strings.Contains(stderr, tc.want) || strings.Count(stderr, "\n") != 1 {
				t.Errorf("stderr = %q, want one line naming %q", stderr, tc.want)
			}
		})
	}
}

func TestRunOutput_RefusalsNameReason(t *testing.T) {
	for reason, status := range map[string]int{
		"run_output_off": 409, "run_output_not_kept": 409, "run_output_expired": 410,
		"run_output_erased": 404, "run_output_interactive": 409,
	} {
		t.Run(reason, func(t *testing.T) {
			var out bytes.Buffer
			_, err := runOutputCLI(t, outputServer(t, status, map[string]string{"error": "no output", "reason": reason}), &out)
			if err == nil || !strings.Contains(err.Error(), reason) {
				t.Fatalf("err = %v, want it to name %s", err, reason)
			}
			if out.Len() != 0 {
				t.Errorf("stdout = %q, want empty", out.String())
			}
		})
	}
}

func TestRunOutput_JSONDecodesIntoSDK(t *testing.T) {
	body := map[string]any{"output": "hi", "truncated": true, "complete": true, "source": "stdout",
		"incomplete": true, "capture_gap": false, "mask_scope": "run", "captured_at": "2026-10-03T01:02:03Z"}
	var out bytes.Buffer
	if _, err := runOutputCLI(t, outputServer(t, 200, body), &out, "--json"); err != nil {
		t.Fatal(err)
	}
	var got sdk.RunOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Output != "hi" || !got.Truncated || !got.Incomplete || got.MaskScope != "run" || got.CapturedAt == nil {
		t.Errorf("got %+v", got)
	}
}

func TestRunOutput_TailQuery(t *testing.T) {
	var q string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"output":"x","complete":true,"source":"stdout"}`))
	}))
	defer srv.Close()
	if _, err := runOutputCLI(t, srv, &bytes.Buffer{}, "--tail", "50"); err != nil || q != "tail=50" {
		t.Fatalf("err=%v query=%q, want tail=50", err, q)
	}
}

func TestEscapeControls(t *testing.T) {
	got := escapeControls("a\n\r\tb\x1b]52;c;AAAA\x07\x1b[31m\u009b\xff")
	want := `a` + "\n\r\t" + `b\x1b]52;c;AAAA\x07\x1b[31m\x9b\xff`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// openPTY returns a pty's slave side as an *os.File that term.IsTerminal
// accepts, and drains the master into the returned buffer.
func openPTY(t *testing.T) (slave *os.File, read func() string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	var unlock, n int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		t.Skipf("unlockpt: %v", e)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		t.Skipf("ptsname: %v", e)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("open slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	// Raw mode so the line discipline does not rewrite the bytes.
	var tio syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&tio))); e == 0 {
		tio.Oflag &^= syscall.OPOST
		syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TCSETS, uintptr(unsafe.Pointer(&tio)))
	}
	return slave, func() string {
		buf := make([]byte, 4096)
		k, _ := master.Read(buf)
		return string(buf[:k])
	}
}

func TestRunOutput_TerminalEscapesUnlessRaw(t *testing.T) {
	const payload = "x\x1b]52;c;QUJD\x07y\x1b[31mred\x1b[0m\n"
	body := map[string]any{"output": payload, "complete": true, "source": "stdout"}
	srv := outputServer(t, 200, body)

	slave, read := openPTY(t)
	if _, err := runOutputCLI(t, srv, slave); err != nil {
		t.Fatal(err)
	}
	got := read()
	if strings.ContainsRune(got, 0x1b) || !strings.Contains(got, `\x1b]52;c;QUJD\x07y\x1b[31mred\x1b[0m`) {
		t.Errorf("pty output = %q, want escaped", got)
	}

	slave2, read2 := openPTY(t)
	if _, err := runOutputCLI(t, srv, slave2, "--raw"); err != nil {
		t.Fatal(err)
	}
	if got := read2(); got != payload {
		t.Errorf("--raw pty output = %q, want verbatim", got)
	}

	// A pane snapshot is filtered even with --raw.
	snap := outputServer(t, 200, map[string]any{"output": payload, "complete": true, "source": "pane_snapshot"})
	slave3, read3 := openPTY(t)
	if _, err := runOutputCLI(t, snap, slave3, "--raw"); err != nil {
		t.Fatal(err)
	}
	if got := read3(); strings.ContainsRune(got, 0x1b) {
		t.Errorf("pane snapshot with --raw = %q, want escaped", got)
	}
}
