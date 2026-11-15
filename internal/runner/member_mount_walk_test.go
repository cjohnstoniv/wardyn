// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/foldersync"
)

// openWithin is OpenScanSource at an operator's door, failing t when it has not returned in 10 s.
func openWithin(t *testing.T, p UserMountPolicy, src string, holders []ScanHolder) (*os.Root, error) {
	t.Helper()
	type result struct {
		dir *os.Root
		err error
	}
	done := make(chan result, 1)
	go func() {
		dir, err := p.OpenScanSource("", src, holders, foldersync.OpenFromTop)
		done <- result{dir, err}
	}()
	select {
	case r := <-done:
		return r.dir, r.err
	case <-time.After(10 * time.Second):
		t.Fatalf("OpenScanSource(%s) did not return in 10 s", src)
		return nil, nil
	}
}

// wantWalkGaveUp fails unless OpenScanSource refused src as a folder the walk could not follow, with no handle,
// not as a holder's narrowed roots, and without naming the holder's workspace.
func wantWalkGaveUp(t *testing.T, dir *os.Root, err error, src string) {
	t.Helper()
	if dir != nil {
		_ = dir.Close()
	}
	if dir != nil || !errors.Is(err, errMemberSourceRefused) || errors.As(err, new(*StaleWorkspaceError)) ||
		strings.Contains(err.Error(), "payroll") {
		t.Errorf("OpenScanSource(%s) = %v, %v; want no handle and the refusal for a folder the walk could not follow", src, dir, err)
	}
}

// An operator's link loop on the way to a folder ends the walk: the scan returns and opens nothing. With no member
// workspace attaching the folder it is left to the read by its path, which the loop fails too; with one attaching it,
// it is refused for the loop, not for that person's roots, and so is a name on the way that is missing.
func TestOpenScanSource_AnOperatorsLinkLoopReturnsAndOpensNothing(t *testing.T) {
	base := scanFixture(t, map[string]string{"members/app": "MEMBER_TOKEN"}, map[string]string{"loop-a": "loop-b", "loop-b": "loop-a"})
	p := UserMountPolicy{Roots: []string{filepath.Join(base, "members")}}
	alice := []ScanHolder{{Owner: "alice", Workspace: "payroll"}}
	src := filepath.Join(base, "loop-a", "app")
	if dir, err := openWithin(t, p, src, nil); dir != nil || err != nil {
		t.Errorf("a link loop no member workspace attaches = %v, %v; want no handle and no error", dir, err)
	}
	dir, err := openWithin(t, p, src, alice)
	wantWalkGaveUp(t, dir, err, src)
	missing := filepath.Join(base, "gone", "app")
	dir, err = openWithin(t, p, missing, alice)
	wantWalkGaveUp(t, dir, err, missing)
}

// The walk follows as many links as filepath.EvalSymlinks does, and no more: a chain of memberWalkLinks links into a
// member root names a folder of that root, which is opened; one link more is refused at an operator's door with a
// member workspace attaching it, as EvalSymlinks refuses that chain.
func TestOpenScanSource_TheWalkFollowsAsManyLinksAsEvalSymlinks(t *testing.T) {
	alice := []ScanHolder{{Owner: "alice", Workspace: "payroll"}}
	for _, n := range []int{memberWalkLinks, memberWalkLinks + 1} {
		links := map[string]string{fmt.Sprintf("chain/l%d", n): "members"}
		for i := 1; i < n; i++ {
			links[fmt.Sprintf("chain/l%d", i)] = fmt.Sprintf("chain/l%d", i+1)
		}
		base := scanFixture(t, map[string]string{"members/app": "MEMBER_TOKEN", "chain": "CHAIN_TOKEN"}, links)
		p := UserMountPolicy{Roots: []string{filepath.Join(base, "members")}}
		first := filepath.Join(base, "chain", "l1")
		_, everr := filepath.EvalSymlinks(first)
		src := filepath.Join(first, "app")
		if n == memberWalkLinks {
			if everr != nil {
				t.Fatalf("EvalSymlinks over %d links: %v; the walk's limit is no longer EvalSymlinks'", n, everr)
			}
			wantOpened(t, p, "", src, "MEMBER_TOKEN", alice)
			continue
		}
		if everr == nil {
			t.Fatalf("EvalSymlinks followed %d links; the walk's limit is no longer EvalSymlinks'", n)
		}
		dir, err := openWithin(t, p, src, alice)
		wantWalkGaveUp(t, dir, err, src)
	}
}
