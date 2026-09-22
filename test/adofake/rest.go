// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adofake

import (
	"encoding/json"
	"net/http"
	"time"
)

// policyConfig is a stored branch-policy configuration, keyed by the
// repositoryId/refName pair pulled out of the real API's
// settings.scope[0].{repositoryId,refName} shape — that pair is what a
// "is this ref protected" lookup filters on.
type policyConfig struct {
	id           int
	repositoryID string
	refName      string
	raw          map[string]any // the posted/updated body, "id" kept in sync
}

func decodeJSONMap(r *http.Request) map[string]any {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body == nil {
		body = map[string]any{}
	}
	return body
}

// scopeFromPolicyBody extracts the repositoryId/refName a policy configuration
// applies to from settings.scope[0], the real API's shape for a single-branch
// scoped policy.
func scopeFromPolicyBody(body map[string]any) (repositoryID, refName string) {
	settings, _ := body["settings"].(map[string]any)
	if settings == nil {
		return "", ""
	}
	scopes, _ := settings["scope"].([]any)
	if len(scopes) == 0 {
		return "", ""
	}
	first, _ := scopes[0].(map[string]any)
	repositoryID, _ = first["repositoryId"].(string)
	refName, _ = first["refName"].(string)
	return repositoryID, refName
}

// handlePolicyGet answers GET .../_apis/policy/configurations, optionally
// filtered by ?repositoryId=&refName= — the query shape a caller uses to ask
// "is this ref protected".
func (s *Server) handlePolicyGet(w http.ResponseWriter, r *http.Request) {
	repositoryID := r.URL.Query().Get("repositoryId")
	refName := r.URL.Query().Get("refName")

	s.mu.Lock()
	var out []map[string]any
	for _, p := range s.policies {
		if repositoryID != "" && p.repositoryID != repositoryID {
			continue
		}
		if refName != "" && p.refName != refName {
			continue
		}
		out = append(out, p.raw)
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "value": out})
}

// handlePolicyPost creates a branch-policy configuration — including a
// protected-branch registration, since that IS a policy configuration in the
// real API (a required-reviewer or a require-a-build policy scoped to a ref).
func (s *Server) handlePolicyPost(w http.ResponseWriter, r *http.Request) {
	body := decodeJSONMap(r)
	repositoryID, refName := scopeFromPolicyBody(body)

	s.mu.Lock()
	s.nextPolicyID++
	id := s.nextPolicyID
	body["id"] = id
	s.policies = append(s.policies, policyConfig{id: id, repositoryID: repositoryID, refName: refName, raw: body})
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, body)
}

// handlePolicyPut updates an existing policy configuration by id, merging the
// posted fields over the stored ones.
func (s *Server) handlePolicyPut(w http.ResponseWriter, r *http.Request) {
	id := atoiOr(r.PathValue("id"), -1)
	body := decodeJSONMap(r)
	body["id"] = id

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.policies {
		if s.policies[i].id != id {
			continue
		}
		repositoryID, refName := scopeFromPolicyBody(body)
		s.policies[i] = policyConfig{id: id, repositoryID: repositoryID, refName: refName, raw: body}
		writeJSON(w, http.StatusOK, body)
		return
	}
	http.NotFound(w, r)
}

// handlePullRequestsPost creates a pull request, assigning it the next
// pullRequestId.
func (s *Server) handlePullRequestsPost(w http.ResponseWriter, r *http.Request) {
	body := decodeJSONMap(r)

	s.mu.Lock()
	id := s.nextPRID
	s.nextPRID++
	body["pullRequestId"] = id
	if _, ok := body["status"]; !ok {
		body["status"] = "active"
	}
	s.pullRequests[id] = body
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, body)
}

// handlePullRequestsPatch updates a pull request — including a
// completionOptions.bypassPolicy body, the shape a broker uses to complete a
// PR over a policy the token's own write scope already justified.
func (s *Server) handlePullRequestsPatch(w http.ResponseWriter, r *http.Request) {
	id := atoiOr(r.PathValue("prId"), -1)
	patch := decodeJSONMap(r)

	s.mu.Lock()
	defer s.mu.Unlock()
	pr, ok := s.pullRequests[id]
	if !ok {
		http.NotFound(w, r)
		return
	}
	for k, v := range patch {
		pr[k] = v
	}
	pr["pullRequestId"] = id
	s.pullRequests[id] = pr
	writeJSON(w, http.StatusOK, pr)
}

// handleRefsPost answers POST .../refs, the ref-update API a push-by-API (as
// opposed to git-receive-pack) uses: it echoes every requested update as
// succeeded.
func (s *Server) handleRefsPost(w http.ResponseWriter, r *http.Request) {
	body := decodeJSONMap(r)
	updates, _ := body["refUpdates"].([]any)

	out := make([]map[string]any, 0, len(updates))
	for _, u := range updates {
		update, _ := u.(map[string]any)
		out = append(out, map[string]any{
			"name":         update["name"],
			"newObjectId":  update["newObjectId"],
			"updateStatus": "succeeded",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "value": out})
}

// handlePushesGet lists every push handlePushesPost has recorded.
func (s *Server) handlePushesGet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	out := append([]map[string]any(nil), s.pushes...)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "value": out})
}

// handlePushesPost records a push made through the REST API (as opposed to
// git-receive-pack, which git.go serves directly).
func (s *Server) handlePushesPost(w http.ResponseWriter, r *http.Request) {
	body := decodeJSONMap(r)

	s.mu.Lock()
	id := s.nextPushID
	s.nextPushID++
	body["pushId"] = id
	body["date"] = time.Now().UTC().Format(time.RFC3339)
	s.pushes = append(s.pushes, body)
	s.mu.Unlock()

	writeJSON(w, http.StatusCreated, body)
}

// handleWiqlPost answers a work-item query with one canned result — enough
// for a caller to prove its query reached the fake with the right scope and
// got back a work item id to act on.
func (s *Server) handleWiqlPost(w http.ResponseWriter, r *http.Request) {
	_ = decodeJSONMap(r) // the query text itself isn't interpreted
	writeJSON(w, http.StatusOK, map[string]any{
		"queryType":       "flat",
		"queryResultType": "workItem",
		"asOf":            time.Now().UTC().Format(time.RFC3339),
		"columns":         []map[string]any{{"referenceName": "System.Id", "name": "ID"}},
		"workItems":       []map[string]any{{"id": 1, "url": r.Host + "/_apis/wit/workItems/1"}},
	})
}

// handleWorkItemsPatch applies a JSON-Patch document ([{op,path,value}, ...],
// the real API's update shape) to a work item's field set and bumps its
// revision.
func (s *Server) handleWorkItemsPatch(w http.ResponseWriter, r *http.Request) {
	id := atoiOr(r.PathValue("id"), -1)
	var ops []map[string]any
	_ = json.NewDecoder(r.Body).Decode(&ops)

	fields := map[string]any{}
	for _, op := range ops {
		path, _ := op["path"].(string)
		const fieldPrefix = "/fields/"
		if len(path) > len(fieldPrefix) && path[:len(fieldPrefix)] == fieldPrefix {
			fields[path[len(fieldPrefix):]] = op["value"]
		}
	}

	s.mu.Lock()
	s.workItemRev[id]++
	rev := s.workItemRev[id]
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"id": id, "rev": rev, "fields": fields})
}

// handleConnectionData answers GET .../_apis/connectionData with a canned
// identity — enough for a caller to prove IT authenticated, without modelling
// the real service's full identity graph.
func (s *Server) handleConnectionData(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticatedUser": map[string]any{"id": "00000000-0000-0000-0000-000000000001", "providerDisplayName": "adofake"},
		"authorizedUser":    map[string]any{"id": "00000000-0000-0000-0000-000000000001", "providerDisplayName": "adofake"},
		"instanceId":        "00000000-0000-0000-0000-000000000002",
		"deploymentId":      "00000000-0000-0000-0000-000000000003",
		"deploymentType":    "hosted",
	})
}

// handleProjectsGet answers GET .../_apis/projects with the fixtures a test
// registered via AddProject.
func (s *Server) handleProjectsGet(w http.ResponseWriter, r *http.Request) {
	org := r.PathValue("org")
	s.mu.Lock()
	out := append([]map[string]any(nil), s.projects[org]...)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "value": out})
}

// atoiOr parses s as a base-10 int, returning fallback on any error —
// path-value parsing has no error path worth propagating in a test fake.
func atoiOr(s string, fallback int) int {
	n := 0
	if s == "" {
		return fallback
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return fallback
		}
		n = n*10 + int(c-'0')
	}
	return n
}
