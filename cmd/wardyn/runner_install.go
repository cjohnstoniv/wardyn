// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/managedprefs"
	"github.com/cjohnstoniv/wardyn/internal/releasesig"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// Seams a test swaps.
var (
	releaseTrust        = releasesig.TrustedRoot
	managedArtifactBase = func() (string, error) {
		return managedprefs.Read(setupGOOS, "/Library/Managed Preferences", managedprefs.Domain, "ArtifactBase",
			func(file, key string) (string, error) {
				return managedprefs.PlutilExtract("/usr/bin/plutil", file, key)
			})
	}
)

// releaseVersion is a version a release is tagged with (v<version>): it goes into a URL and the pinned identity.
var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$`)

// runnerInstallCmd is `wardyn runner install` (#3312): `wardyn runner setup` with the runner downloaded from
// a signed release, and, on a computer that already runs one, `wardyn runner upgrade`.
func runnerInstallCmd(client clientFn, sess *sessionState) *cobra.Command {
	var f runnerSetupFlags
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Download this computer's runner from a signed release, verify it and set it up (run it again to upgrade)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.makeDefault && f.pool == "" {
				return errors.New(errSetupDefaultNoPool)
			}
			version, err := installVersion(f.release)
			if err != nil {
				return err
			}
			if local, err := findRunnerIdentity(); err != nil {
				return err
			} else if local.dir != "" && fileExists(installedRunnerd()) {
				return runRunnerUpgrade(cmd, version, f.artifactBase)
			}
			f.release = version
			return runRunnerSetup(cmd, client(), f, func() *sdk.Client {
				sess.reset()
				return client()
			})
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.release, "version", "", "the release to install (defaults to this CLI's own)")
	fl.StringVar(&f.pool, "pool", "", "also put the runner in your user pool of this name (an id or a name), made if you have none by it")
	fl.BoolVar(&f.makeDefault, "default", false, "with --pool, also make that pool your default")
	fl.StringVar(&f.name, "name", "", "runner name (defaults to the host name)")
	fl.StringVar(&f.artifactBase, "artifact-base", "", "download the release from <url>/v<version>/ (a mirror of the release); saved in cli.json for next time")
	return cmd
}

// runnerUpgradeCmd is `wardyn runner upgrade` (#3312).
func runnerUpgradeCmd() *cobra.Command {
	var version, base string
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade this computer's runner to a signed release, and put the previous one back if the new one does not start",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			v, err := installVersion(version)
			if err != nil {
				return err
			}
			if local, err := findRunnerIdentity(); err != nil {
				return err
			} else if local.dir == "" || !fileExists(installedRunnerd()) {
				return errors.New(errUpgradeNotInstalled)
			}
			return runRunnerUpgrade(cmd, v, base)
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "the release to upgrade to (defaults to this CLI's own)")
	cmd.Flags().StringVar(&base, "artifact-base", "", "download the release from <url>/v<version>/ (a mirror of the release); saved in cli.json for next time")
	return cmd
}

// installVersion is --version, else this CLI's own release; a development build has none.
func installVersion(flag string) (string, error) {
	if setupGOOS == "windows" {
		return "", errors.New(errSetupWindows)
	}
	v := cmp.Or(flag, setupVersion())
	if !releaseVersion.MatchString(v) {
		return "", fmt.Errorf(errInstallVersion, v)
	}
	return v, nil
}

func installedRunnerd() string { return filepath.Join(installedRunnerdDir(), "wardyn-runnerd") }

// runRunnerUpgrade replaces the installed runner with version's verified one. wardyn-runnerd setup stages the
// file beside the installed one, stops the service, keeps the old file as .previous, renames the new one into
// place and starts the service; the runner's status then has to say it started and connected. A runner that
// does not is replaced by .previous the same way, after its service files are put back as they were. A runner
// already at version is left alone.
func runRunnerUpgrade(cmd *cobra.Command, version, flagBase string) error {
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	installed := installedRunnerd()
	current := installedRunnerdVersion(ctx, installed)
	if current == version {
		fmt.Fprintf(out, copyUpgradeCurrent+"\n", version)
		return nil
	}
	org := serviceOrgURL()
	if org == "" {
		return errors.New(errUpgradeNoOrg)
	}
	local, err := findRunnerIdentity()
	if err != nil {
		return err
	}
	prev := installed + ".previous"
	// A .previous that cannot be examined is not "none before": the swap would then pick it up as one it kept.
	// It is looked at first, so a refusal comes before anything is downloaded or announced.
	prevBefore, err := os.Stat(prev)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf(errUpgradePreviousUnreadable, err)
	}
	var health struct {
		ArtifactBase string `json:"cli_artifact_base"`
	}
	_ = cliCall(ctx, org, http.MethodGet, "/healthz", "", nil, &health) // the mirror it advertises, if it answers
	bin, cleanup, err := signedRunnerd(ctx, runnerSetupFlags{artifactBase: flagBase, release: version}, org, health.ArtifactBase, out)
	if err != nil {
		return err
	}
	defer cleanup()
	from := cmp.Or(current, copySetupOldVersion)
	fmt.Fprintf(out, copyUpgradeSwapping+"\n", from, version)
	// The new setup moves the old one's settings out of the service files into runnerd.json, where an older
	// setup does not look, so a rollback puts the files back as they are now.
	kept, err := keepServiceFiles(serviceFiles())
	if err != nil {
		return err
	}
	started := time.Now()
	failure := runRunnerdSetup(cmd, bin, org, "", "")
	if failure == nil {
		if failure = waitStarted(ctx, local.dir, started); failure == nil {
			fmt.Fprintf(out, copyUpgradeDone+"\n", version)
			return nil
		}
	}
	// Roll back only to a .previous this swap kept: a setup that failed before the rename left the old file,
	// and one whose old file was already these bytes kept none, so an older .previous is stale.
	if !sameFile(installed, bin) || !keptNow(prev, prevBefore) {
		return fmt.Errorf(errUpgradeNotReplaced, version, failure)
	}
	// The rollback runs from a copy of .previous of its own: the old setup being run may keep the failed binary
	// over .previous before it restores, and a restore that then failed would leave no copy of the old runner.
	rollbackFrom := installed + ".rollback"
	if err := keepRollbackCopy(prev, rollbackFrom); err != nil {
		return fmt.Errorf(errUpgradeRollbackFailed, version, failure, from, fmt.Errorf(errUpgradeRollbackCopy, from, err), restoreLine(from, prev, org))
	}
	fmt.Fprintf(out, copyUpgradeRollingBack+"\n", version, failure, from)
	if err := restoreServiceFiles(kept); err != nil {
		return fmt.Errorf(errUpgradeRollbackFailed, version, failure, from, err, restoreLine(from, rollbackFrom, org))
	}
	started = time.Now()
	if err := runRunnerdSetup(cmd, rollbackFrom, org, "", ""); err != nil {
		return fmt.Errorf(errUpgradeRollbackFailed, version, failure, from, err, restoreLine(from, rollbackFrom, org))
	}
	if err := waitStarted(ctx, local.dir, started); err != nil {
		return fmt.Errorf(errUpgradeRollbackFailed, version, failure, from, err, restoreLine(from, rollbackFrom, org))
	}
	_ = os.Remove(rollbackFrom)
	return fmt.Errorf(errUpgradeRolledBack, version, failure, from)
}

// keepRollbackCopy writes the bytes of prev to dst, atomically and with prev's mode, and reads them back to
// check them. A copy an earlier failed rollback left is replaced.
func keepRollbackCopy(prev, dst string) error {
	fi, err := os.Stat(prev)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(prev)
	if err != nil {
		return err
	}
	_ = os.Remove(dst)
	if err := writeFileAtomic(dst, data, fi.Mode().Perm()); err != nil {
		return err
	}
	if got, err := os.ReadFile(dst); err != nil || !bytes.Equal(got, data) {
		_ = os.Remove(dst)
		return errors.Join(errors.New(errUpgradeRollbackCopyDiffers), err)
	}
	return nil
}

// keptFile is a service file as it was before an upgrade: its bytes and mode, or that it was absent.
type keptFile struct {
	path   string
	data   []byte
	mode   os.FileMode
	absent bool
}

// serviceFiles are the files the installed runner's service takes its settings from: the environment file
// (Linux) or LaunchAgent (macOS) a setup before 0.10 wrote, the environment file a macOS --no-service wrote,
// and runnerd.json, where wardyn-runnerd setup keeps them since.
func serviceFiles() []string {
	home, _ := os.UserHomeDir()
	if setupGOOS == "darwin" {
		data := filepath.Join(home, "Library", "Application Support", "wardyn")
		return []string{filepath.Join(home, "Library", "LaunchAgents", "com.wardyn.runnerd.plist"),
			filepath.Join(data, "runnerd.env"), filepath.Join(data, "runnerd.json")}
	}
	cfg := filepath.Join(home, ".config")
	return []string{filepath.Join(cfg, "wardyn", "runnerd.env"), filepath.Join(cmp.Or(os.Getenv("XDG_CONFIG_HOME"), cfg), "wardyn", "runnerd.json")}
}

// keepServiceFiles reads each of paths as it is now; one that is not there is kept as absent.
func keepServiceFiles(paths []string) ([]keptFile, error) {
	kept := make([]keptFile, 0, len(paths))
	for _, path := range paths {
		k := keptFile{path: path}
		fi, err := os.Stat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			k.absent = true
		case err != nil:
			return nil, err
		default:
			k.mode = fi.Mode().Perm()
			if k.data, err = os.ReadFile(path); err != nil {
				return nil, err
			}
		}
		kept = append(kept, k)
	}
	return kept, nil
}

// restoreServiceFiles puts each kept file back byte for byte with its old mode, and removes one that was
// absent.
func restoreServiceFiles(kept []keptFile) error {
	for _, k := range kept {
		if k.absent {
			if err := os.Remove(k.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		// writeFileAtomic keeps the mode of a file already there, so that file takes the old mode first.
		if err := os.Chmod(k.path, k.mode); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := writeFileAtomic(k.path, k.data, k.mode); err != nil {
			return err
		}
	}
	return nil
}

// signedRunnerd is the runner of release f.release (design #3312): SHA256SUMS and its Sigstore bundle are
// downloaded, the bundle verified offline against the release workflow's pinned identity, and the asset for
// this platform checked against the verified sums, on a private copy that is then the one run.
func signedRunnerd(ctx context.Context, f runnerSetupFlags, _, advertised string, out io.Writer) (string, func(), error) {
	none := func() {}
	asset, err := runnerAsset(setupGOOS, setupGOARCH)
	if err != nil {
		return "", none, err
	}
	base, err := artifactBase(f.artifactBase, advertised)
	if err != nil {
		return "", none, err
	}
	dir := fmt.Sprintf("%s/v%s/", base, f.release)
	sums, err := fetchSmall(ctx, dir+"SHA256SUMS")
	if err != nil {
		return "", none, fmt.Errorf(errInstallSums, dir, err)
	}
	bundle, err := fetchSmall(ctx, dir+"SHA256SUMS.sigstore.json")
	if isNotFound(err) {
		return "", none, fmt.Errorf(errInstallNoBundle, dir, f.release)
	} else if err != nil {
		return "", none, fmt.Errorf(errInstallSums, dir, err)
	}
	tm, err := releaseTrust()
	if err == nil {
		err = releasesig.VerifySums(tm, sums, bundle, f.release)
	}
	if err != nil {
		return "", none, fmt.Errorf(errInstallUnverified, dir, err)
	}
	want, ok := listedSum(sums, asset)
	if !ok {
		return "", none, fmt.Errorf(errInstallNotListed, f.release, asset)
	}
	tmp, err := os.MkdirTemp("", "wardyn-runnerd-")
	if err != nil {
		return "", none, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	dst := filepath.Join(tmp, "wardyn-runnerd")
	got, err := copyHashed(dst, func() (io.ReadCloser, error) { return download(ctx, dir+asset) })
	switch {
	case isNotFound(err):
		err = fmt.Errorf(errInstallAssetMissing, base, asset, f.release)
	case err != nil:
		err = fmt.Errorf(errInstallDownload, dir+asset, err)
	case got != want:
		err = fmt.Errorf(errInstallChecksum, dir+asset, got, want)
	default:
		err = os.Chmod(dst, 0o700)
	}
	if err != nil {
		cleanup()
		return "", none, err
	}
	fmt.Fprintf(out, copyInstallVerified+"\n", dir+asset, f.release, releasesig.Identity(f.release), got)
	return dst, cleanup, nil
}

// runnerAsset is the release's runner for goos/goarch, as the release names it.
func runnerAsset(goos, goarch string) (string, error) {
	if (goos == "linux" || goos == "darwin") && (goarch == "amd64" || goarch == "arm64") {
		return "wardyn-runnerd-" + goos + "-" + goarch, nil
	}
	if goos == "windows" {
		return "", errors.New(errSetupWindows)
	}
	return "", fmt.Errorf(errInstallPlatform, goos, goarch)
}

// listedSum is asset's SHA-256 in a sha256sum listing, whose names the release writes as ./<asset>.
func listedSum(sums []byte, asset string) (string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(strings.TrimPrefix(f[1], "*"), "./") == asset {
			return f[0], true
		}
	}
	return "", false
}

func fetchSmall(ctx context.Context, u string) ([]byte, error) {
	body, err := download(ctx, u)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, 1<<20))
}

func isNotFound(err error) bool {
	var s statusError
	return errors.As(err, &s) && s.code == http.StatusNotFound
}

// installedRunnerdVersion is what the runner at path says its version is, "" when it does not say.
func installedRunnerdVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, path, "--version")
	c.Env = cliutil.ScrubChildEnv(os.Environ())
	b, err := c.Output()
	if f := strings.Fields(string(b)); err == nil && len(f) > 0 {
		return f[len(f)-1]
	}
	return ""
}

// waitStarted waits for the runner's status file to say a service started after since has connected.
func waitStarted(ctx context.Context, stateDir string, since time.Time) error {
	deadline := time.Now().Add(setupConnectWait)
	var st struct {
		State     string    `json:"state"`
		Since     time.Time `json:"since"`
		LastError string    `json:"last_error"`
	}
	for {
		raw, err := os.ReadFile(filepath.Join(stateDir, "status.json"))
		if err == nil && json.Unmarshal(raw, &st) == nil && st.State == "connected" && !st.Since.Before(since.Truncate(time.Second)) {
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
	msg := fmt.Sprintf(errSetupNotConnected, setupConnectWait)
	if st.LastError != "" {
		msg += "; " + fmt.Sprintf(copySetupLastError, st.LastError)
	}
	return errors.New(msg)
}

// keptNow is whether prev exists and is not the file before was (nil when there was none).
func keptNow(prev string, before os.FileInfo) bool {
	after, err := os.Stat(prev)
	return err == nil && (before == nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()))
}

func sameFile(a, b string) bool {
	x, errA := os.ReadFile(a)
	y, errB := os.ReadFile(b)
	return errA == nil && errB == nil && bytes.Equal(x, y)
}

// runnerOfferLine is the notice that this computer's runner is older than its organisation's release.
func runnerOfferLine(ctx context.Context, server string) string {
	release, _, _ := strings.Cut(server, "+")
	if !fileExists(installedRunnerd()) {
		return ""
	}
	if v := installedRunnerdVersion(ctx, installedRunnerd()); v != "" && older(v, release) {
		return fmt.Sprintf(copyNoticeRunnerOlder, v, release, release)
	}
	return ""
}
