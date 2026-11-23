// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The censuses below find every site that decides something by machine kind, keyed by file and enclosing
// function (not line, so they survive edits), and fail on any site they do not classify or any count that
// differs. A change that adds a site classifies it here in the same commit: a Windows VM is a VM with no
// guest agent, so a new site must say what a windows_vm run gets there.

// censusSite is one classified key: how many sites the function holds, and why a Windows VM run is right there.
type censusSite struct {
	n     int
	class string
	why   string
}

// Classes of the .IsVM() census.
const (
	isVMAnyVM = "any VM"
	isVMGuest = "guest-agent feature, unreachable for windows_vm because the create refuses it first"
)

// Classes of the kind-test and Docker lookup censuses.
const (
	kindNonMac      = "N"          // a Windows run takes the not-macOS (or not-Linux) answer on purpose
	kindUnreachable = "U"          // unreachable for windows_vm: refused first
	kindEdited      = "E"          // decided by an edit for windows_vm
	kindMacOSOnly   = "macOS-only" // a macOS-only file, counted per file
	macOSOnlyWhy    = "macOS-only file: each site answers for macos_vm, and a Windows run takes the not-macOS answer"
)

// isVMCensus is every non-test .IsVM() call in internal/api and internal/runner.
var isVMCensus = map[string]censusSite{
	"api/approvals_takeover.go:takeoverSurface":           {1, isVMAnyVM, "a VM run that is not background-only takes over at its desktop"},
	"api/internal.go:handlePostDecision":                  {1, isVMAnyVM, "the relay mark counts for any VM guest's flow; the relay is guest-OS agnostic"},
	"api/provider_env.go:applyClaudeToolSearchDefault":    {1, isVMGuest, "an agent's environment; a Windows run starts no agent"},
	"api/run_agent_cut.go:cutAgent":                       {1, isVMAnyVM, "no agent cut on a VM"},
	"api/run_agent_cut.go:keepable":                       {1, isVMAnyVM, "a VM is never kept with its agent cut off"},
	"api/run_agent_cut.go:copyBackCut":                    {1, isVMGuest, "a computer folder's copy back; a Windows run has no computer folder"},
	"api/run_computer_folder.go:quiesceKeptSandbox":       {2, isVMGuest, "a kept computer folder; a Windows run has none"},
	"api/run_contain_d1.go:d1ServerMode":                  {1, isVMAnyVM, "false: a VM is never a hosted Kubernetes run"},
	"api/run_contain_dispatch.go:containDispatch":         {1, isVMAnyVM, "a VM is not cut from the server"},
	"api/run_desktop_signin.go:handleBeginDesktopSignin":  {1, isVMGuest, "desktop sign-ins need a guest agent; a VM is refused here"},
	"api/run_desktop_vm_signin.go:recordDesktopVMSignIns": {1, isVMGuest, "sign-ins are refused at create, and a Windows sandbox reports none"},
	"api/run_desktop_vm_signin.go:watchVMSignIn":          {1, isVMAnyVM, "a VM never takes a container desktop sign-in"},
	"api/run_egress_edit.go:egressEditRunnerToUpdate":     {1, isVMAnyVM, "no runner update is named for a VM"},
	"api/run_folder_pass_once.go:passGuardFor":            {1, isVMGuest, "a local folder pass; folders are refused at create"},
	"api/run_folder_pass_once.go:holdUnquiesced":          {1, isVMGuest, "a local folder pass; folders are refused at create"},
	"api/run_local_folder.go:holdForLocalFolder":          {1, isVMGuest, "a local folder sync; folders are refused at create"},
	"api/run_lost_at_gen.go:generationStopperFor":         {1, isVMAnyVM, "no proxy generation stop on a VM"},
	"api/run_lost_at_gen.go:runnerNeedsUpdateToRevive":    {1, isVMAnyVM, "no runner update is asked of a VM"},
	"api/run_proxy_readback.go:folderKeepVerdict":         {1, isVMAnyVM, "a VM keeps no computer folder"},
	"api/run_service.go:serviceRefusal":                   {1, isVMAnyVM, "a service runs in a container, never a VM"},
	"api/runs_dispatch_mounts.go:recordVMMachine":         {1, isVMAnyVM, "a VM's machine is recorded; a Windows VM's is QEMU's"},
	"api/uigateway_desktop.go:uiEnterDesktop":             {1, isVMAnyVM, "a VM's desktop is the one door a Windows run has"},
	"api/via_org.go:handleViaOrg":                         {1, isVMAnyVM, "the relay mark counts for any VM guest's flow"},
	"runner/docker/driver_network.go:runNetworkAndProxy":  {1, isVMAnyVM, "a VM's proxy serves the guest's relay"},
	"runner/remote/substrate.go:CreateSandbox":            {2, isVMAnyVM, "a VM spec goes through the runner's VM caps check and is remembered as a VM"},
	"runner/runneragent/dispatch.go:create":               {1, isVMAnyVM, "create_vm starts any VM kind"},
}

// kindTestCensus is every non-test kind test in internal/api, internal/store, internal/runner and
// cmd/wardyn-runnerd: a call X.IsMacOSVM(), X.RunsMemberImage() or X.HasDiskOverlay(), or a selector
// MachineLinuxVM or MachineMacOSVM. The macOS-only files are keyed by file alone.
var kindTestCensus = map[string]censusSite{
	"api/desktop_files.go:desktopRun":                                     {1, kindUnreachable, "the desktop door refuses a Windows run first"},
	"api/run_agent_cast.go:recordMacOSAgent":                              {1, kindNonMac, "the macOS agent-output cast; a Windows run has no agent and records nothing"},
	"api/run_agent_cast.go:agentOutputRecorded":                           {1, kindNonMac, "the macOS agent-output cast; nothing is owed for a Windows run"},
	"api/run_agent_cast.go:unoweAgentCast":                                {1, kindNonMac, "the macOS agent-output cast; nothing is owed for a Windows run"},
	"api/run_attachments.go:runAttachments":                               {1, kindNonMac, "folders are refused before the workspaces step, so the mount map is empty"},
	"api/run_background_watch.go:backgroundWatchRefusal":                  {1, kindUnreachable, "a run that is not background-only is refused first"},
	"api/run_background_watch.go:backgroundWatchViewFor":                  {1, kindNonMac, "nil: a Windows run shows no background watch"},
	"api/run_computer_folder_bring_back.go:handleBringComputerFolderBack": {1, kindNonMac, "the container end-hold budget; a Windows run has no computer folder"},
	"api/run_contain_d1.go:d1Stop":                                        {1, kindNonMac, "the linux_vm budget: an ACPI power-down, then the container stop"},
	"api/run_desktop_vm_signin.go:macOSDesktopSignInsOffRead":             {1, kindNonMac, "false: there is no macOS sign-in roster"},
	"api/run_drives.go:authorizeRequestDrives":                            {1, kindUnreachable, "drives are refused before the drive step"},
	"api/run_files.go:runWorkDir":                                         {1, kindUnreachable, "the file listing is refused before any exec"},
	"api/run_fold_steps.go:stepDrive":                                     {1, kindUnreachable, "keep_work_folder is refused before the drive step"},
	"api/run_fold_steps.go:stepPlacementCapability":                       {2, kindNonMac, "no macOS ask; the Windows caps and recording checks are the Windows placement step's"},
	"api/run_fold_steps.go:stepDriveEligibility":                          {1, kindUnreachable, "no drives: refused at create"},
	"api/run_fold_steps.go:stepMainDrive":                                 {2, kindEdited, "a Windows disk is a qcow2 overlay: HasDiskOverlay in place"},
	"api/run_image_admission.go:imageAdmission":                           {1, kindEdited, "a Windows image is the registered ref: RunsMemberImage in place"},
	"api/run_image_admission.go:imageAdmissionPreview":                    {1, kindEdited, "a Windows image is the registered ref: RunsMemberImage in place"},
	"api/run_image_pin.go:conventionImageAdmission":                       {1, kindEdited, "a Windows image is the registered ref: RunsMemberImage in place"},
	"api/run_keep_work_folder.go:auditKeptDrive":                          {1, kindUnreachable, "keep_work_folder is refused at create"},
	"api/run_keep_work_folder.go:dropUncreatedMacKeptDrive":               {1, kindNonMac, "returns at once: a Windows run keeps no drive"},
	"api/run_kept_machine.go:keptMachineDecision":                         {1, kindUnreachable, "kept machines are refused at create"},
	"api/run_kept_machine.go:keptResumeOf":                                {1, kindUnreachable, "machine.resume is refused at create"},
	"api/run_lease.go:stopKeptRun":                                        {1, kindNonMac, "the sweep budget, as linux_vm; a lost Windows run is stopped here"},
	"api/run_placement_capabilities.go:requestDemands":                    {2, kindNonMac, "windows reads as no VM demand; the Windows placement step owns the caps check"},
	"api/run_placement_dispatch.go:createSandboxAtTarget":                 {1, kindNonMac, "a Windows run has no image arches, so the branch is not entered"},
	"api/run_placement_resolve.go:runnerRecordingDemand":                  {1, kindNonMac, "linux_vm's answer; a Windows run under a recording requirement is refused"},
	"api/run_placement_resolve.go:kindTargets":                            {1, kindNonMac, "keeps the registered-runner kind: an organisation runner is the only place a Windows run goes"},
	"api/run_recording.go:applyRunRecording":                              {1, kindNonMac, "a Windows run's recording level is off, so the runner records nothing"},
	"api/run_recording.go:refuseDeclinedUpload":                           {1, kindNonMac, "a Windows run's level is off, so an upload is refused as for any unrecorded run"},
	"api/run_resources.go:handleRunResources":                             {1, kindUnreachable, "resource usage is refused before any exec"},
	"api/run_revive_image.go:reviveImageAdmission":                        {1, kindUnreachable, "revive is refused first"},
	"api/run_terminal_effects.go:macOSVMRun":                              {1, kindNonMac, "false: the container budgets, as linux_vm"},
	"api/runs_create_validate.go:machineKindRefusal":                      {3, kindEdited, "windows_vm is refused before the macOS switch is read"},
	"api/runs_dispatch.go:dispatchRun":                                    {1, kindNonMac, "no unstarted-end marker: it serves a macOS kept drive"},
	"api/runs_dispatch_mounts.go:auditDriveMount":                         {1, kindUnreachable, "no drives, so the loop body never runs"},
	"api/runs_dispatch_mounts.go:recordVMMachine":                         {1, kindNonMac, "a Windows run is never background-only"},
	"api/runs_dispatch_start.go:startAgentsOrIdle":                        {1, kindNonMac, "interactive with no macOS prep: the run waits for its desktop"},
	"api/runs_dispatch_start.go:macPrepInterrupted":                       {1, kindNonMac, "false: no prep"},
	"api/sandbox_sweep.go:sweepTerminalSandboxRun":                        {1, kindNonMac, "the container stop bound, as linux_vm"},
	"api/sshgateway_channels.go:sshRelayArgv":                             {2, kindUnreachable, "SSH is refused at authentication"},
	"api/sshgateway_channels.go:handleSSHDirectTCPIP":                     {1, kindUnreachable, "SSH is refused at authentication"},
	"api/sshgateway_sftp.go:sftpServerPathFor":                            {1, kindUnreachable, "SSH is refused at authentication"},
	"api/uigateway.go:uiDial":                                             {1, kindUnreachable, "a Windows run declares no app, so a non-desktop app is refused first"},
	"api/uigateway.go:uiLauncherScriptFor":                                {2, kindUnreachable, "a Windows run declares no app, so a non-desktop app is refused first"},
	"api/uigateway_desktop.go:serveDesktopRFB":                            {1, kindNonMac, "no macOS window tracking; one stream per Windows VM is the runner's rule"},
	"runner/docker/driver_network.go:CreateSandbox":                       {1, kindNonMac, "not macOS: goes to createVM, which branches to the Windows create"},
	"runner/docker/driver_proxy_revive.go:ReplaceProxyHashedAt":           {1, kindUnreachable, "the VM lookup answers unsupported first; revive is refused in the API too"},
	"runner/docker/macvm.go:CoversKind":                                   {2, kindEdited, "a windows_vm case while the Windows config is on"},
	"runner/docker/vm.go:isLinuxVM":                                       {1, kindEdited, "matches the Windows label too"},
	"runner/docker/vm.go:refuseVMSpec":                                    {1, kindUnreachable, "createVM branches to the Windows create first"},
	"runner/docker/vm.go:createVM":                                        {1, kindUnreachable, "createVM branches to the Windows create first"},
	"runner/k8s/linux_vm.go:refuseVMSpec":                                 {2, kindNonMac, "Kubernetes refuses windows_vm as unknown on purpose; the API refuses first with a named reason"},
	"runner/k8s/linux_vm.go:podSandbox":                                   {1, kindUnreachable, "refuseVMSpec refused first"},
	"runner/offline/observation.go:coversKind":                            {1, kindNonMac, "a substrate without a kind coverer covers no Windows VM; Docker has one"},
	"runner/remote/streams.go:byKind":                                     {2, kindEdited, "a windows case"},
	"runner/remote/substrate.go:checkVMCaps":                              {1, kindEdited, "a windows case"},
	"runner/runneragent/dispatch.go:create":                               {2, kindEdited, "a refusal case"},
	"store/store_watcher.go:ClaimStaleRunWatchers":                        {1, kindNonMac, "the macOS prep clause; a Windows run never carries that status"},

	"api/run_mac_window.go":         {3, kindMacOSOnly, macOSOnlyWhy},
	"api/run_mac_window_launch.go":  {2, kindMacOSOnly, macOSOnlyWhy},
	"api/run_macos_base.go":         {4, kindMacOSOnly, macOSOnlyWhy},
	"api/run_macos_disk.go":         {1, kindMacOSOnly, macOSOnlyWhy},
	"api/run_macos_egress.go":       {2, kindMacOSOnly, macOSOnlyWhy},
	"api/run_macos_hold.go":         {1, kindMacOSOnly, macOSOnlyWhy},
	"api/run_macos_view.go":         {3, kindMacOSOnly, macOSOnlyWhy},
	"runner/docker/macvm_prep.go":   {1, kindMacOSOnly, macOSOnlyWhy},
	"store/store_macos_vm_disks.go": {1, kindMacOSOnly, macOSOnlyWhy},
}

// dockerVMLookupCensus is every vmLookup( and vmFor( call in internal/runner/docker: what a Windows VM's vmRun,
// whose agent refuses every call with one named error, answers there.
var dockerVMLookupCensus = map[string]censusSite{
	"runner/docker/aws_cache_wipe.go:wipeAWSCaches":             {1, kindUnreachable, "the wipe returns first: a Windows VM stages no AWS profile"},
	"runner/docker/driver_exec.go:exec":                         {1, kindUnreachable, "the agent's named refusal; an interactive run gets no agent exec"},
	"runner/docker/driver_exec.go:Wait":                         {1, kindUnreachable, "a run on a registered runner starts no completion watcher"},
	"runner/docker/driver_exec.go:Status":                       {1, kindNonMac, "inspects the QEMU container; an exited Windows VM reads stopped with no exit code"},
	"runner/docker/driver_exec.go:AgentStatus":                  {1, kindNonMac, "the container status for an empty exec id; the agent's named refusal otherwise"},
	"runner/docker/driver_fence.go:FenceProxy":                  {1, kindNonMac, "unsupported, as for any VM"},
	"runner/docker/driver_fence.go:EndSandboxAt":                {1, kindNonMac, "an ACPI power-down, then the container stop and the sidecars"},
	"runner/docker/driver_network.go:StopSandbox":               {1, kindNonMac, "an ACPI power-down, then the container stop"},
	"runner/docker/driver_network.go:EndSandbox":                {1, kindNonMac, "as EndSandboxAt"},
	"runner/docker/driver_proxy_revive.go:CanReplaceProxy":      {1, kindNonMac, "unsupported, as for any VM"},
	"runner/docker/driver_proxy_revive.go:ReplaceProxyHashedAt": {1, kindNonMac, "unsupported, as for any VM"},
	"runner/docker/driver_proxy_revive.go:StartSandbox":         {1, kindNonMac, "unsupported, as for any VM"},
	"runner/docker/macvm_window.go:OpenWindow":                  {1, kindNonMac, "the macOS window is unsupported on a VM that is not a Mac"},
	"runner/docker/output_recover.go:RecoverOutput":             {1, kindNonMac, "output is unrecoverable, as for any VM"},
	"runner/docker/session.go:Attach":                           {1, kindUnreachable, "the agent's named refusal; the terminal door is refused"},
	"runner/docker/session.go:execStream":                       {1, kindUnreachable, "the agent's named refusal; SSH, the UI relay, resources and files are refused"},
	"runner/docker/session_signin.go:ExecStreamAs":              {1, kindNonMac, "sign-ins are unsupported, as for any VM"},
	"runner/docker/session_signin.go:SignalMain":                {1, kindNonMac, "sign-ins are unsupported, as for any VM"},
	"runner/docker/vm.go:vmFor":                                 {1, kindNonMac, "the lookup itself"},
	"runner/docker/vm.go:EndVM":                                 {1, kindNonMac, "EndSandbox, as for any VM"},
	"runner/docker/vm_desktop.go:DialDesktop":                   {1, kindEdited, "a Windows VM's desktop keeps one stream, the newest"},
	"runner/docker/vm_desktop.go:PauseAgent":                    {1, kindUnreachable, "the agent's named refusal; desktop pause is refused"},
	"runner/docker/vm_desktop.go:ResumeAgent":                   {1, kindUnreachable, "the agent's named refusal; desktop resume is refused"},
}

// macOSOnlyFiles are the files the kind-test census counts per file. They are named one by one: a new file whose
// name looks like theirs is counted by function.
var macOSOnlyFiles = map[string]bool{}

func init() {
	for k, v := range kindTestCensus {
		if v.class == kindMacOSOnly {
			macOSOnlyFiles[k] = true
		}
	}
}

// censusScan parses every non-test .go file under each root (relative to internal/types, walked recursively,
// testdata skipped) and counts the nodes match accepts, keyed "<path from internal/>:<enclosing function>", or by
// the path alone for a file perFile names.
func censusScan(t *testing.T, roots []string, perFile map[string]bool, match func(ast.Node) bool) map[string]int {
	t.Helper()
	got := map[string]int{}
	fset := token.NewFileSet()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel := censusKeyPath(path)
			for _, decl := range f.Decls {
				fn := "(package)"
				if fd, ok := decl.(*ast.FuncDecl); ok {
					fn = fd.Name.Name
				}
				key := rel + ":" + fn
				if perFile[rel] {
					key = rel
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					if n != nil && match(n) {
						got[key]++
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
	}
	return got
}

// censusKeyPath is path from internal/ ("api/x.go"), or from the repository root for cmd/.
func censusKeyPath(path string) string {
	p := filepath.ToSlash(path)
	for _, pre := range []string{"../../", "../"} {
		if strings.HasPrefix(p, pre) {
			p = strings.TrimPrefix(p, pre)
			break
		}
	}
	return p
}

// censusCompare fails on a key the census does not list, a listed key that is gone, or a count that differs.
func censusCompare(t *testing.T, name string, got map[string]int, want map[string]censusSite, total int) {
	t.Helper()
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	for k := range want {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	sum := 0
	for _, k := range sorted {
		w, listed := want[k]
		g := got[k]
		sum += g
		switch {
		case !listed:
			t.Errorf("%s: %s holds %d site(s) the census does not classify: say what a windows_vm run gets there and add the row", name, k, g)
		case w.class == "" || w.why == "":
			t.Errorf("%s: %s has no class or reason", name, k)
		case g != w.n:
			t.Errorf("%s: %s holds %d site(s), the census lists %d: reclassify the site and set the count", name, k, g, w.n)
		}
	}
	if sum != total {
		t.Errorf("%s: %d sites in all, want %d", name, sum, total)
	}
}

// selectorCall reports n as a call X.<name>() with no arguments, for any X.
func selectorCall(n ast.Node, names ...string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	for _, name := range names {
		if sel.Sel.Name == name {
			return true
		}
	}
	return false
}

// Every .IsVM() site is classified: a Windows VM is a VM, so it takes each one's VM answer, or the site serves
// a guest-agent feature that a Windows run is refused before it reaches.
func TestIsVMCensus(t *testing.T) {
	got := censusScan(t, []string{"../api", "../runner"}, nil, func(n ast.Node) bool { return selectorCall(n, "IsVM") })
	censusCompare(t, "IsVM", got, isVMCensus, 28)
}

// Every kind test outside the .IsVM() census is classified for windows_vm.
func TestMachineKindTestCensus(t *testing.T) {
	roots := []string{"../api", "../store", "../runner", "../../cmd/wardyn-runnerd"}
	got := censusScan(t, roots, macOSOnlyFiles, func(n ast.Node) bool {
		if selectorCall(n, "IsMacOSVM", "RunsMemberImage", "HasDiskOverlay") {
			return true
		}
		sel, ok := n.(*ast.SelectorExpr)
		return ok && (sel.Sel.Name == "MachineLinuxVM" || sel.Sel.Name == "MachineMacOSVM")
	})
	censusCompare(t, "kind test", got, kindTestCensus, 88)
}

// Every Docker driver VM lookup is classified for the Windows VM's vmRun.
func TestDockerVMLookupCensus(t *testing.T) {
	got := censusScan(t, []string{"../runner/docker"}, nil, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		return ok && (sel.Sel.Name == "vmLookup" || sel.Sel.Name == "vmFor")
	})
	censusCompare(t, "VM lookup", got, dockerVMLookupCensus, 23)
}
