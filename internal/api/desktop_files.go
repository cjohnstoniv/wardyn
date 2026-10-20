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
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/authz"
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
	// never sees half a file and a cut-off upload leaves nothing behind.
	desktopUploadScript = `set -e; d="${HOME:?}/Transfers"; mkdir -p "$d"; [ ! -d "$d/$1" ] || exit 5
t="$d/.wardyn-upload-$$"; cat > "$t"; if [ "$(wc -c < "$t")" -ne "$2" ]; then rm -f "$t"; exit 4; fi; mv -f "$t" "$d/$1"`
	desktopDownloadScript = `p="${HOME:?}/Transfers/$1"; [ -f "$p" ] && [ ! -L "$p" ] || exit 3; exec cat -- "$p"`
)

type desktopFile struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// desktopFilesView is GET /runs/{id}/desktop/files: the run's effective desktop rules and, only
// when download is allowed, what is in the transfer folder.
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

// desktopRun loads the run for a transfer: the person who may enter it, while it runs, with a
// desktop declared. It mirrors handleAttachTicket's entry rule (mayEnterRun) without the sponsor arm,
// and it is the desktop door: a background-only run has no desktop to move files to.
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
	if !superAdmin && s.refuseUIAppsDenied(w, r, run) {
		return run, types.DesktopControls{}, false
	}
	if s.refuseBackgroundOnly(w, r, run, "desktop") {
		return run, types.DesktopControls{}, false
	}
	switch {
	case run.State != types.RunRunning || run.SandboxRef == "":
		writeErrorReason(w, http.StatusConflict, reasonAttachNotRunning, "run is not RUNNING; no files can move (state="+string(run.State)+")")
		return run, types.DesktopControls{}, false
	case runIsKept(run):
		writeErrorReason(w, http.StatusConflict, reasonAttachRunKept, "run has ended; no files can move")
		return run, types.DesktopControls{}, false
	case s.cfg.Runner == nil:
		writeErrorReason(w, http.StatusServiceUnavailable, reasonUIGatewayNoRunner, "no runner configured; the desktop's files are unavailable")
		return run, types.DesktopControls{}, false
	}
	apps, rules, err := s.runDesktopPolicy(r.Context(), run.ID)
	if err != nil {
		slog.WarnContext(r.Context(), "wardynd: desktop rules unreadable; the transfer is refused", "run_id", run.ID, "err", err)
		writeErrorReason(w, http.StatusServiceUnavailable, reasonDesktopRulesUnreadable, desktopRulesUnreadableMsg)
		return run, types.DesktopControls{}, false
	}
	if len(apps) == 0 {
		writeErrorReason(w, http.StatusConflict, reasonDesktopTransferUnavailable, "this run's policy declares no UI app, so it has no desktop to move files to")
		return run, types.DesktopControls{}, false
	}
	at, principal := actorFromRequest(r)
	if err := s.thawForExec(r.Context(), run, at, principal, "presence"); err != nil {
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopTransferFailed, "run is paused and could not be resumed; try again")
		return run, types.DesktopControls{}, false
	}
	return run, rules, true
}

// desktopExec runs one in-sandbox half with stdin as its input, streaming its stdout to out.
func (s *Server) desktopExec(ctx context.Context, run types.AgentRun, script string, stdin io.Reader, out func(io.Reader) error, args ...string) (int, error) {
	sess, err := s.cfg.Runner.ExecStream(ctx, run.SandboxRef, runner.ExecSpec{Argv: append([]string{"sh", "-c", script, "sh"}, args...)})
	if err != nil {
		return -1, err
	}
	defer func() { _ = sess.Close() }()
	go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	copyErr := make(chan error, 1)
	go func() {
		var err error
		if stdin != nil {
			_, err = io.Copy(sess.Stdin, stdin)
		}
		_ = sess.Stdin.Close()
		copyErr <- err
	}()
	var outErr error
	if out != nil {
		outErr = out(sess.Stdout)
	}
	_, _ = io.Copy(io.Discard, sess.Stdout)
	code, err := sess.Wait()
	if err == nil {
		err = errors.Join(<-copyErr, outErr)
	}
	return code, err
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
	if rules.Download == types.TransferAllowed {
		var listing bytes.Buffer
		if code, err := s.desktopExec(r.Context(), run, desktopListScript, nil, func(o io.Reader) error {
			_, err := io.Copy(&listing, io.LimitReader(o, 64<<10))
			return err
		}, strconv.Itoa(maxDesktopListed)); err != nil || code != 0 {
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
		size, name, ok := strings.Cut(strings.TrimSpace(line), "/")
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
	name := chi.URLParam(r, "name")
	switch {
	case !validDesktopFileName(name):
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
	sum := sha256.New()
	body := io.TeeReader(io.LimitReader(r.Body, r.ContentLength), sum)
	code, err := s.desktopExec(r.Context(), run, desktopUploadScript, body, nil, name, strconv.FormatInt(r.ContentLength, 10))
	digest := hex.EncodeToString(sum.Sum(nil))
	if err != nil || code != 0 {
		s.auditDesktopTransfer(r, run, "upload", "failure", r.ContentLength, "")
		msg := fmt.Sprintf("the file did not arrive whole in %s (exit %d). Nothing was kept.", desktopTransferFolder, code)
		if code == 5 {
			msg = "a folder in " + desktopTransferFolder + " already has that name. Nothing was sent."
		}
		writeErrorReason(w, http.StatusBadGateway, reasonDesktopTransferFailed, msg)
		return
	}
	s.auditDesktopTransfer(r, run, "upload", "success", r.ContentLength, digest)
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "bytes": r.ContentLength, "sha256": digest})
}

// handleDownloadDesktopFile is GET /runs/{id}/desktop/files/{name}.
func (s *Server) handleDownloadDesktopFile(w http.ResponseWriter, r *http.Request) {
	run, rules, ok := s.desktopRun(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	switch {
	case !validDesktopFileName(name):
		writeErrorReason(w, http.StatusBadRequest, reasonDesktopTransferBadRequest, "a file name is a plain name: no folder, no leading dot")
		return
	case rules.Download != types.TransferAllowed:
		s.auditDesktopTransfer(r, run, "download", "denied", -1, "")
		writeErrorReason(w, http.StatusForbidden, reasonDesktopTransferBlocked, "this run's desktop rules block downloading files. Nothing was sent.")
		return
	}
	var sent int64
	var sum hash.Hash
	started := false
	code, err := s.desktopExec(r.Context(), run, desktopDownloadScript, nil, func(o io.Reader) error {
		br := bufio.NewReader(o)
		if _, err := br.Peek(1); err != nil {
			return nil // an empty file, or the script refused it: the exit code says which
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
		}
		return
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
