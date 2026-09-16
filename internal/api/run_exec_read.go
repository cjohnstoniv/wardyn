// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "io"

// readExecStdout reads at most limit bytes from an exec session's Stdout and
// reports whether the stream had MORE to give.
//
// THE `capped` RETURN IS THE POINT, and it is why this is a shared helper
// rather than two io.LimitReader calls. Both readers of a sandbox exec stream
// (the Files widget and the Resources widget) bound what they read, because the
// sandbox's stdout is not something wardynd controls. But an ExecSession's
// Stdout is an UNBUFFERED io.Pipe fed by ONE demux goroutine (runner.ExecSession's
// doc), so leaving bytes in it does not merely discard them: the in-sandbox
// process blocks on write, the demux goroutine blocks with it, and Wait — which
// both endpoints called next — can never return. The docker driver's Wait
// selects on the 5 s handler ctx, so what the operator actually saw was a
// five-second stall, a 500, and a run.files FAILURE row per poll tick, for a
// workspace whose only crime was a chatty `git status` (B1-F4).
//
// Draining to EOF instead would contradict the cap's own rationale (the cap
// exists so a chatty sandbox cannot park this handler). So the caller SKIPS
// Wait when capped is true and lets the deferred Close tear the exec down,
// which releases the demux goroutine and the blocked writer with it.
//
// capped is also the honest answer to "is this list complete?" — before this,
// byte-cap truncation left truncated=false on the wire, i.e. a short list
// presented as the whole truth.
//
// err is only ever a real read failure: hitting the limit is reported through
// capped, never as an error, because ReadAll stops at the limit of its own
// accord.
func readExecStdout(r io.Reader, limit int64) (out []byte, capped bool, err error) {
	if r == nil {
		return nil, false, nil
	}
	// limit+1: the one extra byte is how "exactly limit bytes and EOF" is told
	// apart from "limit bytes and more waiting".
	out, err = io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(out)) > limit {
		return out[:limit], true, nil
	}
	return out, false, err
}
