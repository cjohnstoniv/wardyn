// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// EditActionGuest is a window boot's guest that carries the edit window's buttons to the guest agent, over
// the tools session only (windowGuest): each opens something in the person's session in the VM and takes
// no input. A refusal with nothing done (nobody signed in, nothing to trust) wraps guestagent.ErrEditRefused.
type EditActionGuest interface {
	EditAdminTerminal(ctx context.Context) (guestagent.EditActionResult, error)
	EditTrustCAs(ctx context.Context) (guestagent.EditActionResult, error)
	EditFullDiskAccess(ctx context.Context) (guestagent.EditActionResult, error)
}

// The stage-2 window boot's guest is the one the edit window's buttons reach.
var _ EditActionGuest = (*windowGuest)(nil)

// ShutDownGuest is a window boot's guest whose agent shuts the guest down itself when the edit window
// closes, with no "Are you sure you want to shut down" dialog for the person to answer.
type ShutDownGuest interface {
	EditShutDown(ctx context.Context) error
}

var _ ShutDownGuest = (*windowGuest)(nil)

// LiveWindow is a Window with a line of its strip that Go rewrites: the edit window's read of the guest and
// what its last button did.
type LiveWindow interface {
	Window
	ShowLive(text string)
}

// editLiveEvery is how often the edit window reads the certificates' trust and Full Disk Access again.
var editLiveEvery = 5 * time.Second

// editNoCerts is the strip's words when Trust the list's CAs has nothing to trust.
const editNoCerts = "The install list has no certificates."

// editButtons is the edit window's buttons and live line, for the window boot's guest g of the copy in
// staging. Its methods run off the main thread; ctx ends with the window.
type editButtons struct {
	ctx     context.Context
	g       ToolsGuest
	staging string
	o       *EditOptions

	mu         sync.Mutex
	w          LiveWindow // nil until the window is up, and for a window with no live line
	read, note string
	watching   sync.WaitGroup
}

// wire gives text the buttons' callbacks and whether Trust the list's CAs has anything to trust: a ca item
// in staging's tools.json. A guest without the edit actions gets no callbacks, so its buttons do nothing.
func (b *editButtons) wire(text *WindowText) {
	rec, _ := ReadToolsRecord(b.staging)
	text.CanTrust = hasCA(rec.Items)
	eg, ok := b.g.(EditActionGuest)
	if !ok {
		return
	}
	text.AdminTerminal = b.act("Open admin Terminal", eg.EditAdminTerminal, func(guestagent.EditActionResult) string {
		return "Terminal is open in the VM at su - wardyn-admin. Type the one-time code there (Type code types it), then press Return."
	})
	text.TrustCAs = b.act("Trust the list's CAs", eg.EditTrustCAs, func(r guestagent.EditActionResult) string {
		n := fmt.Sprintf("%d certificates", r.Certificates)
		if r.Certificates == 1 {
			n = "1 certificate"
		}
		return "Terminal is open in the VM with " + n + " to trust. Check each one, and type the one-time code each time it asks (Type code types it). " +
			"In macOS's dialog, enter wardyn-admin and the code."
	})
	text.FullDiskAccess = b.act("Full Disk Access", eg.EditFullDiskAccess, func(r guestagent.EditActionResult) string {
		if r.Revealed {
			return "System Settings is open at Full Disk Access in the VM, and Finder shows the guest agent: drag it into the list and turn it on."
		}
		return "System Settings is open at Full Disk Access in the VM. Add " + guestagent.MacAgentBinaryPath + " with its + button, and turn it on."
	})
	b.wireRosetta(text)
}

// act is a button's callback: it asks the guest for the action and shows what came of it: the guest's own
// words when it refused or failed, and "did not reach" only when no answer came back.
func (b *editButtons) act(name string, call func(context.Context) (guestagent.EditActionResult, error), done func(guestagent.EditActionResult) string) func() {
	words := func(err, kind error) string {
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(err.Error(), kind.Error()), ":"))
	}
	return func() {
		res, err := call(b.ctx)
		switch {
		case errors.Is(err, guestagent.ErrEditRefused):
			b.show("", name+" did nothing: "+words(err, guestagent.ErrEditRefused)+".")
		case errors.Is(err, guestagent.ErrEditFailed):
			b.o.Log.Warn("macvm: an edit window button failed in the guest", "button", name, "err", err.Error())
			b.show("", name+" failed in the VM: "+words(err, guestagent.ErrEditFailed)+".")
		case err != nil:
			b.o.Log.Warn("macvm: an edit window button did not reach the guest agent", "button", name, "err", err.Error())
			b.show("", name+" did not reach the guest agent. Try again, or follow the steps above by hand.")
		default:
			b.show("", done(res))
		}
	}
}

// wait returns once the live line's reads have stopped (after ctx ends).
func (b *editButtons) wait() { b.watching.Wait() }

// show sets the live line's read (when not empty) or note, and redraws it.
func (b *editButtons) show(read, note string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if read != "" {
		b.read = read
	}
	if note != "" {
		b.note = note
	}
	if b.w == nil {
		return
	}
	text := b.read
	if b.note != "" {
		text = strings.TrimPrefix(text+"\n"+b.note, "\n")
	}
	b.w.ShowLive(text)
}

// watch draws the live line in w, reading the guest now and every editLiveEvery until ctx ends; wait
// returns once it has stopped.
func (b *editButtons) watch(w LiveWindow) {
	b.mu.Lock()
	b.w = w
	b.mu.Unlock()
	every := editLiveEvery
	b.watching.Add(1)
	go func() {
		defer b.watching.Done()
		for {
			rctx, cancel := context.WithTimeout(b.ctx, 30*time.Second)
			read := b.readGuest(rctx)
			cancel()
			if b.ctx.Err() != nil {
				return
			}
			b.show(read, "")
			select {
			case <-b.ctx.Done():
				return
			case <-time.After(every):
			}
		}
	}()
}

// readGuest is the live line's read: each ca item of staging's tools.json trusted or not, this Mac's agent
// signing certificate when the agent is signed with it, and Full Disk Access allowed or refused, as the
// guest reads them now. A read that fails is "could not be read", never trusted or allowed.
func (b *editButtons) readGuest(ctx context.Context) string {
	rec, _ := ReadToolsRecord(b.staging)
	words := func(state string) string {
		return map[string]string{ItemTrusted: "trusted", ItemNotTrusted: "not trusted", "allowed": "allowed", "refused": "refused"}[state]
	}
	unread := "could not be read"
	var parts []string
	verify := func(policy, cert string) string {
		_, exit, err := b.g.Run(ctx, []string{"/usr/bin/security", "verify-cert", "-L", "-p", policy, "-c", cert})
		if err != nil {
			return unread
		}
		return words(map[bool]string{true: ItemTrusted, false: ItemNotTrusted}[exit == 0])
	}
	for _, r := range rec.Items {
		if r.Kind == ItemKindCA {
			parts = append(parts, "certificate "+r.Name+" "+verify("ssl", guestagent.ItemDir(r.Name)+"/"+guestagent.ItemCert))
		}
	}
	fda, fileVault := unread, ""
	if p, ok := b.g.(interface {
		Posture(ctx context.Context) (guestagent.ToolsPosture, error)
	}); ok {
		if posture, err := p.Posture(ctx); err == nil {
			fileVault = posture.FileVault
			if words(posture.FullDiskAccess) != "" {
				fda = words(posture.FullDiskAccess)
			}
		}
	}
	parts = append(parts, "Full Disk Access "+fda)
	line := "Read by the guest now: " + strings.Join(parts, "; ") + "."
	if warn := FileVaultWarning(fileVault); warn != "" {
		line += " " + warn
	}
	if !hasCA(rec.Items) {
		line = editNoCerts + " " + line
	}
	return line
}

func hasCA(items []ItemRecord) bool {
	for _, r := range items {
		if r.Kind == ItemKindCA {
			return true
		}
	}
	return false
}
