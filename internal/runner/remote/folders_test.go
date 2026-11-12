// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// streamGoneTransport is a runner whose pass ended before the server took its stream: sync_folder answers a
// stream id that is already gone (TakeStream: ErrNoStream), and sync_folder_wait answers the pass's account,
// or waitErr.
type streamGoneTransport struct {
	account runnerwire.FolderPass
	waitErr error
	waited  runnerwire.SyncFolderWaitArgs
}

func (*streamGoneTransport) Online() bool       { return true }
func (*streamGoneTransport) SetHandler(Handler) {}
func (*streamGoneTransport) TakeStream(uint32) (*runnerwire.Stream, error) {
	return nil, runnerwire.ErrNoStream
}
func (t *streamGoneTransport) Call(_ context.Context, method string, args, result any, _ ...runnerwire.CallOption) error {
	switch method {
	case runnerwire.MethodSyncFolder:
		result.(*runnerwire.StreamResult).Stream = 7
	case runnerwire.MethodSyncFolderWait:
		t.waited = args.(runnerwire.SyncFolderWaitArgs)
		if t.waitErr != nil {
			return t.waitErr
		}
		*result.(*runnerwire.FolderPass) = t.account
	}
	return nil
}

// A pass that ended before the server took its stream reports its own reason, not "no such stream": the
// runner is asked for the pass's account of that stream, and it comes back in a PassEnded.
func TestSyncFolder_StreamGoneReportsThePassReason(t *testing.T) {
	tr := &streamGoneTransport{account: runnerwire.FolderPass{Error: "the pass ran out of time", Unsynced: 2}}
	_, err := New("r1", tr, Options{}).SyncFolder(context.Background(), runnerwire.SyncFolderArgs{Pull: true})
	var ended *PassEnded
	if !errors.As(err, &ended) {
		t.Fatalf("SyncFolder = %v, want a PassEnded", err)
	}
	if ended.Pass.Error != tr.account.Error || ended.Pass.Unsynced != tr.account.Unsynced {
		t.Errorf("the pass = %+v, want the runner's account %+v", ended.Pass, tr.account)
	}
	if tr.waited.Stream != 7 {
		t.Errorf("asked for the outcome of stream %d, want 7", tr.waited.Stream)
	}
	if errors.Is(err, runnerwire.ErrNoStream) || strings.Contains(err.Error(), "no such stream") {
		t.Errorf("err = %q, want the pass's reason, not the missing stream", err)
	}
}

// When the runner cannot give the account either, the error keeps both: the missing stream, and why the outcome
// could not be asked for.
func TestSyncFolder_StreamGoneAndNoAccountNamesBoth(t *testing.T) {
	tr := &streamGoneTransport{waitErr: errors.New("the link dropped")}
	_, err := New("r1", tr, Options{}).SyncFolder(context.Background(), runnerwire.SyncFolderArgs{})
	var ended *PassEnded
	if errors.As(err, &ended) || !errors.Is(err, runnerwire.ErrNoStream) ||
		!strings.Contains(err.Error(), "asking for the pass's outcome failed: the link dropped") {
		t.Errorf("SyncFolder = %v, want no such stream and the failed ask", err)
	}
}
