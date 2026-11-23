// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The kubectl door's pod-spec check. A run whose kubectl may write can create a
// pod and read what it prints through pods/<name>/log, so a pod that mounts a
// service account token, or reads a Secret into a file or a variable, would
// hand that credential to the sandbox. A create or replace of a resource that
// carries a pod spec must set automountServiceAccountToken: false and name no
// Secret. A patch is merged into an object the door never sees, so one that
// touches the pod spec at all is refused, and so is any JSON Patch.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ponytail: the door has no body cap of its own; 1 MiB holds any pod or
// workload kubectl sends (the API server's own limit is 3 MiB).
const kubePodSpecMaxBody = 1 << 20

var (
	errKubePodToken  = errors.New("this run's kubectl cannot create a pod that mounts a service account token: set automountServiceAccountToken: false")
	errKubePodSecret = errors.New("this run's kubectl cannot create a pod that reads a Secret")
	errKubePodShape  = errors.New("this run's kubectl sent a pod or workload Wardyn cannot read")
)

// kubePodSpecPaths maps each resource that carries a pod spec, in any API
// group, to the keys (lower case) that lead from the object to its pod spec.
var kubePodSpecPaths = map[string][]string{
	"pods":                   {"spec"},
	"deployments":            {"spec", "template", "spec"},
	"replicasets":            {"spec", "template", "spec"},
	"statefulsets":           {"spec", "template", "spec"},
	"daemonsets":             {"spec", "template", "spec"},
	"jobs":                   {"spec", "template", "spec"},
	"replicationcontrollers": {"spec", "template", "spec"},
	"cronjobs":               {"spec", "jobtemplate", "spec", "template", "spec"},
	"podtemplates":           {"template", "spec"},
}

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
	obj, err := kubeFoldedObject(body)
	if err != nil {
		return err
	}
	if r.Method == http.MethodPatch {
		return judgeKubePodSpecPatch(obj, path)
	}
	spec, _ := kubeWalk(obj, path)
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

// kubeFoldedObject parses body as one JSON object with every key lower-cased.
// A key repeated, even in another case, is refused: two parsers could read it
// differently.
func kubeFoldedObject(body []byte) (map[string]any, error) {
	if _, err := types.CanonicalJSON(body); err != nil {
		return nil, errKubePodShape
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, errKubePodShape
	}
	folded, ok := kubeFoldKeys(v)
	obj, isObj := folded.(map[string]any)
	if !ok || !isObj {
		return nil, errKubePodShape
	}
	return obj, nil
}

func kubeFoldKeys(v any) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			lk := strings.ToLower(k)
			if _, dup := out[lk]; dup {
				return nil, false
			}
			f, ok := kubeFoldKeys(e)
			if !ok {
				return nil, false
			}
			out[lk] = f
		}
		return out, true
	case []any:
		for i, e := range t {
			f, ok := kubeFoldKeys(e)
			if !ok {
				return nil, false
			}
			t[i] = f
		}
	}
	return v, true
}

// kubeWalk follows path from obj; it reports at which step it stopped (len(path)
// when it reached a value).
func kubeWalk(obj map[string]any, path []string) (map[string]any, int) {
	cur := obj
	for i, k := range path {
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil, i
		}
		cur = next
	}
	return cur, len(path)
}

// judgeKubePodSpecPatch admits a merge patch only when it leaves the pod spec
// alone: every object on the way to it holds no strategic merge directive, and
// the patch names no step of the path past where it stops.
func judgeKubePodSpecPatch(obj map[string]any, path []string) error {
	_, at := kubeWalk(obj, path)
	cur := obj
	for i := 0; i <= at && i < len(path); i++ {
		for k := range cur {
			if strings.HasPrefix(k, "$") {
				return errors.New("this run's kubectl cannot send a strategic merge directive to a pod or workload")
			}
		}
		if i == at {
			if _, named := cur[path[i]]; named {
				return errKubePodShape
			}
			return nil
		}
		cur = cur[path[i]].(map[string]any)
	}
	return errors.New("this run's kubectl cannot patch the pod spec of a pod or workload, because Wardyn cannot see what it already mounts: replace the whole object instead")
}

// judgeKubePodSpec refuses a pod spec that does not set
// automountServiceAccountToken: false, or that names a Secret or a service
// account token in a volume or a container's environment.
func judgeKubePodSpec(spec map[string]any) error {
	if off, ok := spec["automountserviceaccounttoken"].(bool); !ok || off {
		return errKubePodToken
	}
	vols, ok := kubeObjects(spec["volumes"])
	if !ok {
		return errKubePodShape
	}
	for _, v := range vols {
		if _, has := v["secret"]; has {
			return errKubePodSecret
		}
		if err := judgeKubeProjected(v["projected"]); err != nil {
			return err
		}
	}
	for _, key := range []string{"containers", "initcontainers", "ephemeralcontainers"} {
		cs, ok := kubeObjects(spec[key])
		if !ok {
			return errKubePodShape
		}
		for _, c := range cs {
			if err := judgeKubeContainerEnv(c); err != nil {
				return err
			}
		}
	}
	return nil
}

func judgeKubeProjected(p any) error {
	if p == nil {
		return nil
	}
	proj, ok := p.(map[string]any)
	if !ok {
		return errKubePodShape
	}
	srcs, ok := kubeObjects(proj["sources"])
	if !ok {
		return errKubePodShape
	}
	for _, s := range srcs {
		if _, has := s["secret"]; has {
			return errKubePodSecret
		}
		if _, has := s["serviceaccounttoken"]; has {
			return errors.New("this run's kubectl cannot create a pod that mounts a service account token")
		}
	}
	return nil
}

func judgeKubeContainerEnv(c map[string]any) error {
	env, ok := kubeObjects(c["env"])
	if !ok {
		return errKubePodShape
	}
	for _, e := range env {
		if e["valuefrom"] == nil {
			continue
		}
		from, ok := e["valuefrom"].(map[string]any)
		if !ok {
			return errKubePodShape
		}
		if _, has := from["secretkeyref"]; has {
			return errKubePodSecret
		}
	}
	envFrom, ok := kubeObjects(c["envfrom"])
	if !ok {
		return errKubePodShape
	}
	for _, e := range envFrom {
		if _, has := e["secretref"]; has {
			return errKubePodSecret
		}
	}
	return nil
}

// kubeObjects reads an absent or null value as no items, and refuses anything
// but a list of objects.
func kubeObjects(v any) ([]map[string]any, bool) {
	if v == nil {
		return nil, true
	}
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, false
		}
		out = append(out, m)
	}
	return out, true
}
