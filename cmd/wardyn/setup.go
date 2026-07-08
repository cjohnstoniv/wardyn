// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// setup.go turns "enable a stronger confinement tier" into the ONE right thing
// for THIS host. gVisor (Wall) and Kata (Vault) are enabled by registering a
// runtime with the Docker daemon that actually runs the agent's containers — so
// the correct action depends entirely on what kind of Docker this host has:
//
//   - native rootful dockerd  -> install runsc/kata + register + reload the daemon
//   - Docker Desktop (any OS)  -> DEAD END: the engine is in a managed VM whose
//                                 runtime list resets on restart; point wardynd
//                                 at a native dockerd instead. NEVER restart it.
//   - macOS                    -> engine is always in a VM; use Colima (a VM you
//                                 control), not Docker Desktop's.
//   - rootless dockerd         -> runsc only runs with --TESTONLY-unsafe-nonroot,
//                                 which defeats the isolation; refuse.
//   - no /dev/kvm              -> Vault (hardware-virtualized) is impossible.
//
// Kata installs are FLOORED at kataMinVersion: CVE-2026-44210/-47243 let a
// compromised Kata guest reach host-root via virtiofsd on older virtio-fs
// configs, so kataScript refuses (fail closed) to install anything below the
// floor — whether it resolved "latest" from GitHub or an operator-set
// WARDYN_KATA_VERSION override.
//
// Everything runs under the OPERATOR's shell/sudo; wardynd is only ever *pointed
// at* the result via DOCKER_HOST (it builds its docker client from the standard
// docker env), so the security daemon stays unprivileged and never installs
// anything.

// kataMinVersion is the Kata Containers security floor: versions below this
// predate the virtio-fs hardening for CVE-2026-44210/-47243 (guest-root ->
// host-root via virtiofsd on a permissive virtio_fs_extra_args/kernel_params
// config). kataScript refuses to install below it — see setup.go's package doc.
const kataMinVersion = "3.31.0"

func setupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Set up host components for the stronger confinement tiers (Wall / Vault)",
		Long: "Enable Wardyn's stronger confinement tiers on this host. These commands detect your\n" +
			"OS and Docker setup and either install the runtime, print the exact one-time command,\n" +
			"or explain honestly why a tier isn't reachable here (and what to do instead).\n\n" +
			"They run with YOUR privileges (sudo as needed) — the Wardyn daemon never installs\n" +
			"anything. After running one, Re-check in the Getting-started UI: wardynd re-probes\n" +
			"`docker info` and the tier flips to available.",
	}
	cmd.AddCommand(
		setupTierCmd("wall"),
		setupTierCmd("vault"),
	)
	return cmd
}

func setupTierCmd(use string) *cobra.Command {
	var run, yes bool
	label := "Wall (gVisor)"
	if use == "vault" {
		label = "Vault (Kata Containers)"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: fmt.Sprintf("Set up the %s tier", label),
		RunE: func(_ *cobra.Command, _ []string) error {
			e := detectDocker()
			p := planWall(e)
			if use == "vault" {
				p = planVault(e)
			}
			return executePlan(p, run, yes)
		},
	}
	cmd.Flags().BoolVar(&run, "run", false, "actually execute the install (default: print the plan)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	return cmd
}

// ---------------------------------------------------------------------------
// Detection
// ---------------------------------------------------------------------------

type dockerEnv struct {
	goos       string // runtime.GOOS
	hasDocker  bool   // docker on PATH
	desktop    bool   // `docker info` OperatingSystem contains "Docker Desktop"
	osType     string // "linux" | "windows" (docker engine, not host)
	rootless   bool   // SecurityOptions has "rootless" || DOCKER_HOST under /run/user
	wsl        bool   // /proc/version mentions microsoft
	initSys    string // "systemd" | "openrc" | "sysv"
	family     string // "debian" | "fedora" | "arch" | "suse" | "other"
	selinux    bool   // SELinux enforcing
	kvm        bool   // /dev/kvm present
	colima     bool   // colima on PATH (macOS)
	dockerHost string // DOCKER_HOST
}

func detectDocker() dockerEnv {
	e := dockerEnv{goos: runtime.GOOS, dockerHost: os.Getenv("DOCKER_HOST")}
	if _, err := exec.LookPath("docker"); err == nil {
		e.hasDocker = true
	}
	e.wsl = strings.Contains(strings.ToLower(readFileTrim("/proc/version")), "microsoft")
	e.initSys = detectInit()
	e.family = parseOSFamily(osReleaseField("ID"), osReleaseField("ID_LIKE"))
	e.selinux = readFileTrim("/sys/fs/selinux/enforce") == "1"
	if _, err := os.Stat("/dev/kvm"); err == nil {
		e.kvm = true
	}
	if _, err := exec.LookPath("colima"); err == nil {
		e.colima = true
	}
	if strings.HasPrefix(e.dockerHost, "unix:///run/user/") {
		e.rootless = true
	}
	if e.hasDocker {
		if info, ok := dockerInfo(); ok {
			e.osType = info.OSType
			e.desktop = strings.Contains(info.OperatingSystem, "Docker Desktop")
			for _, s := range info.SecurityOptions {
				if strings.Contains(s, "rootless") {
					e.rootless = true
				}
			}
		}
	}
	return e
}

type dockerInfoJSON struct {
	OperatingSystem string   `json:"OperatingSystem"`
	OSType          string   `json:"OSType"`
	SecurityOptions []string `json:"SecurityOptions"`
}

func dockerInfo() (dockerInfoJSON, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{json .}}").Output()
	if err != nil {
		return dockerInfoJSON{}, false
	}
	var info dockerInfoJSON
	if err := json.Unmarshal(out, &info); err != nil {
		return dockerInfoJSON{}, false
	}
	return info, true
}

func detectInit() string {
	switch readFileTrim("/proc/1/comm") {
	case "systemd":
		return "systemd"
	case "openrc-init", "openrc":
		return "openrc"
	}
	if _, err := os.Stat("/run/openrc"); err == nil {
		return "openrc"
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		return "systemd"
	}
	return "sysv"
}

// parseOSFamily maps /etc/os-release ID + ID_LIKE onto a package-manager family.
func parseOSFamily(id, idLike string) string {
	hay := strings.ToLower(id + " " + idLike)
	switch {
	case matchAny(hay, "debian", "ubuntu"):
		return "debian"
	case matchAny(hay, "fedora", "rhel", "centos", "rocky", "almalinux"):
		return "fedora"
	case matchAny(hay, "arch"):
		return "arch"
	case matchAny(hay, "suse"):
		return "suse"
	}
	return "other"
}

// ---------------------------------------------------------------------------
// Planning
// ---------------------------------------------------------------------------

type action int

const (
	actAuto        action = iota // clean, supported install (offer --run)
	actPrint                     // host-specific but scriptable (offer --run)
	actUnsupported               // not reachable here — explain + alternative; never run
)

type plan struct {
	title    string
	why      string // honest one-paragraph explanation
	script   string // install steps (auto/print) or informational guidance (unsupported)
	hostHint string // DOCKER_HOST wiring line, when an alternate dockerd is involved
	action   action
}

func planWall(e dockerEnv) plan {
	switch {
	case e.goos == "windows":
		return plan{action: actUnsupported, title: "Wall on Windows",
			why: "The Windows wardyn.exe can't reach a Linux sandbox runtime. Run wardynd and the wardyn " +
				"CLI inside a WSL2 distro with a native Docker Engine, then enable Wall there:",
			script:   wslNativeDockerSteps(),
			hostHint: dockerHostHint("unix:///var/run/wardyn-docker.sock")}
	case e.goos == "darwin":
		return planWallMac(e)
	case !e.hasDocker:
		return plan{action: actUnsupported, title: "Docker not found",
			why: "Docker isn't installed or isn't on PATH. Install Docker first, then re-run."}
	case e.osType == "windows":
		return plan{action: actUnsupported, title: "Windows containers",
			why: "Docker is in Windows-containers mode. Wall (gVisor) and Vault (Kata) are Linux-kernel " +
				"sandboxes — switch Docker to Linux containers to use them."}
	case e.desktop:
		return planWallDesktop(e)
	case e.rootless:
		return plan{action: actUnsupported, title: "Wall under rootless Docker",
			why: "runsc registers under rootless Docker, but current gVisor only starts a sandbox there with " +
				"the --TESTONLY-unsafe-nonroot flag, which disables the host isolation Wall exists to provide. " +
				"Use a rootful dockerd for a real Wall tier."}
	default:
		return planWallNativeLinux(e)
	}
}

func planWallNativeLinux(e dockerEnv) plan {
	if e.family == "debian" && e.initSys == "systemd" {
		return plan{action: actAuto, title: "Enable the Wall tier (gVisor)",
			why: "Installs gVisor (runsc) from the official apt repo and registers it with Docker. Runs with " +
				"your sudo; wardynd never installs anything.",
			script: gvisorAptScript(e)}
	}
	why := "Installs the checksum-verified gVisor binaries to /usr/local/bin and registers runsc with Docker."
	if e.selinux {
		why += " SELinux is enforcing — gVisor has no SELinux integration, so runsc containers need " +
			"--security-opt label=disable (wardyn's docker driver applies this)."
	}
	return plan{action: actPrint, title: "Enable the Wall tier (gVisor)", why: why, script: gvisorBinaryScript(e)}
}

func planWallDesktop(e dockerEnv) plan {
	steps := dockerDesktopLinuxSteps()
	if e.wsl {
		steps = wslNativeDockerSteps()
	}
	return plan{action: actUnsupported, title: "Wall needs a native Docker engine",
		why: "This host runs Docker Desktop — its engine lives in a Docker-managed VM whose runtime list " +
			"resets on restart, so a runsc runtime can't persist there (that's why `systemctl restart docker` " +
			"can't help). Run a native Docker Engine and point wardynd at it:",
		script:   steps,
		hostHint: dockerHostHint("unix:///var/run/wardyn-docker.sock")}
}

func planWallMac(e dockerEnv) plan {
	why := "macOS runs Docker inside a Linux VM. Docker Desktop's VM can't persist a custom runtime, so use " +
		"Colima (a VM you control): install runsc inside it and register the runsc runtime."
	if !e.colima {
		why += " Colima isn't installed yet — `brew install colima` first."
	}
	return plan{action: actPrint, title: "Enable the Wall tier (gVisor) on macOS",
		why: why, script: colimaWallScript(), hostHint: dockerHostHint("unix://$HOME/.colima/default/docker.sock")}
}

func planVault(e dockerEnv) plan {
	switch {
	case e.goos == "windows":
		return plan{action: actUnsupported, title: "Vault on Windows",
			why: "Vault (Kata) needs /dev/kvm, which Windows/WSL2 doesn't expose to the Docker engine. Use a " +
				"bare-metal Linux host or a nested-virt-enabled cloud VM for Vault."}
	case e.goos == "darwin":
		return plan{action: actUnsupported, title: "Vault on macOS",
			why: "macOS exposes no nested KVM to the Linux VM, so Kata can't run. The strongest tier on macOS " +
				"is Wall (gVisor) — run `wardyn setup wall`."}
	case !e.hasDocker:
		return plan{action: actUnsupported, title: "Docker not found",
			why: "Docker isn't installed or isn't on PATH. Install Docker first, then re-run."}
	case e.desktop:
		return plan{action: actUnsupported, title: "Vault needs a native Docker engine + KVM",
			why: "Docker Desktop's managed VM can't persist a kata runtime, and Vault also needs /dev/kvm. Use " +
				"a native dockerd on a KVM-capable Linux host and point wardynd at it.",
			hostHint: dockerHostHint("unix:///var/run/wardyn-docker.sock")}
	case e.rootless:
		return plan{action: actUnsupported, title: "Vault under rootless Docker",
			why: "Kata device passthrough isn't supported under rootless Docker. Use a rootful dockerd on a " +
				"KVM-capable host for Vault."}
	case !e.kvm:
		return plan{action: actUnsupported, title: "Vault needs KVM hardware",
			why: "This host has no /dev/kvm, so Vault (hardware-virtualized) isn't possible. The strongest " +
				"available tier here is Wall (gVisor) — run `wardyn setup wall`."}
	default:
		why := "Installs Kata Containers and checks host virtualization. Kata is host/kernel-sensitive, so this " +
			"verifies with `kata-runtime check` and prints the daemon.json runtime for you to add (it won't " +
			"edit your daemon.json). Refuses to install below v" + kataMinVersion + " (CVE-2026-44210/-47243: " +
			"virtio-fs guest-root -> host-root via virtiofsd)."
		if e.wsl {
			why += " NOTE: under WSL2, Kata is nested QEMU inside Hyper-V — experimental, not an officially " +
				"supported target."
		}
		return plan{action: actPrint, title: "Enable the Vault tier (Kata Containers)", why: why, script: kataScript()}
	}
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

func executePlan(p plan, run, yes bool) error {
	fmt.Printf("%s\n\n%s\n", p.title, p.why)

	if p.action == actUnsupported {
		if p.script != "" {
			fmt.Print("\n" + indentBlock(p.script) + "\n")
		}
		if p.hostHint != "" {
			fmt.Println(p.hostHint)
		}
		return nil // never run an install that can't work here
	}

	fmt.Println("\nIt will run (with sudo as needed):")
	fmt.Print(indentBlock(p.script))
	if p.hostHint != "" {
		fmt.Println("\n" + p.hostHint)
	}
	if !run {
		fmt.Println("\nRe-run with --run to execute (or copy the commands above), then Re-check in the UI.")
		return nil
	}
	if !yes && !confirm("\nProceed now?") {
		fmt.Println("Aborted.")
		return nil
	}
	return runScript(p.script)
}

// ---------------------------------------------------------------------------
// Scripts (trusted constants — no user input is interpolated; the only variable
// parts are fixed branch selections like the restart line.)
// ---------------------------------------------------------------------------

// restartDocker returns the init-appropriate reload/restart. reload picks up new
// runtimes without killing running containers; restart is the fallback.
func restartDocker(e dockerEnv) string {
	switch e.initSys {
	case "systemd":
		return "sudo systemctl reload docker || sudo systemctl restart docker"
	case "openrc":
		return "sudo rc-service docker restart"
	default:
		return "sudo service docker restart"
	}
}

const gvisorDone = `echo "OK gVisor installed. Re-check Wardyn's Getting started — the Wall tier should now be available."`

func gvisorAptScript(e dockerEnv) string {
	return `set -euo pipefail
sudo apt-get update && sudo apt-get install -y apt-transport-https ca-certificates curl gnupg
curl -fsSL https://gvisor.dev/archive.key | sudo gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] https://storage.googleapis.com/gvisor/releases release main" | sudo tee /etc/apt/sources.list.d/gvisor.list > /dev/null
sudo apt-get update && sudo apt-get install -y runsc
sudo runsc install
` + restartDocker(e) + "\n" + gvisorDone + "\n"
}

func gvisorBinaryScript(e dockerEnv) string {
	return `set -euo pipefail
ARCH="$(uname -m)"
URL="https://storage.googleapis.com/gvisor/releases/release/latest/${ARCH}"
TMP="$(mktemp -d)"; cd "$TMP"
echo "-> downloading runsc + shim for ${ARCH}"
wget --quiet "${URL}/runsc" "${URL}/runsc.sha512" "${URL}/containerd-shim-runsc-v1" "${URL}/containerd-shim-runsc-v1.sha512"
sha512sum -c runsc.sha512 -c containerd-shim-runsc-v1.sha512
chmod a+rx runsc containerd-shim-runsc-v1
sudo mv runsc containerd-shim-runsc-v1 /usr/local/bin
sudo /usr/local/bin/runsc install
` + restartDocker(e) + "\n" + gvisorDone + "\n"
}

// wslNativeDockerSteps is printed (never auto-run) when the host is WSL2 +
// Docker Desktop: stand up a native dockerd on its own socket so it coexists
// with Docker Desktop, then enable Wall against it.
func wslNativeDockerSteps() string {
	return `# Run these INSIDE your WSL2 Linux distro (e.g. Ubuntu), not the docker-desktop distro.
# 1. Enable systemd in WSL once, then restart WSL from Windows:
#      printf '[boot]\nsystemd=true\n' | sudo tee -a /etc/wsl.conf
#      # in PowerShell:  wsl --shutdown   (then reopen the distro)
# 2. Install native Docker Engine:
curl -fsSL https://get.docker.com | sudo sh
# 3. Run it on its own socket so it coexists with Docker Desktop:
sudo mkdir -p /etc/systemd/system/docker.service.d
sudo tee /etc/systemd/system/docker.service.d/wardyn.conf > /dev/null <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/bin/dockerd -H unix:///var/run/wardyn-docker.sock --data-root /var/lib/wardyn-docker
EOF
sudo systemctl mask docker.socket
sudo systemctl daemon-reload && sudo systemctl enable --now docker
# 4. Enable Wall against THIS engine:
DOCKER_HOST=unix:///var/run/wardyn-docker.sock wardyn setup wall --run
#
# Simpler alternative (REPLACE): disable Docker Desktop's WSL integration for this distro in
# Docker Desktop settings, then this dockerd owns the default socket — skip step 3.`
}

func dockerDesktopLinuxSteps() string {
	return `# Install a native Docker Engine alongside Docker Desktop and point wardynd at it.
# 1. Install docker-ce:
curl -fsSL https://get.docker.com | sudo sh
# 2. Run it on its own socket (coexists with Docker Desktop):
sudo mkdir -p /etc/systemd/system/docker.service.d
sudo tee /etc/systemd/system/docker.service.d/wardyn.conf > /dev/null <<'EOF'
[Service]
ExecStart=
ExecStart=/usr/bin/dockerd -H unix:///var/run/wardyn-docker.sock --data-root /var/lib/wardyn-docker
EOF
sudo systemctl mask docker.socket
sudo systemctl daemon-reload && sudo systemctl enable --now docker
# 3. Enable Wall against THIS engine:
DOCKER_HOST=unix:///var/run/wardyn-docker.sock wardyn setup wall --run`
}

func colimaWallScript() string {
	return `set -euo pipefail
# Requires Colima:  brew install colima
# 1. Start a Docker VM you control:
colima start --runtime docker
# 2. Install gVisor inside the VM and register the runsc runtime (persists on the VM disk):
colima ssh -- sudo sh -euc '
  A=$(uname -m)
  U=https://storage.googleapis.com/gvisor/releases/release/latest/$A
  wget -qO /usr/local/bin/runsc $U/runsc
  wget -qO /usr/local/bin/containerd-shim-runsc-v1 $U/containerd-shim-runsc-v1
  chmod 0755 /usr/local/bin/runsc /usr/local/bin/containerd-shim-runsc-v1
  runsc install
  systemctl restart docker || service docker restart'
# To survive colima stop/start, bake step 2 into ` + "`colima start --edit`" + ` as a provision: block.`
}

// kataScript installs Kata + verifies KVM, then PRINTS the daemon.json runtime
// to add — it never edits daemon.json (avoids clobbering an existing one).
//
// The resolved version — "latest" from the GitHub API, or an operator-set
// WARDYN_KATA_VERSION override — is FLOORED at kataMinVersion (via `sort -V`,
// no extra dependency) and the install refuses (exit 1) below it: an old
// mirror, a GitHub API hiccup, or an explicit downgrade override must never
// silently reintroduce CVE-2026-44210/-47243 (virtio-fs guest-root ->
// host-root via virtiofsd).
func kataScript() string {
	return `set -euo pipefail
test -e /dev/kvm || { echo "no /dev/kvm — Vault needs KVM-capable hardware"; exit 1; }
echo "-> installing Kata Containers (static build) to /opt/kata"
KATA_MIN_VERSION="` + kataMinVersion + `"
VER="${WARDYN_KATA_VERSION:-$(curl -s https://api.github.com/repos/kata-containers/kata-containers/releases/latest | grep -oP '"tag_name":\s*"\K[^"]+')}"
ver_num="${VER#v}"
if [ "$(printf '%s\n%s\n' "$KATA_MIN_VERSION" "$ver_num" | sort -V | head -n1)" != "$KATA_MIN_VERSION" ]; then
  echo "ERROR: resolved Kata version ${VER} is below the required security floor v${KATA_MIN_VERSION}" >&2
  echo "       (CVE-2026-44210/-47243: virtio-fs guest-root -> host-root via virtiofsd)." >&2
  echo "       Refusing to install. Set WARDYN_KATA_VERSION to a floor-compliant tag to override." >&2
  exit 1
fi
# Kata's release assets use the Go arch token (amd64/arm64), NOT uname -m
# (x86_64/aarch64), and are zstd-compressed (.tar.zst) as of the 3.x line.
case "$(uname -m)" in
  x86_64)  ARCH=amd64 ;;
  aarch64) ARCH=arm64 ;;
  *)       ARCH="$(uname -m)" ;;
esac
if ! command -v zstd >/dev/null 2>&1; then sudo apt-get update && sudo apt-get install -y zstd; fi
curl -fsSL -o /tmp/kata-static.tar.zst "https://github.com/kata-containers/kata-containers/releases/download/${VER}/kata-static-${VER}-${ARCH}.tar.zst"
sudo tar -C / --zstd -xf /tmp/kata-static.tar.zst
echo "-> checking host virtualization"
# Non-fatal: the check can be conservative (e.g. under a nested hypervisor) even
# when Kata runs fine as docker-root; do not abort the install/registration on it.
sudo /opt/kata/bin/kata-runtime check || echo "(kata-runtime check reported warnings — Kata often still runs under docker-as-root; proceed and test)"
# Kata is a CONTAINERD SHIM (io.containerd.kata.v2), NOT an OCI-runtime binary.
# containerd resolves the shim by name on PATH, so symlink it where containerd looks.
# (Registering the shim as a docker runtime "path" is WRONG — Docker then drives it
# as an OCI runtime and it exits 2 on the containerd -info handshake.)
echo "-> putting the kata shim on PATH for containerd"
sudo ln -sf /opt/kata/bin/containerd-shim-kata-v2 /usr/local/bin/containerd-shim-kata-v2
cat <<'NOTE'

Add this runtime to /etc/docker/daemon.json (merge with any existing "runtimes"),
then restart Docker and Re-check in the UI. wardynd detects Vault from a runtime named "kata".
Use runtimeType (the containerd-shim form), NOT path:
See https://github.com/kata-containers/kata-containers/blob/main/docs/how-to/how-to-run-docker-with-kata.md

  "runtimes": { "kata": { "runtimeType": "io.containerd.kata.v2" } }
NOTE`
}

// dockerHostHint is the load-bearing wiring: whenever a plan stands up an
// alternate dockerd, wardynd only finds it via DOCKER_HOST (client.FromEnv).
func dockerHostHint(sock string) string {
	return "\nwardynd finds Docker only via DOCKER_HOST. After the engine is up, add this to wardynd's\n" +
		"service environment (systemd unit `Environment=` or your launcher) — not just your shell:\n" +
		"    DOCKER_HOST=" + sock + "\n" +
		"Setting it only in the current shell isn't enough; wardynd would fall back to the old socket\n" +
		"and the tier would stay off."
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// runScript executes an install script via bash -c. Callers pass only the
// compile-time-constant scripts above (the only variable parts are fixed branch
// selections like the restart line), so there is no command-injection surface.
func runScript(script string) error {
	c := exec.Command("bash", "-c", script) // #nosec G204 -- trusted constant script, no user input
	c.Stdout, c.Stderr, c.Stdin = os.Stdout, os.Stderr, os.Stdin
	return c.Run()
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

func indentBlock(s string) string {
	var b strings.Builder
	for _, ln := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("    " + ln + "\n")
	}
	return b.String()
}

func readFileTrim(p string) string {
	b, err := os.ReadFile(p) // #nosec G304 -- fixed system paths (/proc, /sys, /etc/os-release)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// osReleaseField pulls a single KEY= value out of /etc/os-release.
func osReleaseField(key string) string {
	for _, ln := range strings.Split(readFileTrim("/etc/os-release"), "\n") {
		if strings.HasPrefix(ln, key+"=") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(ln, key+"=")), `"'`)
		}
	}
	return ""
}

func matchAny(hay string, subs ...string) bool {
	for _, s := range subs {
		if strings.Contains(hay, s) {
			return true
		}
	}
	return false
}
