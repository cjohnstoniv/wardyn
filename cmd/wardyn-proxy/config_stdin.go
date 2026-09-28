// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"
)

// configStdinEnv, set to "1", says the config arrives on stdin: the Docker
// driver writes it there once at start (#1176), so the container's config and
// environment never hold the run token, the MITM CA key or the upstream-proxy
// credential inside it.
const configStdinEnv = "WARDYN_PROXY_CONFIG_STDIN"

// stdinConfigTimeout bounds the wait for it. The driver writes right after the
// start, so a proxy still waiting was started by something else (a manual
// `docker start` of a stopped one) and must not run.
const stdinConfigTimeout = 30 * time.Second

// readStdinConfig reads the config from r to EOF. None (an empty stream, or
// nothing within timeout) is an error: the proxy exits non-zero rather than
// run without its policy.
func readStdinConfig(r io.Reader, timeout time.Duration) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(r)
		ch <- result{b, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			return nil, fmt.Errorf("read config from stdin: %w", res.err)
		}
		if len(bytes.TrimSpace(res.b)) == 0 {
			return nil, errors.New("no config arrived on stdin")
		}
		return res.b, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("no config arrived on stdin within %s", timeout)
	}
}
