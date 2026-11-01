// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

// MacOSResourcesScript is the Sandbox widget's reading for a macOS VM guest, which has no cgroup files
// or procfs. Like the Linux script beside its handler it runs INSIDE the sandbox and prints one
// key=value line per metric it could read, so its output is the guest's word: display only. It reads
// the whole VM, not one process tree, because the VM is the sandbox.
//
//   - memory_current is app + wired + compressed, the sum Activity Monitor calls Memory Used. It leaves
//     out file cache, which cgroup v2's memory.current counts.
//   - memory_max is the memory the VM was given, not a limit anything enforces on the first number.
//   - cpu_percent is a whole number. ps's %cpu is a decaying average, so it lags a sudden load. An
//     integer also keeps NaN and Inf from a replaced tool out of the response.
//   - disk_root_used_kb is df's Size - Available, not its Used: / is the sealed system snapshot, whose
//     Used leaves out the data volume, while every APFS volume shares the container's free space.
//
// LC_ALL=C because ps prints %cpu with the locale's decimal separator. The tools' directories are
// appended because a guest agent's PATH need not hold /usr/sbin, where sysctl lives.
const MacOSResourcesScript = `
export LC_ALL=C
PATH="$PATH:/usr/bin:/bin:/usr/sbin:/sbin"

vm_stat 2>/dev/null | awk '
/page size of/ {for(i=1;i<=NF;i++) if($i=="of") size=$(i+1)+0}
/^Anonymous pages:/ {anon=$NF+0; a=1}
/^Pages purgeable:/ {purge=$NF+0; p=1}
/^Pages wired down:/ {wired=$NF+0; w=1}
/^Pages occupied by compressor:/ {comp=$NF+0; c=1}
END {if(size>0 && a && p && w && c) printf "memory_current=%.0f\n", (anon-purge+wired+comp)*size}'

mem=$(sysctl -n hw.memsize 2>/dev/null)
[ -n "$mem" ] && echo "memory_max=$mem"

n=$(sysctl -n hw.ncpu 2>/dev/null)
ps -A -o %cpu= 2>/dev/null | awk -v n="$n" '
{s+=$1}
END {if(NR>0){print "proc_count=" NR; if(n+0>0) printf "cpu_percent=%.0f\n", s/n}}'

df -kP / 2>/dev/null | awk 'NR==2 && $2 ~ /^[0-9]+$/ && $4 ~ /^[0-9]+$/ {printf "disk_root_used_kb=%.0f\n", $2-$4}'
`
