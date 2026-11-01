// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ASSUMPTION, for a real Mac to confirm: no captured macOS output was available when these were
// written. vmStatSample follows the labels in Apple's vm_stat source with made-up counts; dfSample
// and psCPUSample follow the documented -P and keyword= formats.
const (
	vmStatSample = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                               12345.
Pages active:                            123456.
Pages inactive:                          120000.
Pages speculative:                         2000.
Pages throttled:                              0.
Pages wired down:                         60000.
Pages purgeable:                           1000.
"Translation faults":                  98765432.
Pages copy-on-write:                    1234567.
Pages zero filled:                     45678901.
Pages reactivated:                       234567.
Pages purged:                             12345.
File-backed pages:                        80000.
Anonymous pages:                         165000.
Pages stored in compressor:               40000.
Pages occupied by compressor:             15000.
Decompressions:                          123456.
Compressions:                            234567.
Pageins:                                 345678.
Pageouts:                                  1234.
Swapins:                                      0.
Swapouts:                                     0.
`
	dfSample = `Filesystem     1024-blocks      Used Available Capacity  Mounted on
/dev/disk3s1s1   482797652  10502316 281843612     4%    /
`
	psCPUSample = "  12.3\n   0.0\n 100.0\n   3.7\n"
)

// runMacOSResourcesScript runs the script with stand-ins for the four macOS tools first on PATH. A
// tool's output of "" makes it print nothing and fail, as a missing or broken tool would.
func runMacOSResourcesScript(t *testing.T, vmStat, df, ps, memsize, ncpu string) map[string]string {
	t.Helper()
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh")
	}
	dir := t.TempDir()
	tool := func(name, output string) {
		t.Helper()
		body := "#!/bin/sh\nexit 1\n"
		if output != "" {
			data := filepath.Join(dir, name+".out")
			if err := os.WriteFile(data, []byte(output), 0o600); err != nil {
				t.Fatal(err)
			}
			body = "#!/bin/sh\ncat '" + data + "'\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil { //nolint:gosec // a test stand-in must be executable
			t.Fatal(err)
		}
	}
	tool("vm_stat", vmStat)
	tool("df", df)
	tool("ps", ps)
	sysctl := "#!/bin/sh\ncase \"$2\" in\nhw.memsize) v='" + memsize + "';;\nhw.ncpu) v='" + ncpu + "';;\nesac\n[ -n \"$v\" ] || exit 1\necho \"$v\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sysctl"), []byte(sysctl), 0o700); err != nil { //nolint:gosec // a test stand-in must be executable
		t.Fatal(err)
	}

	cmd := exec.Command("/bin/sh", "-c", MacOSResourcesScript, "wardyn-resources")
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	kv := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[k] = v
		}
	}
	return kv
}

func TestMacOSResourcesScript_ReadsEveryMetric(t *testing.T) {
	kv := runMacOSResourcesScript(t, vmStatSample, dfSample, psCPUSample, "8589934592", "4")
	want := map[string]string{
		// (anonymous 165000 - purgeable 1000 + wired 60000 + compressor 15000) * 16384
		"memory_current": "3915776000",
		"memory_max":     "8589934592",
		// (12.3 + 0 + 100 + 3.7) / 4 CPUs
		"cpu_percent": "29",
		"proc_count":  "4",
		// Size - Available, not Used: / is the sealed system snapshot, whose Used leaves out the data volume.
		"disk_root_used_kb": "200954040",
	}
	for k, v := range want {
		if kv[k] != v {
			t.Errorf("%s = %q, want %q", k, kv[k], v)
		}
	}
	if len(kv) != len(want) {
		t.Errorf("keys = %v, want exactly %v", kv, want)
	}
}

// A page size read from the header, never assumed: an Intel guest's pages are 4 KiB.
func TestMacOSResourcesScript_UsesTheReportedPageSize(t *testing.T) {
	sample := strings.Replace(vmStatSample, "page size of 16384", "page size of 4096", 1)
	kv := runMacOSResourcesScript(t, sample, dfSample, psCPUSample, "8589934592", "4")
	if got, want := kv["memory_current"], "978944000"; got != want {
		t.Errorf("memory_current = %q, want %q", got, want)
	}
}

// Whatever a tool fails to give leaves its key absent, never a zero, and costs no other key.
func TestMacOSResourcesScript_MissingInputLeavesTheKeyAbsent(t *testing.T) {
	cases := []struct {
		name                         string
		vmStat, df, ps, memsize, cpu string
		absent                       []string
	}{
		{"every tool failed", "", "", "", "", "", []string{"memory_current", "memory_max", "cpu_percent", "proc_count", "disk_root_used_kb"}},
		{"vm_stat failed", "", dfSample, psCPUSample, "8589934592", "4", []string{"memory_current"}},
		{"no wired line", strings.Replace(vmStatSample, "Pages wired down:", "Pages wired:", 1), dfSample, psCPUSample, "8589934592", "4", []string{"memory_current"}},
		{"no compressor line", strings.Replace(vmStatSample, "Pages occupied by compressor:", "Occupied:", 1), dfSample, psCPUSample, "8589934592", "4", []string{"memory_current"}},
		{"no page size", strings.Replace(vmStatSample, "(page size of 16384 bytes)", "", 1), dfSample, psCPUSample, "8589934592", "4", []string{"memory_current"}},
		{"df's columns are not numbers", vmStatSample, "Filesystem Size\n/dev/disk3s1s1 a b c\n", psCPUSample, "8589934592", "4", []string{"disk_root_used_kb"}},
		{"no CPU count", vmStatSample, dfSample, psCPUSample, "8589934592", "", []string{"cpu_percent"}},
		{"ps failed", vmStatSample, dfSample, "", "8589934592", "4", []string{"cpu_percent", "proc_count"}},
		{"no memsize", vmStatSample, dfSample, psCPUSample, "", "4", []string{"memory_max"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kv := runMacOSResourcesScript(t, tc.vmStat, tc.df, tc.ps, tc.memsize, tc.cpu)
			for _, k := range tc.absent {
				if v, ok := kv[k]; ok {
					t.Errorf("%s = %q, want absent", k, v)
				}
			}
			if want := 5 - len(tc.absent); len(kv) != want {
				t.Errorf("keys = %v, want %d of them", kv, want)
			}
		})
	}
}
