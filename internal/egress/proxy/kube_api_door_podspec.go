// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The kubectl door's pod-spec check. A run whose kubectl may write can create a
// pod and read what it prints through pods/<name>/log, so a pod that mounts a
// service account token, a Secret or the node's files, or reads a Secret into a
// variable, would hand that credential to the sandbox. A create or replace of a
// resource that carries a pod spec must set automountServiceAccountToken: false,
// and may use only the plain volume kinds, environment sources and container
// capabilities listed below, so a kind the door does not know is refused. It
// may use no host access: a host namespace, a privileged or Windows
// host-process container, an unmasked /proc, or a Windows credential spec. A
// patch is merged into an object the door never sees, so one that touches the
// pod spec at all is refused, and so is any JSON Patch.
//
// The API server reads keys case-sensitively and ignores one spelled otherwise,
// so the door reads each key in its exact spelling and refuses an object that
// also holds it in another case: a key the server ignores never satisfies a rule.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ponytail: the door has no body cap of its own; 1 MiB holds any pod or
// workload kubectl sends (the API server's own limit is 3 MiB).
const kubePodSpecMaxBody = 1 << 20

var (
	errKubePodToken  = errors.New("this run's kubectl cannot create a pod that mounts a service account token: set automountServiceAccountToken: false")
	errKubePodEnv    = errors.New("this run's kubectl cannot create a pod that reads a Secret into its environment: a variable may come only from fieldRef, resourceFieldRef, configMapKeyRef or fileKeyRef, and envFrom only from configMapRef")
	errKubePodVolume = errors.New("this run's kubectl may mount only emptyDir, configMap, downwardAPI, persistentVolumeClaim, ephemeral, image and projected volumes in a pod, and project only configMap, downwardAPI and clusterTrustBundle")
	errKubePodHost   = errors.New("this run's kubectl cannot create a pod with host access: a host namespace, a privileged or Windows host-process container, a capability outside the Pod Security baseline set, or an unmasked /proc")
	errKubePodGMSA   = errors.New("this run's kubectl cannot create a pod that names a Windows credential spec: windowsOptions may set only hostProcess and runAsUserName")
	errKubePodCase   = errors.New("this run's kubectl sent a pod or workload with a key Wardyn reads spelled in another case")
	errKubePodShape  = errors.New("this run's kubectl sent a pod or workload Wardyn cannot read")
)

// kubePodSpecPaths maps each resource that carries a pod spec, in any API
// group, to the keys that lead from the object to its pod spec.
var kubePodSpecPaths = map[string][]string{
	"pods":                   {"spec"},
	"deployments":            {"spec", "template", "spec"},
	"replicasets":            {"spec", "template", "spec"},
	"statefulsets":           {"spec", "template", "spec"},
	"daemonsets":             {"spec", "template", "spec"},
	"jobs":                   {"spec", "template", "spec"},
	"replicationcontrollers": {"spec", "template", "spec"},
	"cronjobs":               {"spec", "jobTemplate", "spec", "template", "spec"},
	"podtemplates":           {"template", "spec"},
}

// kubePlainVolumes are the keys a volume may carry: its name and the kinds that
// hold no Secret, service account token or node path. kubePlainProjections are
// the sources a projected volume may carry.
var (
	kubePlainVolumes     = []string{"name", "emptyDir", "configMap", "downwardAPI", "persistentVolumeClaim", "ephemeral", "image", "projected"}
	kubePlainProjections = []string{"configMap", "downwardAPI", "clusterTrustBundle"}
)

// kubePlainValueFrom are the sources a container's variable may read, and
// kubePlainEnvFrom the keys an envFrom item may carry: none reads a Secret.
var (
	kubePlainValueFrom = []string{"fieldRef", "resourceFieldRef", "configMapKeyRef", "fileKeyRef"}
	kubePlainEnvFrom   = []string{"prefix", "configMapRef"}
)

// kubeBaselineCapabilities are the capabilities a container may add: the Pod
// Security baseline set, in its exact spelling. Any other name, ALL among them,
// is host access.
var kubeBaselineCapabilities = []string{"AUDIT_WRITE", "CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "MKNOD",
	"NET_BIND_SERVICE", "SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT"}

// admitPodSpec judges the body of a create, replace or patch of a resource
// that carries a pod spec and leaves r.Body readable again with the bytes it
// judged. Subresources (status, scale, binding) cannot change the pod spec.
// raw is the raw request path, already admitted.
func (d *kubeAPIDoor) admitPodSpec(r *http.Request, raw string) error {
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
		return nil
	}
	_, rest, ok := kubeNamespaced(strings.Split(raw[1:], "/"))
	if !ok || len(rest) < 1 || len(rest) > 2 {
		return nil
	}
	path, ok := kubePodSpecPaths[rest[0]]
	if !ok {
		return nil
	}
	if err := kubePodSpecMediaType(r); err != nil {
		return err
	}
	body, err := readKubePodSpecBody(r)
	if err != nil {
		return err
	}
	obj, err := kubeObject(body)
	if err != nil {
		return err
	}
	if r.Method == http.MethodPatch {
		return judgeKubePodSpecPatch(obj, path)
	}
	spec, err := kubeWalk(obj, path)
	if err != nil {
		return err
	}
	return judgeKubePodSpec(spec)
}

// kubePodSpecMediaType admits an unencoded JSON body: a create or replace as
// application/json, a patch as a merge or strategic merge patch.
func kubePodSpecMediaType(r *http.Request) error {
	for _, enc := range r.Header.Values("Content-Encoding") {
		if v := strings.ToLower(strings.TrimSpace(enc)); v != "" && v != "identity" {
			return errors.New("this run's kubectl cannot send an encoded pod or workload")
		}
	}
	notJSON := errors.New("this run's kubectl must send a pod or workload as JSON")
	ct := r.Header.Values("Content-Type")
	if len(ct) != 1 {
		return notJSON
	}
	mt, _, err := mime.ParseMediaType(ct[0])
	switch {
	case err != nil:
		return notJSON
	case r.Method != http.MethodPatch:
		if mt != "application/json" {
			return notJSON
		}
	case mt == "application/json-patch+json":
		return errors.New("this run's kubectl cannot send a JSON Patch to a pod or workload")
	case mt != "application/merge-patch+json" && mt != "application/strategic-merge-patch+json":
		return notJSON
	}
	return nil
}

// readKubePodSpecBody reads the body up to the cap and puts it back.
func readKubePodSpecBody(r *http.Request) ([]byte, error) {
	tooLarge := errors.New("this run's kubectl cannot send a pod or workload over 1 MiB")
	if r.ContentLength > kubePodSpecMaxBody {
		return nil, tooLarge
	}
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		if body, err = io.ReadAll(io.LimitReader(r.Body, kubePodSpecMaxBody+1)); err != nil {
			return nil, errKubePodShape
		}
	}
	if len(body) > kubePodSpecMaxBody {
		return nil, tooLarge
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength, r.TransferEncoding = int64(len(body)), nil
	return body, nil
}

// kubeObject parses body as one JSON object. A key repeated at any depth is
// refused: two parsers could keep different copies.
func kubeObject(body []byte) (map[string]any, error) {
	if _, err := types.CanonicalJSON(body); err != nil {
		return nil, errKubePodShape
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return nil, errKubePodShape
	}
	return obj, nil
}

// kubeKey reads key from obj in its exact spelling, and refuses obj when it
// also holds key spelled in another case.
func kubeKey(obj map[string]any, key string) (any, error) {
	for k := range obj {
		if k != key && strings.EqualFold(k, key) {
			return nil, errKubePodCase
		}
	}
	return obj[key], nil
}

// kubeHas reports whether obj holds key, null included, as kubeKey reads it.
func kubeHas(obj map[string]any, key string) (bool, error) {
	if _, err := kubeKey(obj, key); err != nil {
		return false, err
	}
	_, has := obj[key]
	return has, nil
}

// kubeMap reads key as an object; absent or null is nil.
func kubeMap(obj map[string]any, key string) (map[string]any, error) {
	v, err := kubeKey(obj, key)
	if err != nil || v == nil {
		return nil, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errKubePodShape
	}
	return m, nil
}

// kubeList reads key as a list of objects; absent or null is none.
func kubeList(obj map[string]any, key string) ([]map[string]any, error) {
	v, err := kubeKey(obj, key)
	if err != nil || v == nil {
		return nil, err
	}
	list, ok := v.([]any)
	if !ok {
		return nil, errKubePodShape
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, errKubePodShape
		}
		out = append(out, m)
	}
	return out, nil
}

// kubeTrue reads key as a boolean; absent or null is false.
func kubeTrue(obj map[string]any, key string) (bool, error) {
	v, err := kubeKey(obj, key)
	if err != nil || v == nil {
		return false, err
	}
	b, ok := v.(bool)
	if !ok {
		return false, errKubePodShape
	}
	return b, nil
}

// kubeWalk follows path from obj to the pod spec; nil when the body has none.
func kubeWalk(obj map[string]any, path []string) (map[string]any, error) {
	for _, k := range path {
		next, err := kubeMap(obj, k)
		if err != nil || next == nil {
			return nil, err
		}
		obj = next
	}
	return obj, nil
}

// judgeKubePodSpecPatch admits a merge patch only when it leaves the pod spec
// alone: every object on the way to it holds no strategic merge directive, and
// the patch stops short of it without naming the next step.
func judgeKubePodSpecPatch(obj map[string]any, path []string) error {
	for _, k := range path {
		for key := range obj {
			if strings.HasPrefix(key, "$") {
				return errors.New("this run's kubectl cannot send a strategic merge directive to a pod or workload")
			}
		}
		if has, err := kubeHas(obj, k); err != nil || !has {
			return err
		}
		next, ok := obj[k].(map[string]any)
		if !ok {
			return errKubePodShape
		}
		obj = next
	}
	return errors.New("this run's kubectl cannot patch the pod spec of a pod or workload, because Wardyn cannot see what it already mounts: replace the whole object instead")
}

// judgeKubePodSpec refuses a pod spec that does not set
// automountServiceAccountToken: false, that shares a host namespace, whose
// Windows options are not plain, that mounts a volume kind outside the plain
// ones, or whose containers fail judgeKubeContainer.
func judgeKubePodSpec(spec map[string]any) error {
	if v, err := kubeKey(spec, "automountServiceAccountToken"); err != nil {
		return err
	} else if off, ok := v.(bool); !ok || off {
		return errKubePodToken
	}
	for _, key := range []string{"hostPID", "hostIPC", "hostNetwork"} {
		on, err := kubeTrue(spec, key)
		if err != nil {
			return err
		}
		if on {
			return errKubePodHost
		}
	}
	psc, err := kubeMap(spec, "securityContext")
	if err != nil {
		return err
	}
	if err := judgeKubeWindows(psc); err != nil {
		return err
	}
	vols, err := kubeList(spec, "volumes")
	if err != nil {
		return err
	}
	for _, v := range vols {
		if err := judgeKubeVolume(v); err != nil {
			return err
		}
	}
	for _, key := range []string{"containers", "initContainers", "ephemeralContainers"} {
		cs, err := kubeList(spec, key)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if err := judgeKubeContainer(c); err != nil {
				return err
			}
		}
	}
	return nil
}

// judgeKubeVolume admits a volume that carries only a name and plain kinds,
// and a projected one only plain sources.
func judgeKubeVolume(v map[string]any) error {
	if !kubeOnlyKeys(v, kubePlainVolumes) {
		return errKubePodVolume
	}
	proj, err := kubeMap(v, "projected")
	if err != nil {
		return err
	}
	srcs, err := kubeList(proj, "sources")
	if err != nil {
		return err
	}
	for _, s := range srcs {
		if !kubeOnlyKeys(s, kubePlainProjections) {
			return errKubePodVolume
		}
	}
	return nil
}

// kubeOnlyKeys reports whether every key of obj, null or not, is one of
// allowed in its exact spelling.
func kubeOnlyKeys(obj map[string]any, allowed []string) bool {
	for k := range obj {
		if !slices.Contains(allowed, k) {
			return false
		}
	}
	return true
}

// judgeKubeWindows judges a pod's or a container's windowsOptions: a
// host-process container runs on the node itself, and a gMSA credential spec
// hands the pod a domain identity, so only hostProcess (not true) and
// runAsUserName may be set.
func judgeKubeWindows(sc map[string]any) error {
	wo, err := kubeMap(sc, "windowsOptions")
	if err != nil {
		return err
	}
	on, err := kubeTrue(wo, "hostProcess")
	if err != nil {
		return err
	}
	if on {
		return errKubePodHost
	}
	for k, v := range wo {
		if v != nil && k != "hostProcess" && k != "runAsUserName" {
			return errKubePodGMSA
		}
	}
	return nil
}

// judgeKubeContainer refuses a container with host access and one whose
// environment reads anything but a field, a resource, a ConfigMap or a file.
func judgeKubeContainer(c map[string]any) error {
	sc, err := kubeMap(c, "securityContext")
	if err != nil {
		return err
	}
	if err := judgeKubeContainerSecurity(sc); err != nil {
		return err
	}
	env, err := kubeList(c, "env")
	if err != nil {
		return err
	}
	for _, e := range env {
		from, err := kubeMap(e, "valueFrom")
		if err != nil {
			return err
		}
		if !kubeOnlyKeys(from, kubePlainValueFrom) {
			return errKubePodEnv
		}
	}
	envFrom, err := kubeList(c, "envFrom")
	if err != nil {
		return err
	}
	for _, e := range envFrom {
		if !kubeOnlyKeys(e, kubePlainEnvFrom) {
			return errKubePodEnv
		}
	}
	return nil
}

// judgeKubeContainerSecurity refuses a container security context that is
// privileged, adds a capability outside the baseline set, asks for a /proc
// mount other than Default, or sets Windows options that are not plain.
func judgeKubeContainerSecurity(sc map[string]any) error {
	priv, err := kubeTrue(sc, "privileged")
	if err != nil {
		return err
	}
	if priv {
		return errKubePodHost
	}
	caps, err := kubeMap(sc, "capabilities")
	if err != nil {
		return err
	}
	add, err := kubeKey(caps, "add")
	if err != nil {
		return err
	}
	if add != nil {
		list, ok := add.([]any)
		if !ok {
			return errKubePodShape
		}
		for _, e := range list {
			name, ok := e.(string)
			if !ok {
				return errKubePodShape
			}
			if !slices.Contains(kubeBaselineCapabilities, name) {
				return errKubePodHost
			}
		}
	}
	pm, err := kubeKey(sc, "procMount")
	if err != nil {
		return err
	}
	if pm != nil && pm != "Default" {
		return errKubePodHost
	}
	return judgeKubeWindows(sc)
}
