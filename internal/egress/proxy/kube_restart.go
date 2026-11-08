// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The restart arm of the kubectl door. A row that says `restart` reads like a
// read row and changes exactly one thing: the request `kubectl rollout restart
// deployment/<name>` sends, a strategic merge patch whose only content is the
// restartedAt annotation on the deployment's pod template. Nothing else is
// admitted: not another verb, resource, group, query, media type, encoding,
// size or patch content. It is judged here, after the namespace and path gate
// in admit, and before a token is minted.
//
// The shape was verified against kubectl v0.32.2: rollout_restart.go sends the
// patch through resource.Helper.Patch with the field manager "kubectl-rollout"
// and types.StrategicMergePatchType, and objectrestarter.go sets the annotation
// to time.Now().Format(time.RFC3339).

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	kubeRestartMediaType   = "application/strategic-merge-patch+json"
	kubeRestartQuery       = "fieldManager=kubectl-rollout"
	kubeRestartAnnotation  = types.KubeRestartAnnotation
	kubeRestartMaxBody     = 4 << 10
	kubeRestartRefusalLead = "this run's kubectl may only restart a deployment (kubectl rollout restart); "
)

// restartOnly reports whether the door's one change is the restart.
func (d *kubeAPIDoor) restartOnly() bool { return d.restart && !d.write }

// admitRestart judges a change on a restart-only door and, when it is the
// restart, leaves r.Body readable again with the bytes that were checked, so
// what is forwarded is what was judged. raw is the raw request path.
func (d *kubeAPIDoor) admitRestart(r *http.Request, raw string) error {
	refuse := func(why string) error { return errors.New(kubeRestartRefusalLead + why) }
	segs := strings.Split(strings.TrimPrefix(raw, "/"), "/")
	switch {
	case r.Method != http.MethodPatch:
		return refuse("the method " + r.Method + " is not one it sends")
	case len(segs) != 7 || segs[0] != "apis" || segs[1] != "apps" || segs[2] != "v1" || segs[3] != "namespaces" || segs[5] != "deployments":
		return refuse("only a named deployment in apps/v1 is restarted")
	case r.URL.RawQuery != "" && r.URL.RawQuery != kubeRestartQuery:
		return refuse("the query must be empty or " + kubeRestartQuery)
	}
	if ct := r.Header.Values("Content-Type"); len(ct) != 1 {
		return refuse("the patch must be a " + kubeRestartMediaType)
	} else if mt, _, err := mime.ParseMediaType(ct[0]); err != nil || mt != kubeRestartMediaType {
		return refuse("the patch must be a " + kubeRestartMediaType)
	}
	for _, enc := range r.Header.Values("Content-Encoding") {
		if v := strings.ToLower(strings.TrimSpace(enc)); v != "" && v != "identity" {
			return refuse("the patch must not be encoded")
		}
	}
	if r.ContentLength > kubeRestartMaxBody {
		return refuse("the patch is over 4 KiB")
	}
	if r.Body == nil || r.Body == http.NoBody {
		return refuse("the patch is empty")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, kubeRestartMaxBody+1))
	if err != nil {
		return refuse("the patch could not be read")
	}
	if len(body) > kubeRestartMaxBody {
		return refuse("the patch is over 4 KiB")
	}
	if !kubeRestartPatch(body) {
		return refuse("the patch may set only the " + kubeRestartAnnotation + " annotation")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength, r.TransferEncoding = int64(len(body)), nil
	return nil
}

// kubeRestartPatch reports whether body is exactly the restart patch
// (types.KubeRestartPatch), the one the held-write key also reads.
func kubeRestartPatch(body []byte) bool { return types.KubeRestartPatch(body) }
