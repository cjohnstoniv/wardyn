// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// THE PIN FOR "AN UNREACHABLE SERVER IS A FAILURE, NOT A SKIP".
//
// The audit-chain isolation pin used to call t.Skipf when it could not connect
// or ping, so a lane whose Postgres was simply down reported `--- SKIP` -> `ok`
// -> exit 0 with the ordering invariant never exercised — a green that means
// "not run" and reads exactly like a green that means "proven".
//
// A pin against that is worth only as much as its own counterfactual, and a
// source grep is not one: it survives the rename that reintroduces the hole.
// So this test RE-EXECUTES THE TEST BINARY and reads what the isolation pin
// actually does, in both directions:
//
//   - a DSN naming a server nobody is listening on   -> the child must FAIL
//   - no DSN at all (the substrate is genuinely absent) -> the child must SKIP
//
// The second direction matters as much as the first. Turning every skip into a
// failure would redden every lane without Postgres — including the ones this
// repo is routinely developed on — and "it never skips" is not the invariant.
// The invariant is that a skip reports an environment that CANNOT provide the
// substrate, never an environment that could and did not.
//
// It needs no database of its own: 127.0.0.1:1 is refused immediately.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// isolationPinChildMarker keeps a re-executed binary from re-entering this test.
const isolationPinChildMarker = "WARDYN_TEST_SKIP_DISCIPLINE_CHILD"

// isolationPinTarget is the test whose skip discipline is under examination.
const isolationPinTarget = "TestPG_InsertAuditEventDoesNotForkTheChainAtRepeatableRead"

// runIsolationPin re-runs THIS test binary against one WARDYN_TEST_PG value and
// returns the child's verbose output and exit code.
func runIsolationPin(t *testing.T, dsn string) (string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0],
		"-test.run=^"+isolationPinTarget+"$", "-test.v=true", "-test.count=1", "-test.timeout=60s")
	// Built explicitly rather than appended to os.Environ(), so which of two
	// entries for one name wins is never a question this test has to answer.
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		switch strings.SplitN(kv, "=", 2)[0] {
		case "WARDYN_TEST_PG", "WARDYN_TEST_PG_SUPERUSER", isolationPinChildMarker:
		default:
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, isolationPinChildMarker+"=1", "WARDYN_TEST_PG="+dsn)
	out, err := cmd.CombinedOutput()
	switch e := err.(type) {
	case nil:
		return string(out), 0
	case *exec.ExitError:
		return string(out), e.ExitCode()
	default:
		t.Fatalf("re-exec %s: %v", os.Args[0], err)
		return "", -1
	}
}

func TestAuditChainIsolationPinFailsRatherThanSkippingOnAnUnreachableServer(t *testing.T) {
	if os.Getenv(isolationPinChildMarker) == "1" {
		t.Skip("child process of the skip-discipline pin; it does not re-enter itself")
	}

	t.Run("a server that was named and cannot be reached is a FAILURE", func(t *testing.T) {
		out, code := runIsolationPin(t, "postgres://wardyn:wardyn@127.0.0.1:1/wardyn_probe?sslmode=disable")
		if strings.Contains(out, "--- SKIP: "+isolationPinTarget) || code == 0 {
			t.Fatalf("the isolation pin reported exit %d on an unreachable server that WARDYN_TEST_PG NAMED, and a "+
				"skip there is a green `ok` with the audit-chain ordering invariant never exercised — the finding "+
				"this pin closes. Child output:\n%s", code, out)
		}
		if !strings.Contains(out, "--- FAIL: "+isolationPinTarget) {
			t.Fatalf("the isolation pin exited %d on an unreachable server but did not report %s as failing; the "+
				"child died for some other reason and this pin is proving nothing. Child output:\n%s",
				code, isolationPinTarget, out)
		}
	})

	t.Run("a substrate that is genuinely absent is still a SKIP", func(t *testing.T) {
		out, code := runIsolationPin(t, "")
		if !strings.Contains(out, "--- SKIP: "+isolationPinTarget) || code != 0 {
			t.Fatalf("with no WARDYN_TEST_PG at all the isolation pin exited %d without skipping. Skipping what the "+
				"environment cannot provide is the ONE sanctioned skip; turning it into a failure reddens every lane "+
				"with no Postgres and is not what the finding asked for. Child output:\n%s", code, out)
		}
	})
}
