// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run's Files panel (#2967): files move between the console and one folder in the desktop,
// ~/Transfers, over the same exec lane the terminal uses, under the run's desktop rules. A transfer
// that succeeds, fails in the sandbox, or is denied by the desktop rules or the upload limit is
// audited by direction, size and hash, never its name or content (docs/AUDIT-ACTIONS.md). A request
// refused before that (a name that is not plain, no such file, a run it may not enter) writes no row.
const (
	desktopTransferFolder = "~/Transfers"
	// maxDesktopUploadBytes bounds one upload. The body streams into the sandbox; the bound is a
	// stated limit on what a console upload is for, not a buffer size.
	maxDesktopUploadBytes = 256 << 20
	maxDesktopListed      = 200
)

// The in-sandbox halves. Each takes the file name as $1, never spliced into the script.
const (
	desktopListScript = `d="${HOME:?}/Transfers"; [ -d "$d" ] || exit 0; n=0
for f in "$d"/*; do [ -f "$f" ] && [ ! -L "$f" ] || continue
n=$((n+1)); [ "$n" -le "$1" ] || break; printf '%s/%s\n' "$(wc -c < "$f")" "${f##*/}"; done`
	// The upload lands under a dot name and is renamed only when every byte arrived, so the agent
	// never sees half a file. Every failure removes the dot file, a failed write or rename included,
	// and exits 4 only once it is gone: any other failure cannot say what was left in the folder.
	desktopUploadScript = `d="${HOME:?}/Transfers"; mkdir -p "$d" || exit 4; [ ! -d "$d/$1" ] || exit 5
t="$d/.wardyn-upload-$$"; trap 'rm -f "$t"' EXIT; trap 'exit 1' HUP INT TERM
cat > "$t" && [ "$(wc -c < "$t")" -eq "$2" ] && mv -f "$t" "$d/$1" && exit 0
rm -f "$t" && [ ! -e "$t" ] && exit 4; exit 1`
	desktopDownloadScript = `p="${HOME:?}/Transfers/$1"; [ -f "$p" ] && [ ! -L "$p" ] || exit 3; exec cat -- "$p"`
)

type desktopFile struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// desktopFilesView is GET /runs/{id}/desktop/files: the run's effective desktop rules and, only
// when download is allowed or held for review, what is in the transfer folder.
type desktopFilesView struct {
	Folder string                `json:"folder"`
	Rules  types.DesktopControls `json:"rules"`
	Files  []desktopFile         `json:"files"`
}

// validDesktopFileName is a plain name in the transfer folder: no path, no dot file (the upload's
// own temporary name is one), nothing a list line could not carry.
func validDesktopFileName(name string) bool {
	return name != "" && len(name) <= 255 && !strings.HasPrefix(name, ".") && !strings.ContainsAny(name, "/\\\x00\n\r\t")
}

// desktopFileNameParam is the {name} path segment, unescaped once when the client
// percent-encoded it. chi routes on RawPath when Go kept one (a name with one of
// ( ) ! * ' next to a space or a non-ASCII letter), and then hands the segment
// back still encoded; see principalParam. ok is false for a bad escape. A decoded
// '/' still fails validDesktopFileName.
func desktopFileNameParam(r *http.Request) (string, bool) {
	name := chi.URLParam(r, "name")
	if r.URL.RawPath == "" {
		return name, true
	}
	u, err := url.PathUnescape(name)
	return u, err == nil
}

// desktopRun loads the run for a transfer: the person who may enter it, while it runs, with a
// desktop declared or a Linux VM's built-in one. It mirrors handleAttachTicket's entry rule
// (mayEnterRun) without the sponsor arm, and it is the desktop door: a background-only run has no
// desktop to move files to, and deny_ui_apps and deny_interactive close it as they close the desktop
// itself (uiEnterDesktop). It never thaws a paused run: only a transfer the rules allow does, just
// before its exec.
func (s *Server) desktopRun(w http.ResponseWriter, r *http.Request) (types.AgentRun, types.DesktopControls, bool) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return types.AgentRun{}, types.DesktopControls{}, false
	}
	run, ok := s.getRunAuthorizedBy(w, r, id, s.ownsRunOrAdmin)
	if !ok {
		return run, types.DesktopControls{}, false
	}
	superAdmin := s.isOperator(r.Context())
	if !mayEnterRun(run, principalFromRequest(r), superAdmin) {
		if superAdmin {
			s.refuseRunOwnerOnly(w, r, run)
		} else {
			s.refuse(w, r, authz.Deny(authz.ReasonAttachTicketForeignRun, run.ID.String(), "run not found").OnRun(run.ID).AsIf(authz.Reason(reasonRunNotFound)))
		}
		return run, types.DesktopControls{}, false
	}
	if (!superAdmin || !s.adminDoorExempt(run)) && (s.refuseUIAppsDenied(w, r, run) || s.refuseInteractiveAttach(w, r, run)) {
		return run, types.DesktopControls{}, false
	}
	if s.refuseBackgroundOnly(w, r, run, "desktop") {
		return run, types.DesktopControls{}, false
	}
	if refuseDesktopNotLive(w, run) {
		return run, types.DesktopControls{}, false
	}
	if s.cfg.Runner == nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonUIGatewayNoRunner, "no runner configured; the desktop's files are unavailable")
		return run, types.DesktopControls{}, false
	}
	apps, rules, dataOut, err := s.runDesktopPolicy(r.Context(), run.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "wardynd: desktop rules unreadable; the transfer is refused", "run_id", run.ID, "err", err)
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopRulesUnreadable, desktopRulesUnreadableMsg)
		return run, types.DesktopControls{}, false
	}
	// A Linux VM run's built-in desktop is a desktop with no declared app: its files move under the same rules.
	if len(apps) == 0 && run.MachineKind != types.MachineLinuxVM {
		if dataOut != types.TransferReview {
			writeErrorReason(w, http.StatusConflict, reasonDesktopTransferUnavailable, "this run's policy declares no UI app, so it has no desktop to move files to")
			return run, types.DesktopControls{}, false
		}
		// data_out review: ~/Transfers is open for reviewed downloads alone, on a run with no desktop.
		rules = types.DesktopControls{PasteIn: types.TransferBlocked, CopyOut: types.TransferBlocked, Upload: types.TransferBlocked, Download: types.TransferReview}
	}
	return run, rules, true
}

// refuseDesktopNotLive answers a run with no live sandbox to move files to: not RUNNING, no sandbox, or kept.
// True when it has answered.
func refuseDesktopNotLive(w http.ResponseWriter, run types.AgentRun) bool {
	switch {
	case run.State != types.RunRunning || run.SandboxRef == "":
		writeErrorReason(w, http.StatusConflict, reasonAttachNotRunning, "run is not RUNNING; no files can move (state="+string(run.State)+")")
	case runIsKept(run):
		writeErrorReason(w, http.StatusConflict, reasonAttachRunKept, "run has ended; no files can move")
	default:
		return false
	}
	return true
}

// desktopAdmit admits a desktop exec into run, as desktopRun read it. A transfer (thaw) thaws it first: thawForExec
// reads it again under the run's operation lock, so a run that ended since is not thawed. The run is then read once
// more under its start lock (lockStart), which every write that ends the run or marks it kept takes around that
// write, so nothing is sent into a run ended before this read, and an end after it waits until desktopExec's start
// has returned, at most execStartBudget. A list, which never thaws, refuses it paused (runPausedReadMsg). The hold is
// never kept for the transfer. ok=false: the refusal is written.
func (s *Server) desktopAdmit(w http.ResponseWriter, r *http.Request, run types.AgentRun, thaw bool) (fresh types.AgentRun, h startHold, ok bool) {
	if thaw {
		at, principal := actorFromRequest(r)
		switch err := s.thawForExec(r.Context(), run, at, principal, "presence"); {
		case db.LockRefused(err):
			writeLockRefused(w, r, err)
			return types.AgentRun{}, startHold{}, false
		case err != nil:
			writeErrorReason(w, http.StatusBadGateway, reasonDesktopTransferFailed, "run is paused and could not be resumed; try again")
			return types.AgentRun{}, startHold{}, false
		}
	}
	h, err := s.lockStart(r.Context(), run.ID)
	if err != nil {
		writeLockRefused(w, r, err)
		return types.AgentRun{}, startHold{}, false
	}
	fresh, err = s.cfg.Store.GetRun(h.ctx, run.ID)
	switch {
	case err != nil:
		writeServerError(w, r, "get run", err)
	case refuseDesktopNotLive(w, fresh):
	case !thaw && fresh.PausedAt != nil:
		writeErrorReason(w, http.StatusConflict, reasonRunInspectPaused, runPausedReadMsg)
	default:
		return fresh, h, true
	}
	h.release()
	return types.AgentRun{}, startHold{}, false
}

// desktopExec runs one in-sandbox half with stdin as its input, streaming its stdout to out. execAdmitted lets
// desktopAdmit's start lock go beside the exec once ExecStream has returned, as for runExec. The exec is closed when
// it ends or when its caller leaves, whichever is first, so a caller gone mid-transfer leaves no exec open.
func (s *Server) desktopExec(ctx context.Context, run types.AgentRun, start startHold, script string, stdin io.Reader, out func(io.Reader) error, args ...string) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sess, err := s.execAdmitted(ctx, start, run.SandboxRef, runner.ExecSpec{Argv: append([]string{"sh", "-c", script, "sh"}, args...)})
	if err != nil {
		return -1, err
	}
	closeSess := sync.OnceFunc(func() { _ = sess.Close() })
	defer closeSess() // a panic in out still closes the session
	context.AfterFunc(ctx, closeSess)
	go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	copyErr := make(chan error, 1)
	go func() {
		var err error
		if stdin != nil {
			_, err = io.Copy(sess.Stdin, stdin)
		}
		// Reported before stdin closes, so an exec that ends at the end of its input always finds the copy
		// reported below, and a whole upload never has its read stopped.
		copyErr <- err
		_ = sess.Stdin.Close()
	}()
	var outErr error
	if out != nil {
		outErr = out(sess.Stdout)
	}
	_, _ = io.Copy(io.Discard, sess.Stdout)
	code, err := sess.Wait()
	// Close before the join, so a stdin copy stuck writing to a gone exec ends. One still waiting to read the
	// caller's body is ended by the caller's stopRead: nothing else interrupts that Read, so the join would wait
	// for as long as the client sends nothing. The join keeps the copy from reading the body after this returns.
	closeSess()
	var copied error
	select {
	case copied = <-copyErr:
	default:
		if st, ok := stdin.(interface{ stopRead() }); ok {
			st.stopRead()
		}
		copied = <-copyErr
	}
	if err != nil {
		return -1, err // no exit status came back: what the exec did is unknown
	}
	return code, errors.Join(copied, outErr)
}

func (s *Server) auditDesktopTransfer(r *http.Request, run types.AgentRun, direction, outcome string, size int64, sum string) {
	at, principal := actorFromRequest(r)
	data := map[string]any{"direction": direction}
	if size >= 0 {
		data["bytes"] = size
	}
	if sum != "" {
		data["sha256"] = sum
	}
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, at, principal, "desktop.file.transfer", desktopTransferFolder, outcome, mustJSON(data)))
}

// handleListDesktopFiles is GET /runs/{id}/desktop/files.
func (s *Server) handleListDesktopFiles(w http.ResponseWriter, r *http.Request) {
	run, rules, ok := s.desktopRun(w, r)
	if !ok {
		return
	}
	view := desktopFilesView{Folder: desktopTransferFolder, Rules: rules, Files: []desktopFile{}}
	if rules.Download == types.TransferAllowed || rules.Download == types.TransferReview { // a reviewed download lists what may be asked for
		// Reading the folder is an exec, which a paused run refuses, and a look is not a person, so it
		// never thaws (desktopAdmit refuses it with runPausedReadMsg). With download blocked no exec runs and
		// the rules still answer.
		fresh, start, ok := s.desktopAdmit(w, r, run, false)
		if !ok {
			return
		}
		defer start.release()
		var listing bytes.Buffer
		if code, err := s.desktopExec(r.Context(), fresh, start, desktopListScript, nil, func(o io.Reader) error {
			_, err := io.Copy(&listing, io.LimitReader(o, 64<<10))
			return err
		}, strconv.Itoa(maxDesktopListed)); err != nil || code != 0 {
			if s.refuseReviewUnavailable(w, r, run, err) {
				return
			}
			writeErrorReason(w, http.StatusBadGateway, reasonDesktopTransferFailed, fmt.Sprintf("could not list %s in the desktop (exit %d)", desktopTransferFolder, code))
			return
		}
		view.Files = parseDesktopListing(listing.String())
	}
	writeJSON(w, http.StatusOK, view)
}

func parseDesktopListing(out string) []desktopFile {
	files := []desktopFile{}
	for _, line := range strings.Split(out, "\n") {
		// Only the size is trimmed: a name may end in a space, and the download must ask for it as it is.
		size, name, ok := strings.Cut(line, "/")
		n, err := strconv.ParseInt(strings.TrimSpace(size), 10, 64)
		if ok && err == nil && validDesktopFileName(name) {
			files = append(files, desktopFile{Name: name, Bytes: n})
		}
	}
	return files
}

// handleUploadDesktopFile is PUT /runs/{id}/desktop/files/{name}: the body is the file.
func (s *Server) handleUploadDesktopFile(w http.ResponseWriter, r *http.Request) {
	run, rules, ok := s.desktopRun(w, r)
	if !ok {
		return
	}
	name, nameOK := desktopFileNameParam(r)
	switch {
	case !nameOK || !validDesktopFileName(name):
		writeErrorReason(w, http.StatusBadRequest, reasonDesktopTransferBadRequest, "a file name is a plain name: no folder, no leading dot")
		return
	case r.ContentLength < 0:
		writeErrorReason(w, http.StatusBadRequest, reasonDesktopTransferBadRequest, "an upload needs a Content-Length")
		return
	case rules.Upload != types.TransferAllowed:
		s.auditDesktopTransfer(r, run, "upload", "denied", r.ContentLength, "")
		writeErrorReason(w, http.StatusForbidden, reasonDesktopTransferBlocked, "this run's desktop rules block uploading files. Nothing was sent.")
		return
	case r.ContentLength > maxDesktopUploadBytes:
		s.auditDesktopTransfer(r, run, "upload", "denied", r.ContentLength, "")
		writeErrorReason(w, http.StatusRequestEntityTooLarge, reasonDesktopTransferTooLarge,
			fmt.Sprintf("the file is over the %d MiB limit for a desktop upload. Nothing was sent.", maxDesktopUploadBytes>>20))
		return
	}
	run, start, ok := s.desktopAdmit(w, r, run, true)
	if !ok {
		return
	}
	defer start.release()
	sum := sha256.New()
	body := uploadBody{io.TeeReader(io.LimitReader(r.Body, r.ContentLength), sum), w, r.ProtoMajor == 1}
	code, err := s.desktopExec(r.Context(), run, start, desktopUploadScript, body, nil, name, strconv.FormatInt(r.ContentLength, 10))
	// A stopped read (uploadBody.stopRead) or a browser that left cancels the request's context on HTTP/1.x; the
	// outcome row keeps the request's values and is written anyway.
	r = r.WithContext(context.WithoutCancel(r.Context()))
	digest := hex.EncodeToString(sum.Sum(nil))
	if err != nil || code != 0 {
		s.auditDesktopTransfer(r, run, "upload", "failure", r.ContentLength, "")
		// Only the script's own exit 4 says the partial file is gone; a lost exit status proves nothing.
		msg := "the upload to " + desktopTransferFolder + " failed, and some bytes may remain there."
		switch code {
		case 4:
			msg = fmt.Sprintf("the file did not arrive whole in %s (exit %d). Nothing was kept.", desktopTransferFolder, code)
		case 5:
			msg = "a folder in " + desktopTransferFolder + " already has that name. Nothing was sent."
		}
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopTransferFailed, msg)
		return
	}
	s.auditDesktopTransfer(r, run, "upload", "success", r.ContentLength, digest)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "bytes": r.ContentLength, "sha256": digest})
}

// uploadBody is an upload's request body as the exec's stdin. stopRead ends a Read that is waiting on a client
// that sends nothing: the server sets no read timeout, and closing the body does not interrupt a Read in progress.
type uploadBody struct {
	io.Reader
	w     http.ResponseWriter
	http1 bool
}

// stopRead sets a read deadline in the past. On HTTP/1.x, a read it ends cancels the connection's context, and
// once the body has been read whole the connection would carry its next request already cancelled, so the reply
// closes the connection.
func (b uploadBody) stopRead() {
	if b.http1 {
		b.w.Header().Set("Connection", "close")
	}
	_ = http.NewResponseController(b.w).SetReadDeadline(time.Now())
}

// handleDownloadDesktopFile is GET /runs/{id}/desktop/files/{name}.
func (s *Server) handleDownloadDesktopFile(w http.ResponseWriter, r *http.Request) {
	run, rules, ok := s.desktopRun(w, r)
	if !ok {
		return
	}
	name, nameOK := desktopFileNameParam(r)
	switch {
	case !nameOK || !validDesktopFileName(name):
		writeErrorReason(w, http.StatusBadRequest, reasonDesktopTransferBadRequest, "a file name is a plain name: no folder, no leading dot")
		return
	case rules.Download == types.TransferReview:
		s.serveReviewedDownload(w, r, run, name) // desktop_review.go
		return
	case rules.Download != types.TransferAllowed:
		s.auditDesktopTransfer(r, run, "download", "denied", -1, "")
		writeErrorReason(w, http.StatusForbidden, reasonDesktopTransferBlocked, "this run's desktop rules block downloading files. Nothing was sent.")
		return
	}
	run, start, ok := s.desktopAdmit(w, r, run, true)
	if !ok {
		return
	}
	defer start.release()
	var sent int64
	var sum hash.Hash
	started := false
	code, err := s.desktopExec(r.Context(), run, start, desktopDownloadScript, nil, func(o io.Reader) error {
		br := bufio.NewReader(o)
		if _, err := br.Peek(1); err != nil {
			if errors.Is(err, io.EOF) {
				return nil // an empty file, or the script refused it: the exit code says which
			}
			return err // the stream failed before its first byte: that is not an empty file
		}
		started, sum = true, sha256.New()
		desktopDownloadHeaders(w, name)
		n, err := io.Copy(io.MultiWriter(w, sum), br)
		sent = n
		return err
	}, name)
	switch {
	case !started && code == 3:
		writeErrorReason(w, http.StatusNotFound, reasonDesktopTransferNotFound, "no file of that name in "+desktopTransferFolder)
		return
	case err != nil || code != 0:
		s.auditDesktopTransfer(r, run, "download", "failure", sent, "")
		if !started {
			writeErrorReason(w, http.StatusBadGateway, reasonDesktopTransferFailed, fmt.Sprintf("could not read the file in the desktop (exit %d)", code))
			return
		}
		// The 200 is committed: send what was read, then abort, so the client sees a broken transfer and never a
		// short file that ended cleanly. Unflushed, a body under the response buffer would end as no response.
		_ = http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	case !started: // an empty file
		sum = sha256.New()
		desktopDownloadHeaders(w, name)
	}
	s.auditDesktopTransfer(r, run, "download", "success", sent, hex.EncodeToString(sum.Sum(nil)))
}

func desktopDownloadHeaders(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.WriteHeader(http.StatusOK)
}
