// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// narrow exit codes: 0 an answer was printed, 3 nothing was delivered, 4 the request may have been.
const (
	narrowExitNoListener = 3
	narrowExitUncertain  = 4
)

const (
	narrowDialTimeout  = 2 * time.Second
	narrowTotalTimeout = 10 * time.Second
)

// runNarrow is wardyn-proxy -narrow: it sends the live deny request on stdin to the running proxy of this
// pod and prints its one-line answer. It reads no config and no list file. The proxy refuses a request
// over proxy.MaxNarrowMessageBytes, so one more byte than that is sent as read and no rule is kept here.
func runNarrow(stdin io.Reader, stdout io.Writer) int {
	req, err := io.ReadAll(io.LimitReader(stdin, proxy.MaxNarrowMessageBytes+1))
	if err != nil {
		slog.Error("wardyn-proxy: narrow: read the request", slog.Any("err", err))
		return narrowExitNoListener
	}
	dir := os.Getenv(proxy.LiveDenyDirEnv)
	if dir == "" {
		slog.Error("wardyn-proxy: narrow: " + proxy.LiveDenyDirEnv + " is not set")
		return narrowExitNoListener
	}
	conn, err := net.DialTimeout("unix", proxy.NarrowSocketPath(dir), narrowDialTimeout)
	if err != nil {
		slog.Error("wardyn-proxy: narrow: no listener", slog.Any("err", err))
		return narrowExitNoListener
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(narrowTotalTimeout))
	if _, err := conn.Write(req); err != nil {
		slog.Error("wardyn-proxy: narrow: write the request", slog.Any("err", err))
		return narrowExitUncertain
	}
	// The proxy reads the request up to this half-close.
	if uc, ok := conn.(*net.UnixConn); ok {
		if err := uc.CloseWrite(); err != nil {
			slog.Error("wardyn-proxy: narrow: end the request", slog.Any("err", err))
			return narrowExitUncertain
		}
	}
	line, err := io.ReadAll(io.LimitReader(conn, proxy.MaxNarrowMessageBytes+1))
	if err != nil {
		slog.Error("wardyn-proxy: narrow: read the answer", slog.Any("err", err))
		return narrowExitUncertain
	}
	if len(line) > proxy.MaxNarrowMessageBytes {
		slog.Error("wardyn-proxy: narrow: the answer is over the bound", slog.Int("bytes", len(line)))
		return narrowExitUncertain
	}
	if line = bytes.TrimRight(line, "\r\n"); !json.Valid(line) {
		slog.Error("wardyn-proxy: narrow: the answer is not JSON", slog.Int("bytes", len(line)))
		return narrowExitUncertain
	}
	if _, err := stdout.Write(append(line, '\n')); err != nil {
		slog.Error("wardyn-proxy: narrow: write the answer", slog.Any("err", err))
		return narrowExitUncertain
	}
	return 0
}

// applyLiveDenies adds the live deny list kept in dir to the config bytes raw. It answers the bytes the
// proxy runs on and the bytes before the list, which a narrowing keeps. With no list file the bytes are
// raw itself. An unreadable or invalid list is an error, so the proxy does not start behind one.
func applyLiveDenies(raw []byte, dir string) (eff, base []byte, err error) {
	list, err := proxy.ReadLiveDenyFile(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("live deny list in %s: %w", dir, err)
	}
	eff, err = proxy.WithLiveDenies(raw, list)
	if err != nil {
		return nil, nil, fmt.Errorf("live deny list in %s: %w", dir, err)
	}
	return eff, raw, nil
}
