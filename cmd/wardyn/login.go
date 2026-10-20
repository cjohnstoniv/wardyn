// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// noSignInLine is what login says about signing in while no control plane offers a browser sign-in.
const noSignInLine = "This control plane does not offer CLI sign-in: set WARDYN_TOKEN to a personal API token. " +
	"A Wardyn admin can invite you to create one, and you claim it under Your account in the console."

// loopbackWait is how long a browser sign-in waits for the browser to come back (design ATTACH 11.2).
var loopbackWait = 5 * time.Minute

// loginCmd is `wardyn login [<url>]`: it saves where this computer's CLI points and, when the organisation
// offers CLI sign-in (/healthz.cli_sign_in), signs this computer in through the browser. resolved is the URL
// the root flags settled on and whether it is only the built-in default.
func loginCmd(resolved func() (string, bool)) *cobra.Command {
	var admin, noBrowser bool
	cmd := &cobra.Command{
		Use:   "login [<url>]",
		Short: "Sign this computer's CLI in to your organisation",
		Long: `Save your organisation's address in this computer's CLI profile, then sign in through the browser when the
organisation offers CLI sign-in. The sign-in is kept in the keychain (on Linux with no keychain, in a file only you
can read) and renews itself; --token, WARDYN_ADMIN_TOKEN and WARDYN_TOKEN still take precedence over it.

--admin asks for admin access for 1 hour, approved again in the browser. --no-browser prints the address to open on
any computer and asks for the code the page shows, where the organisation allows it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, fromDefault := resolved()
			if len(args) == 1 {
				raw, fromDefault = args[0], false
			}
			if fromDefault {
				return errors.New(errNoOrg)
			}
			u, err := cliprofile.Normalize(raw)
			if err != nil {
				return err
			}
			// Advisory, like every command's token warning: a TLS-terminating proxy is legitimate.
			if host := plaintextHost(u); host != "" && !cliutil.EnvBool("WARDYN_CLI_ALLOW_PLAINTEXT", false) {
				fmt.Fprintf(cmd.ErrOrStderr(), "wardyn: WARNING: %s is plain http://, so a token sent to it travels in cleartext — "+
					"use https://, or set WARDYN_CLI_ALLOW_PLAINTEXT=1 if TLS terminates in front of it\n", host)
			}
			dir, err := cliprofile.Dir()
			if err != nil {
				return err
			}
			if err := cliprofile.Write(dir, u); err != nil {
				return err
			}
			var h struct {
				types.CLISignInHealthz
				Version string `json:"version"`
			}
			if err := cliCall(cmd.Context(), u, http.MethodGet, "/healthz", "", nil, &h); err != nil {
				return fmt.Errorf(errUnreachable, u, err)
			}
			l := &login{origin: u, dir: dir, out: cmd.OutOrStdout(), errOut: cmd.ErrOrStderr(), in: cmd.InOrStdin(),
				noBrowser: noBrowser, paste: h.CLIPasteSignIn}
			if err := otherOrigin(h.CLISignInHealthz, u); err != nil {
				return err
			}
			switch {
			case !h.CLISignIn:
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Saved %s. %s\nThe profile is %s; --url and WARDYN_URL still take precedence.\n",
					u, noSignInLine, filepath.Join(dir, cliprofile.FileName))
			case admin:
				err = l.stepUp(cmd.Context())
			default:
				err = l.signIn(cmd.Context(), nil)
			}
			if err == nil {
				versionNotice(cmd.Context(), cmd.ErrOrStderr(), u, h.Version)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&admin, "admin", false, "ask for admin access for 1 hour (approved again in the browser)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "sign in from a computer with no browser: open the address elsewhere and paste the code back")
	return cmd
}

// otherOrigin is the refusal when h offers a sign-in at an origin other than u, the URL dialled, else nil.
// Proofs are signed over u, never over the one the server names: it is only compared, so a look-alike
// cannot obtain a proof for the origin it advertises.
func otherOrigin(h types.CLISignInHealthz, u string) error {
	if h.CLISignIn && h.CLISignInOrigin != "" && h.CLISignInOrigin != u {
		return fmt.Errorf(errOtherOrigin, h.CLISignInOrigin, u, h.CLISignInOrigin)
	}
	return nil
}

// login is one `wardyn login` against origin.
type login struct {
	origin, dir      string
	out, errOut      io.Writer
	in               io.Reader
	noBrowser, paste bool // --no-browser; /healthz.cli_paste_sign_in
}

// stepUp is `login --admin`: a cli_admin token for this computer's live CLI session, signing in first when
// there is none.
func (l *login) stepUp(ctx context.Context) error {
	s, err := loadSession(ctx, l.origin)
	if err != nil || s.kind == "" {
		fmt.Fprintln(l.errOut, copyAdminFirst)
		if err := l.signIn(ctx, nil); err != nil {
			return err
		}
		if s, err = loadSession(ctx, l.origin); err != nil {
			return err
		}
	}
	return l.signIn(ctx, &s)
}

// signIn runs one sign-in request to its grant and stores it. base is nil for a new CLI session, or the
// live session an admin step-up belongs to: the step-up reuses its device key and store.
func (l *login) signIn(ctx context.Context, base *storedSession) error {
	var (
		key     ed25519.PrivateKey
		kind    string
		machine string
		err     error
	)
	if base != nil {
		if key, err = base.item.key(); err != nil {
			return err
		}
		kind, machine = base.kind, base.item.Machine
	} else {
		if _, key, err = ed25519.GenerateKey(rand.Reader); err != nil {
			return err
		}
		kind, machine = chooseStore(l.origin), machineName()
	}
	mode := types.CLISignInModeLoopback
	switch {
	case l.noBrowser && !l.paste:
		return errors.New(errNoPasteMode)
	case l.noBrowser:
		mode = types.CLISignInModePaste
	case !canOpenBrowser() && l.paste:
		return errors.New(errNoBrowserPaste)
	case !canOpenBrowser():
		return errors.New(errNoBrowserNoPaste)
	}
	if kind == types.CLIStoreFile && base == nil {
		fmt.Fprintf(l.errOut, copyFileStore+"\n", fileStore{l.dir}.path(), inWords(types.CLIFileStoreSessionCap))
	}
	verifier := randomB64(32)
	sum := sha256.Sum256([]byte(verifier))
	req := types.CLISignInCreateRequest{Challenge: base64.RawURLEncoding.EncodeToString(sum[:]), Machine: machine,
		PublicKey: base64.RawURLEncoding.EncodeToString(key.Public().(ed25519.PublicKey)), Mode: mode, Store: kind}
	if base != nil {
		id := base.item.SessionID
		req.Admin, req.SessionID = true, &id
	}
	var ln net.Listener
	if mode == types.CLISignInModeLoopback {
		// 127.0.0.1 only: the approve page's redirect names it, and nothing off this computer can reach it.
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
		defer ln.Close()
		req.Port = ln.Addr().(*net.TCPAddr).Port
	}
	var created types.CLISignInCreateResponse
	if err := cliCall(ctx, l.origin, http.MethodPost, "/api/v1/cli/sign-ins", "", req, &created); err != nil {
		return err
	}
	approve := l.origin + "/cli/sign-in?id=" + created.ID.String()
	var (
		code   string
		finish = func(bool) {}
	)
	if mode == types.CLISignInModeLoopback {
		fmt.Fprintf(l.out, copyOpening+"\n", approve)
		fmt.Fprintf(l.out, copyCheckCode+"\n", created.UserCode)
		_ = openBrowser(approve)
		if code, finish, err = awaitCallback(ln, created.ID, l.paste); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(l.out, copyPasteOpen+"\n", approve)
		fmt.Fprintf(l.out, copyCheckCode+"\n", created.UserCode)
		if code, err = readPasted(l.in, l.errOut); err != nil {
			return err
		}
	}
	route := "/api/v1/cli/sign-ins/" + created.ID.String() + "/token"
	var g types.CLISessionGrant
	err = cliCall(ctx, l.origin, http.MethodPost, route, "", types.CLISignInTokenRequest{Code: code, Verifier: verifier,
		Proof: cliProof(key, l.origin, route, created.ID, code, time.Now())}, &g)
	if err != nil {
		finish(false)
		return err
	}
	it := cliItem{KeySeed: base64.RawURLEncoding.EncodeToString(key.Seed()), Machine: machine}
	if base != nil {
		it = base.item
	}
	it.applyGrant(g)
	prev, replacing, err := l.store(kind, it, base != nil)
	// The browser hears "Signed in" only once the sign-in is stored.
	finish(err == nil)
	if err != nil {
		return err
	}
	// The previous session ends only once the new one is stored: ended first, a failed sign-in would
	// leave this computer with none.
	if replacing && prev.SessionID != g.SessionID {
		l.endPrevious(ctx, prev, g.AccessToken)
	}
	who := whoIs(ctx, l.origin, g.AccessToken)
	host := l.origin
	if pu, err := url.Parse(l.origin); err == nil {
		host = pu.Host
	}
	if base != nil {
		_, err = fmt.Fprintf(l.out, copyAdminSignedIn+"\n", host, who, g.AccessExpiresAt.Local().Format("15:04"))
	} else {
		_, err = fmt.Fprintf(l.out, copySignedIn+"\n", host, who, machine)
	}
	return err
}

// store saves it under cli.lock, as one read-modify-write against the item stored now. A new session
// replaces the item (and one another store held) and points the profile at kind; it returns the live item
// it replaced. A step-up keeps the stored item, which a refresh may have rotated while the browser was
// open, and adds only the cli_admin token to it.
func (l *login) store(kind string, it cliItem, stepUp bool) (prev cliItem, replacing bool, err error) {
	unlock, ok, err := lockRefresh(l.dir)
	if err != nil {
		return prev, false, err
	}
	if !ok {
		return prev, false, fmt.Errorf(errStoreBusy, l.origin)
	}
	defer unlock()
	st := storeFor(kind, l.dir)
	if stepUp {
		cur, found, err := st.get(l.origin)
		if err != nil {
			return prev, false, err
		}
		// The same session with the same device key, still live; a replaced or ended one is not restored.
		if !found || cur.SessionID != it.SessionID || cur.KeySeed != it.KeySeed || cur.endedWhy(time.Now()) != "" {
			return prev, false, fmt.Errorf(errStepUpLost, l.origin)
		}
		cur.AdminToken, cur.AdminExpiresAt = it.AdminToken, it.AdminExpiresAt
		return prev, false, st.put(l.origin, cur)
	}
	prev, replacing = l.previous()
	if old, _ := cliprofile.ReadSessionStore(l.dir, l.origin); old != "" && old != kind {
		_ = storeFor(old, l.dir).del(l.origin)
	}
	if err := st.put(l.origin, it); err != nil {
		return prev, false, err
	}
	return prev, replacing, cliprofile.WriteSessionStore(l.dir, l.origin, kind)
}

// previous is the live sign-in this computer holds for the organisation, which a new one replaces.
func (l *login) previous() (cliItem, bool) {
	kind, err := cliprofile.ReadSessionStore(l.dir, l.origin)
	if err != nil || kind == "" {
		return cliItem{}, false
	}
	it, ok, err := storeFor(kind, l.dir).get(l.origin)
	return it, err == nil && ok && it.endedWhy(time.Now()) == ""
}

// endPrevious signs the replaced session out at the server (DELETE /api/v1/me/cli-sessions/{id}) with the
// new session's token, or, when that is another person's sign-in (404), with the old session's own token
// while it lasts. A session that already ended is not an error; any other failure is one stderr line, and
// the new sign-in stands.
func (l *login) endPrevious(ctx context.Context, prev cliItem, token string) {
	route := "/api/v1/me/cli-sessions/" + prev.SessionID.String()
	err := cliCall(ctx, l.origin, http.MethodDelete, route, token, nil, nil)
	var ae *sdk.APIError
	if old, _ := prev.bearer(time.Now()); old != "" && errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		err = cliCall(ctx, l.origin, http.MethodDelete, route, old, nil, nil)
	}
	if err = sessionGone(err); err != nil {
		fmt.Fprintf(l.errOut, copyPreviousNotEnded+"\n", err)
	}
}

// whoIs names the person a fresh token belongs to, for the signed-in line: email, else principal.
func whoIs(ctx context.Context, origin, token string) string {
	var me meIdentity
	if err := cliCall(ctx, origin, http.MethodGet, "/api/v1/me", token, nil, &me); err != nil {
		return copyWhoUnknown
	}
	return me.name()
}

// awaitCallback serves the loopback callback on ln until the browser comes back for sign-in id. Only a
// request naming id counts, once; the server closes after it. finish(ok) writes the page that browser
// sees, after the token call, and must be called once code is returned.
func awaitCallback(ln net.Listener, id uuid.UUID, paste bool) (code string, finish func(bool), err error) {
	type callback struct {
		code   string
		denied bool
	}
	got, result := make(chan callback, 1), make(chan bool, 1)
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("id") != id.String() {
			http.Error(w, "not this sign-in", http.StatusNotFound)
			return
		}
		first := false
		once.Do(func() { first = true })
		if !first {
			http.Error(w, "this sign-in already came back", http.StatusGone)
			return
		}
		if q.Get("error") == "denied" {
			got <- callback{denied: true}
			writePage(w, pageDeniedTitle, pageDeniedBody)
			return
		}
		got <- callback{code: q.Get("code")}
		if <-result {
			writePage(w, pageTitle, pageSignedIn)
		} else {
			writePage(w, pageTitle, pageFailed)
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	stop := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	select {
	case cb := <-got:
		if cb.denied {
			stop()
			return "", nil, errors.New(errDenied)
		}
		return cb.code, func(ok bool) { result <- ok; stop() }, nil
	case <-time.After(loopbackWait):
		stop()
		if paste {
			return "", nil, errors.New(errNoCallbackPaste)
		}
		return "", nil, errors.New(errNoCallback)
	}
}

// writePage is the loopback page: one sentence, no script, nothing from the request.
func writePage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, "<!doctype html><meta charset=utf-8><title>%s</title><p>%s</p>\n", html.EscapeString(title), html.EscapeString(body))
}

// readPasted reads the pasted code K from the terminal without echo, or a line from a non-terminal input.
func readPasted(in io.Reader, prompt io.Writer) (string, error) {
	fmt.Fprint(prompt, copyPastePrompt)
	var line string
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(prompt)
		if err != nil {
			return "", err
		}
		line = string(b)
	} else {
		s, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && s == "" {
			return "", errors.New(errNoCode)
		}
		line = s
	}
	if line = strings.TrimSpace(line); line == "" {
		return "", errors.New(errNoCode)
	}
	return line, nil
}

// randomB64 is n random bytes, base64url with no padding.
func randomB64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// machineName is this computer's name for the approve page and Signed-in CLIs, within the server's bounds.
func machineName() string {
	h, err := os.Hostname()
	if h = strings.TrimSpace(h); err != nil || h == "" {
		h = runtime.GOOS
	}
	if len(h) > 100 {
		h = h[:100]
	}
	return h
}

// isWSL reports whether this Linux is WSL2, whose browser is the Windows one.
func isWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	return err == nil && strings.Contains(strings.ToLower(string(b)), "microsoft")
}

// canOpenBrowser is false in an SSH session and on Linux with no display (WSL2 uses the Windows browser).
// A var so a test can be either computer.
var canOpenBrowser = func() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	if runtime.GOOS == "linux" && !isWSL() {
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	}
	return true
}

// openBrowser opens u, which carries only the sign-in id, in the person's browser: on WSL2 the Windows one
// (wslview, else explorer.exe). A var so a test plays the browser.
var openBrowser = func(u string) error {
	name, args := "xdg-open", []string{u}
	switch {
	case runtime.GOOS == "darwin":
		name = "open"
	case runtime.GOOS == "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", u}
	case isWSL():
		name = "explorer.exe"
		if _, err := exec.LookPath("wslview"); err == nil {
			name = "wslview"
		}
	}
	c := exec.Command(name, args...)
	c.Env = cliutil.ScrubChildEnv(os.Environ())
	if err := c.Start(); err != nil {
		return err
	}
	go func() { _ = c.Wait() }()
	return nil
}
