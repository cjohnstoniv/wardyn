// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// rollingCollector keeps a bounded HEAD (first headCap bytes) and TAIL (last
// tailCap bytes) of a command's combined output, so a long build log's setup
// context (head) and its failure (tail) both survive truncation. It is an
// io.Writer fed by cmd.Stdout+Stderr. Cline's createRollingCollector pattern.
type rollingCollector struct {
	headCap, tailCap int
	headBuf          []byte
	tailBuf          []byte // ring: last tailCap bytes
	total            int
}

func newRollingCollector(headCap, tailCap int) *rollingCollector {
	return &rollingCollector{headCap: headCap, tailCap: tailCap}
}

func (c *rollingCollector) Write(p []byte) (int, error) {
	n := len(p)
	c.total += n
	// Head: append until full.
	if len(c.headBuf) < c.headCap {
		room := c.headCap - len(c.headBuf)
		if room > len(p) {
			room = len(p)
		}
		c.headBuf = append(c.headBuf, p[:room]...)
	}
	// Tail: keep the last tailCap bytes.
	c.tailBuf = append(c.tailBuf, p...)
	if len(c.tailBuf) > c.tailCap {
		c.tailBuf = c.tailBuf[len(c.tailBuf)-c.tailCap:]
	}
	return n, nil
}

func (c *rollingCollector) head() string { return string(c.headBuf) }

// tail returns the tail only when the output overflowed the head (otherwise the
// head already holds everything, and returning the tail too would duplicate).
func (c *rollingCollector) tail() string {
	if c.total <= c.headCap {
		return ""
	}
	return string(c.tailBuf)
}
