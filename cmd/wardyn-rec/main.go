// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command wardyn-rec is Wardyn's per-workspace PTY session recorder. It runs
// INSIDE the agent container as a thin wrapper around the agent process.
//
// License hygiene: asciinema is GPL. wardyn-rec only ever *execs* it as a
// subprocess (never linked), so Wardyn stays Apache-2.0. When asciinema is not
// present on PATH, wardyn-rec falls back to piping combined output to a plain
// .log file so a recording always exists.
//
// Delivery modes (after the recording is captured):
//
//  1. -out-dir <path>    Copy the finished cast file into path/<run-id>.cast
//     (or .log for the fallback). This is the shared-volume
//     path used in compose/kind deployments.
//  2. -upload-url <url>  HTTP PUT the cast to <url> with a Bearer token from
//     -run-token (or WARDYN_RUN_TOKEN env). Used when the
//     control plane is reachable from the agent container.
//
// The two modes are NOT exclusive; both may be set. If neither is set the
// recording is left in -cast-dir (the original behavior).
//
// Usage:
//
//	wardyn-rec -cast-dir /var/log/wardyn -run <uuid> [options] -- <agent argv...>
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "wardyn-rec:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("wardyn-rec", flag.ContinueOnError)
	castDir := fs.String("cast-dir", "/var/log/wardyn", "directory for in-progress recordings")
	runID := fs.String("run", "", "run id (required)")
	// asciinemaBin lets tests point at a stub; defaults to PATH lookup.
	asciinemaBin := fs.String("asciinema", "asciinema", "asciinema binary name (PATH lookup)")
	// Delivery flags.
	outDir := fs.String("out-dir", "", "copy finished recording into this shared-volume directory")
	uploadURL := fs.String("upload-url", "", "PUT finished cast to this URL (control-plane endpoint)")
	runToken := fs.String("run-token", os.Getenv("WARDYN_RUN_TOKEN"), "bearer token for -upload-url auth (env: WARDYN_RUN_TOKEN)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	agentArgv := fs.Args()
	if len(agentArgv) == 0 {
		return fmt.Errorf("no agent argv after flags (use: wardyn-rec [flags] -- <argv>)")
	}
	if *runID == "" {
		return fmt.Errorf("-run is required")
	}
	if err := os.MkdirAll(*castDir, 0o750); err != nil {
		return fmt.Errorf("create cast dir: %w", err)
	}

	// Determine recording mode and output path.
	if path, err := exec.LookPath(*asciinemaBin); err == nil {
		// asciinema path: exec replaces this process, so we must capture the
		// cast to a file first and post-process it via a wrapper. Instead of
		// using syscall.Exec (which never returns), we run asciinema as a
		// child when delivery options are set so we can upload afterwards.
		cast := castFile(*castDir, *runID)
		if *outDir == "" && *uploadURL == "" {
			// No delivery: original fast-path — exec asciinema directly.
			return execAsciinema(path, cast, agentArgv)
		}
		exitCode, err := runAsciinema(path, cast, agentArgv)
		if err != nil {
			return err
		}
		// Delivery MUST run before we propagate the agent's exit code: a failed
		// copy/upload (e.g. the proxy denies the brokered:recording route in
		// host-mode) is a recording problem, not a task failure — log it and
		// still exit with the agent's real outcome below.
		if derr := deliver(cast, *runID, *outDir, *uploadURL, *runToken); derr != nil {
			fmt.Fprintln(os.Stderr, "wardyn-rec: recording delivery failed (non-fatal):", derr)
		}
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return nil
	}

	// Fallback: plain log capture.
	log := logFile(*castDir, *runID)
	exitCode, err := recordToLog(log, agentArgv)
	if err != nil {
		return err
	}
	if *outDir == "" && *uploadURL == "" {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return nil
	}
	// Best-effort delivery (see the asciinema path): never fail an otherwise
	// successful agent run because the recording could not be copied/uploaded.
	if derr := deliver(log, *runID, *outDir, *uploadURL, *runToken); derr != nil {
		fmt.Fprintln(os.Stderr, "wardyn-rec: recording delivery failed (non-fatal):", derr)
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
	return nil
}

func castFile(dir, runID string) string { return filepath.Join(dir, runID+".cast") }
func logFile(dir, runID string) string  { return filepath.Join(dir, runID+".log") }

// execAsciinema replaces the current process with asciinema (no return on
// success). Used when no post-recording delivery is needed.
func execAsciinema(asciinemaPath, cast string, agentArgv []string) error {
	argv := buildAsciinemaArgv(asciinemaPath, cast, agentArgv)
	if err := syscall.Exec(asciinemaPath, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec asciinema: %w", err)
	}
	return nil
}

// runAsciinema runs asciinema as a child process (not exec) so we can deliver
// the cast file after it exits. The returned int is the agent's exit code (0
// on success); the caller must run delivery before propagating it via
// os.Exit, so a non-zero agent exit is reported here rather than exited
// directly. A non-nil error means asciinema itself could not be run.
func runAsciinema(asciinemaPath, cast string, agentArgv []string) (int, error) {
	argv := buildAsciinemaArgv(asciinemaPath, cast, agentArgv)
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if asExit(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 0, fmt.Errorf("asciinema: %w", err)
	}
	return 0, nil
}

func buildAsciinemaArgv(asciinemaPath, cast string, agentArgv []string) []string {
	return []string{
		asciinemaPath, "rec",
		"--stdin",
		"-q",
		"-c", strings.Join(quoteArgs(agentArgv), " "),
		cast,
	}
}

// recordToLog runs the agent argv directly, tee-ing combined output to a log
// file and the caller's stdout/stderr. Used when asciinema is unavailable.
// The returned int is the agent's exit code (0 on success); see runAsciinema
// for why a non-zero exit is returned rather than exited directly here.
func recordToLog(logPath string, agentArgv []string) (int, error) {
	f, err := os.Create(logPath)
	if err != nil {
		return 0, fmt.Errorf("create log: %w", err)
	}
	defer f.Close()

	cmd := exec.Command(agentArgv[0], agentArgv[1:]...) //nolint:gosec
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(os.Stdout, f)
	cmd.Stderr = io.MultiWriter(os.Stderr, f)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if asExit(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 0, fmt.Errorf("run agent: %w", err)
	}
	return 0, nil
}

// deliver copies/uploads the finished recording file. Both modes may be active.
func deliver(srcPath, runID, outDir, uploadURL, runToken string) error {
	var errs []string

	if outDir != "" {
		if err := copyToDir(srcPath, outDir); err != nil {
			errs = append(errs, fmt.Sprintf("out-dir: %v", err))
		}
	}
	if uploadURL != "" {
		if err := uploadCast(srcPath, uploadURL, runToken); err != nil {
			errs = append(errs, fmt.Sprintf("upload: %v", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("deliver: %s", strings.Join(errs, "; "))
	}
	return nil
}

// copyToDir copies srcPath into dstDir, keeping only the base filename.
func copyToDir(srcPath, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0o750); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	dst := filepath.Join(dstDir, filepath.Base(srcPath))
	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer src.Close()

	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dst: %w", err)
	}
	defer f.Close()
	_, err = io.Copy(f, src)
	return err
}

// uploadCast PUTs the recording file to uploadURL with Bearer auth. The server
// is expected to return 2xx on success. Retries are not attempted to keep the
// sidecar simple and dependency-free; callers that require reliability should
// use -out-dir with a separate upload agent.
func uploadCast(srcPath, uploadURL, runToken string) error {
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read cast: %w", err)
	}

	client := &http.Client{Timeout: 60 * time.Second}
	req, err := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-asciicast")
	if runToken != "" {
		req.Header.Set("Authorization", "Bearer "+runToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http PUT: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// quoteArgs shell-quotes each arg so the joined `-c` command string that
// asciinema runs via `$SHELL -c` both preserves argument boundaries AND can
// never be interpreted as shell syntax. This is security-critical: agentArgv
// can contain untrusted run-task/command data, so any byte with shell meaning
// ($(), backticks, ;, |, &, >, <, *, whitespace, newlines, ...) MUST be
// neutralized. A token is emitted verbatim only when it is composed solely of
// known shell-safe characters; otherwise it is single-quoted with embedded
// single quotes escaped via the `'\”` idiom. Inside single quotes a POSIX
// shell treats every other byte literally, so quoted tokens cannot inject.
func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return out
}

// shellQuote returns a single POSIX-shell token that expands back to exactly s.
// Fully shell-safe (and non-empty) strings are returned unquoted for
// readability; everything else — including the empty string and anything with
// shell metacharacters — is wrapped in single quotes with embedded single
// quotes escaped as '\”.
func shellQuote(s string) string {
	if s != "" && isShellSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// isShellSafe reports whether every rune of s is drawn from a conservative set
// of characters that carry no special meaning to a POSIX shell and therefore
// need no quoting. Anything outside this set (including all shell
// metacharacters and whitespace) forces quoting.
func isShellSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			// alphanumeric: safe
		case r == '@' || r == '%' || r == '+' || r == '=' || r == ':' ||
			r == ',' || r == '.' || r == '/' || r == '-' || r == '_':
			// safe punctuation
		default:
			return false
		}
	}
	return true
}

// asExit is a tiny errors.As wrapper kept local to avoid an import just for
// the type assertion at one call site.
func asExit(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}
