// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k6_test

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// Every leg dumps the same thing when a case fails, so a red leg says which failure mode it was
// without a rerun (#3538): the run's pods, the namespace's recent events, the run's proxy log, and
// the org's and runnerd's lines for the run. The dump runs in a defer inside the case's t.Run, which
// ends before the case's t.Cleanup funcs: those delete the run's pods, and a dump after them reads
// nothing. Each source is cut to its last dumpLines lines and dumpBytes bytes.
const (
	dumpLines = 80
	dumpBytes = 8000
)

// runIDs holds the ids of the runs a case started, until its kase takes them.
type runIDs struct {
	mu  sync.Mutex
	ids []string
}

func (r *runIDs) add(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
}

func (r *runIDs) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := r.ids
	r.ids = nil
	return ids
}

// diagSrc is where a dump reads from. orgNS and runnerd are empty when the harness did not hand them over.
type diagSrc struct {
	admin                    kubernetes.Interface
	kc, runsNS, ownNS, orgNS string
	runnerd, runnerID        string
}

// kubectl runs kubectl under the admin kubeconfig and returns its output, whether or not it failed.
func (d diagSrc) kubectl(args ...string) string {
	out, err := exec.Command("kubectl", append([]string{"--kubeconfig", d.kc}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// tailLines keeps the last dumpLines lines of s, then its last dumpBytes bytes.
func tailLines(s string) string {
	ls := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(ls) > dumpLines {
		ls = ls[len(ls)-dumpLines:]
	}
	return tail(strings.Join(ls, "\n"), dumpBytes)
}

// grep keeps the lines of s that hold any of needles.
func grep(s string, needles ...string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		for _, n := range needles {
			if n != "" && strings.Contains(l, n) {
				keep = append(keep, l)
				break
			}
		}
	}
	return strings.Join(keep, "\n")
}

// redactedPod is the pod as YAML with every env value blanked: a name and a secretKeyRef stay, a
// literal value does not, so the dump cannot print a credential an env var carried. It is cut in the
// middle when long, since a pod's finalizers and deletion mark sit at the top and its container
// states at the bottom.
func redactedPod(p corev1.Pod) string {
	p.ManagedFields = nil
	for _, cs := range [][]corev1.Container{p.Spec.Containers, p.Spec.InitContainers} {
		for i := range cs {
			for j := range cs[i].Env {
				if cs[i].Env[j].Value != "" {
					cs[i].Env[j].Value = "<redacted>"
				}
			}
		}
	}
	b, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Sprintf("pod %s: %v", p.Name, err)
	}
	s := string(b)
	if len(s) > 2*dumpBytes {
		s = s[:dumpBytes/2] + "\n...\n" + s[len(s)-dumpBytes*3/2:]
	}
	return s
}

// dump logs the diagnostics for the runs ids, which may be empty.
func (d diagSrc) dump(t *testing.T, ids []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t.Logf("failure diagnostics for runs %v", ids)
	t.Logf("pods in %s:\n%s", d.runsNS, tailLines(d.kubectl("-n", d.runsNS, "get", "pods", "-o", "wide")))
	for _, id := range ids {
		pods, err := d.admin.CoreV1().Pods(d.runsNS).List(ctx, metav1.ListOptions{LabelSelector: labelRun + "=" + id})
		if err != nil {
			t.Logf("run %s: listing pods: %v", id, err)
		}
		for _, p := range pods.Items {
			t.Logf("pod %s of run %s:\n%s", p.Name, id, redactedPod(p))
		}
		logs := d.kubectl("-n", d.runsNS, "logs", "wardyn-proxy-"+id, "-c", "wardyn-proxy", "--timestamps", "--tail", fmt.Sprint(dumpLines))
		t.Logf("proxy log of run %s (tail):\n%s", id, tailLines(logs))
	}
	t.Logf("events in %s (recent):\n%s", d.runsNS, d.events(ctx))
	match := append([]string{d.runnerID, "orphan sweep"}, ids...)
	if d.orgNS != "" {
		org := d.kubectl("-n", d.orgNS, "logs", "deployment/wardyn-org", "--all-containers", "--timestamps")
		t.Logf("org lines for runner %s and runs %v:\n%s", d.runnerID, ids, tailLines(grep(org, match...)))
	}
	if d.runnerd != "" {
		rd := d.kubectl("-n", d.ownNS, "logs", "deployment/"+d.runnerd, "-c", "runnerd", "--timestamps")
		t.Logf("runnerd lines for runs %v:\n%s", ids, tailLines(grep(rd, append([]string{"orphan sweep"}, ids...)...)))
		t.Logf("runnerd log (last lines):\n%s", tailLines(tail(rd, 2000)))
	}
}

// events is the runs namespace's events, oldest first, one line each.
func (d diagSrc) events(ctx context.Context) string {
	evs, err := d.admin.CoreV1().Events(d.runsNS).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err.Error()
	}
	at := func(e corev1.Event) time.Time {
		switch {
		case !e.LastTimestamp.IsZero():
			return e.LastTimestamp.Time
		case !e.EventTime.IsZero():
			return e.EventTime.Time
		}
		return e.CreationTimestamp.Time
	}
	items := evs.Items
	sort.Slice(items, func(i, j int) bool { return at(items[i]).Before(at(items[j])) })
	var out []string
	for _, e := range items {
		out = append(out, fmt.Sprintf("%s %s %s %s/%s: %s", at(e).UTC().Format(time.RFC3339), e.Type, e.Reason, strings.ToLower(e.InvolvedObject.Kind), e.InvolvedObject.Name, e.Message))
	}
	return tailLines(strings.Join(out, "\n"))
}

// TestRedactedPod needs no cluster: a dump must not print an env value, whichever container carries
// it, and must keep the name of a secretKeyRef.
func TestRedactedPod(t *testing.T) {
	const secret = "s3cr3t-value"
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "wardyn-proxy-x",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "managed-fields-marker"}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Env: []corev1.EnvVar{
				{Name: "TOKEN", Value: secret},
				{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "wardyn-env-secret"}, Key: "k"}}},
			}}},
			InitContainers: []corev1.Container{{Name: "i", Env: []corev1.EnvVar{{Name: "INIT_TOKEN", Value: secret}}}},
		},
	}
	got := redactedPod(p)
	if strings.Contains(got, secret) {
		t.Errorf("an env value survived the redaction:\n%s", got)
	}
	if n := strings.Count(got, "<redacted>"); n != 2 {
		t.Errorf("want 2 redacted values (container and init container), got %d:\n%s", n, got)
	}
	if !strings.Contains(got, "wardyn-env-secret") {
		t.Errorf("the secretKeyRef name is gone:\n%s", got)
	}
	if strings.Contains(got, "managedFields") || strings.Contains(got, "managed-fields-marker") {
		t.Errorf("managedFields appear in the output:\n%s", got)
	}
}
