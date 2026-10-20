// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/cliprofile"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/version"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// Seams a test swaps.
var (
	setupGOOS, setupGOARCH = runtime.GOOS, runtime.GOARCH
	setupVersion           = version.String
	setupExecutable        = os.Executable
	// packageRunnerd is the macOS package's runner, the one copy exempt from the hash (design ATTACH 11.4
	// step 3); packageOwner is the uid owning a file, -1 when the platform does not say.
	packageRunnerd = "/usr/local/wardyn/bin/wardyn-runnerd"
	packageOwner   = fileOwnerUID
	// releaseDownloadBase is where a release's assets live: <base>/v<version>/<asset>.
	releaseDownloadBase = "https://github.com/cjohnstoniv/wardyn/releases/download"
	setupConnectWait    = 60 * time.Second
	setupConnectPoll    = 2 * time.Second
)

// runnerSetupFlags are `wardyn runner setup`'s own flags; --url is the root's.
type runnerSetupFlags struct {
	pool, name, runnerd, artifactBase string
	makeDefault                       bool
}

// runnerSetupCmd is `wardyn runner setup` (design ATTACH 11.4): one command makes this computer one of the
// person's runners, and running it again is the upgrade. The registration token is minted only for a
// computer with no runner identity, held in memory and handed to wardyn-runnerd over a pipe; the CLI
// credential never reaches wardyn-runnerd.
func runnerSetupCmd(client clientFn) *cobra.Command {
	var f runnerSetupFlags
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Make this computer one of your runners: sign in, register, claim, join a pool and wait for it to connect (run it again to upgrade)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.makeDefault && f.pool == "" {
				return errors.New(errSetupDefaultNoPool)
			}
			return runRunnerSetup(cmd, client(), f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.pool, "pool", "", "also put the runner in your user pool of this name (an id or a name), made if you have none by it")
	fl.BoolVar(&f.makeDefault, "default", false, "with --pool, also make that pool your default")
	fl.StringVar(&f.name, "name", "", "runner name (defaults to the host name)")
	fl.StringVar(&f.runnerd, "runnerd", "", "use this wardyn-runnerd file (checked against the SHA-256 this wardyn pins) instead of finding or downloading one")
	fl.StringVar(&f.artifactBase, "artifact-base", "", "download wardyn-runnerd from <url>/v<version>/ (a mirror of the release); saved in cli.json for next time")
	return cmd
}

func runRunnerSetup(cmd *cobra.Command, c *sdk.Client, f runnerSetupFlags) error {
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	// Step 1: refusals before anything is read or written.
	if setupGOOS == "windows" {
		return errors.New(errSetupWindows)
	}
	if authHintFor.tokenFrom == "WARDYN_ADMIN_TOKEN" {
		return errors.New(errSetupAdminToken)
	}
	// Step 2: the organisation.
	if urlFromDefault {
		return errors.New(errNoOrg)
	}
	org, err := cliprofile.Normalize(c.BaseURL)
	if err != nil {
		return err
	}
	if err := federation.CheckOrgURL(org); err != nil {
		return err
	}
	var health struct {
		CLISignIn      bool   `json:"cli_sign_in"`
		CLIPasteSignIn bool   `json:"cli_paste_sign_in"`
		ArtifactBase   string `json:"cli_artifact_base"`
	}
	if err := cliCall(ctx, org, http.MethodGet, "/healthz", "", nil, &health); err != nil {
		return fmt.Errorf(errSetupUnreachable, org, err)
	}
	local, err := findRunnerIdentity()
	if err != nil {
		return err
	}
	registered := local.id.RunnerID != uuid.Nil
	if registered && local.id.OrgURLSHA256 != federation.OrgURLSHA256(org) {
		other := cmp.Or(serviceOrgURL(), errSetupOtherOrgName)
		return fmt.Errorf(errSetupOtherOrg, other, org, other, local.dir)
	}
	// Step 3: the wardyn-runnerd to run, checked before any credential is used.
	bin, cleanup, err := resolveRunnerd(ctx, f, org, health.ArtifactBase, out)
	if err != nil {
		return err
	}
	defer cleanup()
	// Step 1 again: with no --token or environment token, the stored sign-in, else a browser sign-in now.
	if c.Token == "" {
		if !health.CLISignIn {
			return fmt.Errorf(errSetupNoCredential, noSignInLine)
		}
		if c.Token, err = signInForSetup(cmd, org, health.CLIPasteSignIn); err != nil {
			return err
		}
	}
	// Step 4: the registration token, only for a computer with no runner identity.
	oldVersion := copySetupOldVersion
	var (
		token   string
		expires time.Time
	)
	if registered {
		fmt.Fprintf(out, copySetupRegistered+"\n", org)
		if v, err := c.GetMyRunner(ctx, local.id.RunnerID); err == nil && v.Version != "" {
			oldVersion = v.Version
		}
	} else {
		minted, err := c.MintRunnerToken(ctx, "")
		if err != nil {
			return selfMintRefusal(err)
		}
		token, expires = minted.Token, minted.ExpiresAt
		fmt.Fprintf(out, copySetupMinted+"\n", expires.Local().Format(time.RFC1123))
	}
	// Step 5: wardyn-runnerd setup registers (when given a token), writes and starts the service.
	if err := runRunnerdSetup(cmd, bin, org, f.name, token); err != nil {
		msg := fmt.Sprintf(errSetupRunnerdFail, err)
		if token != "" {
			msg += fmt.Sprintf(errSetupTokenLeft, expires.Local().Format(time.RFC1123))
		}
		return errors.New(msg)
	}
	if local, err = findRunnerIdentity(); err != nil {
		return err
	}
	if local.id.RunnerID == uuid.Nil || local.id.OrgURLSHA256 != federation.OrgURLSHA256(org) {
		return fmt.Errorf(errSetupRunnerdFail, errSetupNoIdentity)
	}
	// Step 6: claim, then --pool and --default, with the CLI's own credential.
	id := local.id.RunnerID
	if v, err := c.GetMyRunner(ctx, id); err != nil || v.State != types.RunnerClaimed {
		if _, err := c.ClaimRunner(ctx, id, local.id.Fingerprint); err != nil && registered {
			// An identity from before this setup: a rerun would refuse the same way.
			return fmt.Errorf(errSetupClaimRegistered, id, err, local.dir)
		} else if err != nil {
			return fmt.Errorf(errSetupClaimLeft, err, int(types.RunnerUnclaimedTTL.Hours()))
		}
		fmt.Fprintf(out, copySetupClaimed+"\n", id, local.id.Fingerprint)
	}
	if f.pool != "" {
		if err := claimIntoPool(cmd, c, id, f.pool, f.makeDefault); err != nil {
			return err
		}
	}
	// Step 7: wait for My runners to show it connected.
	return waitConnected(ctx, c, out, id, local.dir, org, oldVersion)
}

// signInForSetup is `wardyn login`'s browser sign-in for org, saving org as this CLI's organisation as
// login does, and returns the bearer it stored.
func signInForSetup(cmd *cobra.Command, org string, paste bool) (string, error) {
	dir, err := cliprofile.Dir()
	if err != nil {
		return "", err
	}
	if err := cliprofile.Write(dir, org); err != nil {
		return "", err
	}
	l := &login{origin: org, dir: dir, out: cmd.OutOrStdout(), errOut: cmd.ErrOrStderr(), in: cmd.InOrStdin(), paste: paste}
	if err := l.signIn(cmd.Context(), nil); err != nil {
		return "", err
	}
	s, err := loadSession(cmd.Context(), org)
	if err != nil {
		return "", err
	}
	return s.token, nil
}

// selfMintRefusal passes the server's own self_host_runner refusal through verbatim, with the step-up line
// under it (design ATTACH 11.4 step 4); any other failure is returned as it is.
func selfMintRefusal(err error) error {
	var apiErr *sdk.APIError
	if !errors.As(err, &apiErr) || apiErr.Reason != "capability_feature" {
		return err
	}
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(apiErr.Body), &body) != nil || body.Error == "" {
		return err
	}
	return errors.New(body.Error + "\n" + copySetupStepUp)
}

// runRunnerdSetup runs `wardyn-runnerd setup --org <org>`. A token goes over a pipe as file descriptor 3,
// so it is in no argument, no environment variable and no file; the environment passed is the shell's
// without the CLI's own credentials.
func runRunnerdSetup(cmd *cobra.Command, bin, org, name, token string) error {
	args := []string{"setup", "--org", org}
	if name != "" {
		args = append(args, "--name", name)
	}
	if path, _, err := anchorFile(org); err == nil {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--org-ca-file", path)
		}
	}
	if token != "" {
		args = append(args, "--token-file", "/dev/fd/3")
	}
	rc := exec.CommandContext(cmd.Context(), bin, args...)
	rc.Stdin, rc.Stdout, rc.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	rc.Env = withoutCLICredentials(os.Environ())
	if token != "" {
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		defer r.Close()
		_, err = io.WriteString(w, token+"\n")
		if cerr := w.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		rc.ExtraFiles = []*os.File{r}
	}
	return rc.Run()
}

// withoutCLICredentials is env without WARDYN_TOKEN and WARDYN_ADMIN_TOKEN, so runnerd's own claim step
// never sees a CLI credential, and without ANTHROPIC_API_KEY, which cliutil.ScrubChildEnv also keeps from
// every child.
func withoutCLICredentials(env []string) []string {
	kept := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k != "WARDYN_TOKEN" && k != "WARDYN_ADMIN_TOKEN" && k != "ANTHROPIC_API_KEY" {
			kept = append(kept, kv)
		}
	}
	return kept
}

// localRunner is this computer's runner identity as runner.json states it; the key is never read.
type localRunner struct {
	dir string
	id  struct {
		RunnerID     uuid.UUID `json:"runner_id"`
		Fingerprint  string    `json:"key_fingerprint"`
		OrgURLSHA256 string    `json:"org_url_sha256"`
	}
}

// findRunnerIdentity reads runner.json from the first of the places wardyn-runnerd setup keeps it: the
// state directory its service definition names, then the defaults. None is a zero localRunner.
func findRunnerIdentity() (localRunner, error) {
	for _, dir := range runnerStateDirs() {
		raw, err := os.ReadFile(filepath.Join(dir, "runner.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return localRunner{}, err
		}
		l := localRunner{dir: dir}
		if err := json.Unmarshal(raw, &l.id); err != nil {
			return localRunner{}, fmt.Errorf(errSetupBadIdentity, filepath.Join(dir, "runner.json"), err)
		}
		return l, nil
	}
	return localRunner{}, nil
}

func runnerStateDirs() []string {
	home, _ := os.UserHomeDir()
	if setupGOOS == "darwin" {
		return []string{filepath.Join(home, "Library", "Application Support", "wardyn", "runner")}
	}
	var dirs []string
	if dir := envFileSetting(filepath.Join(home, ".config", "wardyn", "runnerd.env"), "WARDYN_RUNNER_STATE_DIR"); filepath.IsAbs(dir) {
		dirs = append(dirs, dir)
	}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		dirs = append(dirs, filepath.Join(xdg, "wardyn", "runner"))
	}
	return append(dirs, filepath.Join(home, ".config", "wardyn", "runner"))
}

// serviceOrgURL is the organisation the installed runner service was set up for, "" when none is found.
func serviceOrgURL() string {
	home, _ := os.UserHomeDir()
	if setupGOOS != "darwin" {
		return envFileSetting(filepath.Join(home, ".config", "wardyn", "runnerd.env"), "WARDYN_RUNNER_ORG_URL")
	}
	plist, _ := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", "com.wardyn.runnerd.plist"))
	if m := regexp.MustCompile(`<string>--org</string>\s*<string>([^<]*)</string>`).FindSubmatch(plist); m != nil {
		return string(m[1])
	}
	return ""
}

// envFileSetting is key's value in the env file wardyn-runnerd setup writes, unquoted.
func envFileSetting(path, key string) string {
	b, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			v = strings.TrimSpace(v)
			if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
				v = strings.ReplaceAll(v[1:len(v)-1], `'\''`, "'")
			}
			return v
		}
	}
	return ""
}

// resolveRunnerd is the wardyn-runnerd to run (design ATTACH 11.4 step 3): --runnerd, else one beside this
// CLI or on PATH, else the download, each checked against pinnedRunnerdSHA256 on a private copy that is
// then the one run; the package's own root-owned copy is run where it is. cleanup removes the copy.
func resolveRunnerd(ctx context.Context, f runnerSetupFlags, org, advertised string, out io.Writer) (string, func(), error) {
	none := func() {}
	if pinnedRunnerdSHA256 == "" {
		return "", none, fmt.Errorf(errSetupNoPin, org)
	}
	src := f.runnerd
	if src == "" {
		for _, found := range foundRunnerds() {
			resolved, err := filepath.EvalSymlinks(found)
			if err != nil {
				continue
			}
			if resolved == packageRunnerd && packageRunnerdTrusted(resolved) {
				fmt.Fprintf(out, copySetupPackage+"\n", resolved)
				return resolved, none, nil
			}
			src = found
			break
		}
	}
	tmp, err := os.MkdirTemp("", "wardyn-runnerd-")
	if err != nil {
		return "", none, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	dst := filepath.Join(tmp, "wardyn-runnerd")
	var got string
	if src != "" {
		got, err = copyHashed(dst, func() (io.ReadCloser, error) { return os.Open(src) })
	} else {
		var u string
		if u, err = runnerdURL(f.artifactBase, advertised); err == nil {
			src = u
			got, err = copyHashed(dst, func() (io.ReadCloser, error) { return download(ctx, u) })
			if err != nil {
				err = fmt.Errorf(errSetupDownload, u, err)
			}
		}
	}
	if err == nil && got != pinnedRunnerdSHA256 {
		err = fmt.Errorf(errSetupHashMismatch, src, got, pinnedRunnerdSHA256, setupVersion())
	}
	if err == nil {
		err = os.Chmod(dst, 0o700)
	}
	if err != nil {
		cleanup()
		return "", none, err
	}
	fmt.Fprintf(out, copySetupRunnerd+"\n", src, got)
	return dst, cleanup, nil
}

// foundRunnerds is a wardyn-runnerd beside this CLI, then one on PATH.
func foundRunnerds() []string {
	var found []string
	if exe, err := setupExecutable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), "wardyn-runnerd"); fileExists(p) {
			found = append(found, p)
		}
	}
	if p, err := exec.LookPath("wardyn-runnerd"); err == nil {
		found = append(found, p)
	}
	return found
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// packageRunnerdTrusted is whether the package's runner and its directory are root-owned regular entries
// that nobody else can write, so the person's account cannot have replaced it.
func packageRunnerdTrusted(path string) bool {
	for i, p := range []string{path, filepath.Dir(path)} {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode().Perm()&0o022 != 0 || packageOwner(fi) != 0 || (i == 0 && !fi.Mode().IsRegular()) {
			return false
		}
	}
	return true
}

// runnerdURL is the download for this platform: <base>/v<version>/wardyn-runnerd-<os>-<arch>, the base
// being --artifact-base (then saved in cli.json), the saved artifact_base, the organisation's advertised
// mirror (/healthz.cli_artifact_base), else the release.
func runnerdURL(flag, advertised string) (string, error) {
	dir, err := cliprofile.Dir()
	if err != nil {
		return "", err
	}
	base := ""
	if flag != "" {
		if base, err = cliprofile.NormalizeArtifactBase(flag); err != nil {
			return "", err
		}
		if err := cliprofile.WriteArtifactBase(dir, base); err != nil {
			return "", err
		}
	} else if base, err = cliprofile.ReadArtifactBase(dir); err != nil {
		return "", err
	}
	base = strings.TrimSuffix(cmp.Or(base, advertised, releaseDownloadBase), "/")
	return fmt.Sprintf("%s/v%s/wardyn-runnerd-%s-%s", base, setupVersion(), setupGOOS, setupGOARCH), nil
}

// download GETs u through orgClient for each origin it is sent to. orgClient follows no redirect, and a
// release asset is served through one, so up to five https redirects are followed here: no credential is
// sent, and the pinned hash is the integrity check.
func download(ctx context.Context, u string) (io.ReadCloser, error) {
	for hop := 0; hop <= 5; hop++ {
		parsed, err := url.Parse(u)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		resp, err := orgClient(parsed.Scheme+"://"+parsed.Host, 5*time.Minute).Do(req)
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			return resp.Body, nil
		case resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.Header.Get("Location") != "":
			next, err := parsed.Parse(resp.Header.Get("Location"))
			resp.Body.Close()
			if err != nil {
				return nil, err
			}
			if next.Scheme != "https" {
				return nil, fmt.Errorf(errSetupRedirect, next.Redacted())
			}
			u = next.String()
		default:
			resp.Body.Close()
			return nil, fmt.Errorf(errSetupAnswered, resp.Status)
		}
	}
	return nil, errors.New(errSetupRedirects)
}

// copyHashed writes what open gives to dst, a new file, and returns its SHA-256.
func copyHashed(dst string, open func() (io.ReadCloser, error)) (string, error) {
	in, err := open()
	if err != nil {
		return "", err
	}
	defer in.Close()
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(in, 512<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return hex.EncodeToString(h.Sum(nil)), err
}

// waitConnected polls My runners until the runner is connected. When it is not within setupConnectWait it
// prints runnerd's last error and, after an upgrade, the restore line, and fails.
func waitConnected(ctx context.Context, c *sdk.Client, out io.Writer, id uuid.UUID, stateDir, org, oldVersion string) error {
	fmt.Fprintf(out, copySetupWaiting+"\n", setupConnectWait)
	deadline := time.Now().Add(setupConnectWait)
	for {
		if v, err := c.GetMyRunner(ctx, id); err == nil && v.Online {
			fmt.Fprintf(out, copySetupConnected+"\n", id)
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(setupConnectPoll):
		}
	}
	var msg bytes.Buffer
	fmt.Fprintf(&msg, errSetupNotConnected, setupConnectWait)
	var status struct {
		LastError string `json:"last_error"`
	}
	if raw, err := os.ReadFile(filepath.Join(stateDir, "status.json")); err == nil && json.Unmarshal(raw, &status) == nil && status.LastError != "" {
		fmt.Fprintf(&msg, "\n"+copySetupLastError, status.LastError)
	}
	if prev := filepath.Join(installedRunnerdDir(), "wardyn-runnerd.previous"); fileExists(prev) {
		fmt.Fprintf(&msg, "\n"+copySetupRestore, oldVersion, prev, org)
	}
	return errors.New(msg.String())
}

// installedRunnerdDir is where wardyn-runnerd setup installs the runner, and keeps the one it replaced.
func installedRunnerdDir() string {
	home, _ := os.UserHomeDir()
	if setupGOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "wardyn", "bin")
	}
	return filepath.Join(cmp.Or(os.Getenv("XDG_DATA_HOME"), filepath.Join(home, ".local", "share")), "wardyn", "bin")
}
