// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adofake

import "net/http"

// Fault is one of the refusal shapes the live Azure DevOps service answers
// with, as measured (F-LIVE-7): status, the headers that tell the shapes
// apart, and the body, byte for byte where the service's is short.
type Fault int

const (
	// FaultSignInPage is the answer to an absent, malformed or unknown
	// credential: 203 with a text/html sign-in page (the live page is ~16 KB;
	// this keeps its doctype and title).
	FaultSignInPage Fault = iota + 1
	// FaultBasicUnauthorized is a token missing the scope, expired or revoked —
	// the three are byte-identical: 401, WWW-Authenticate Basic, no
	// Content-Type, an empty body.
	FaultBasicUnauthorized
	// FaultBearerTF400813 is an Entra bearer refused: 401, WWW-Authenticate
	// Bearer, X-TFS-ServiceError, and a TF400813 body behind a UTF-8 BOM.
	FaultBearerTF400813
	// FaultRepoNotFound is an authenticated identity without read on the
	// repository: 404 TF401019 with X-VSS-UserData.
	FaultRepoNotFound
	// FaultPolicyRejected is an authenticated identity a branch policy
	// refuses: 403 TF402455 with X-VSS-UserData.
	FaultPolicyRejected
)

// faultUserData stands in for the live X-VSS-UserData value (<id>:<account>).
const faultUserData = "00000000-0000-0000-0000-000000000001:person@example.test"

// SignInPage is the body FaultSignInPage answers with.
const SignInPage = `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-strict.dtd">
<html lang="en-US">
<head><title>
            Azure DevOps Services | Sign In
</title></head>
<body></body>
</html>
`

// SetFault makes every subsequent call to endpoint answer with fault's shape,
// the way SetOverride does (scope is still decided and recorded).
// ClearOverride removes it.
func (s *Server) SetFault(endpoint Endpoint, fault Fault) {
	ov := override{header: http.Header{}}
	switch fault {
	case FaultSignInPage:
		ov.status = http.StatusNonAuthoritativeInfo
		ov.header.Set("Content-Type", "text/html; charset=utf-8")
		ov.header.Set("X-Content-Type-Options", "nosniff")
		ov.body = []byte(SignInPage)
	case FaultBasicUnauthorized:
		ov.status = http.StatusUnauthorized
		ov.header.Set("WWW-Authenticate", `Basic realm="https://tfsprodcus14.visualstudio.com/"`)
		ov.header.Set("X-Content-Type-Options", "nosniff")
	case FaultBearerTF400813:
		ov.status = http.StatusUnauthorized
		ov.header.Set("Content-Type", "application/json")
		ov.header.Set("WWW-Authenticate", "Bearer")
		ov.header.Set("X-TFS-ServiceError", "TF400813%3A%20The%20user%20is%20not%20authorized%20to%20access%20this%20resource.%20")
		ov.body = []byte("\ufeff" + `{"$id":"1","innerException":null,"message":"TF400813: The user is not authorized to access this resource. "` +
			`,"typeName":"Microsoft.TeamFoundation.Framework.Server.InvalidIdentityException, Microsoft.TeamFoundation.Framework.Server","typeKey":"InvalidIdentityException","errorCode":0,"eventId":3000}`)
	case FaultRepoNotFound:
		ov.status = http.StatusNotFound
		ov.header.Set("Content-Type", "application/json; charset=utf-8")
		ov.header.Set("X-VSS-UserData", faultUserData)
		ov.body = []byte(`{"$id":"1","innerException":null,"message":"TF401019: The Git repository with name or identifier acb53563-0846-4ea7-adc6-8b3970f04f22 does not exist or you do not have permissions for the operation you are attempting."` +
			`,"typeName":"Microsoft.TeamFoundation.Git.Server.GitRepositoryNotFoundException, Microsoft.TeamFoundation.Git.Server","typeKey":"GitRepositoryNotFoundException","errorCode":0,"eventId":3000}`)
	case FaultPolicyRejected:
		ov.status = http.StatusForbidden
		ov.header.Set("Content-Type", "application/json; charset=utf-8")
		ov.header.Set("X-VSS-UserData", faultUserData)
		ov.body = []byte(`{"$id":"1","innerException":null,"message":"TF402455: Pushes to this branch are not permitted; you must use a pull request to update this branch."` +
			`,"typeName":"Microsoft.TeamFoundation.Git.Server.GitRefUpdateRejectedByPolicyException, Microsoft.TeamFoundation.Git.Server","typeKey":"GitRefUpdateRejectedByPolicyException","errorCode":0,"eventId":3000}`)
	default:
		panic("adofake: unknown fault")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overrides[endpoint] = ov
}
