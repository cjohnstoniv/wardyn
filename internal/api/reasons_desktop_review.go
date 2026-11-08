// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The reviewed-download reasons (desktop.download review, desktop_review.go): the same closed set and
// convention as reasons.go, in a file of their own beside that code. TestReasonDocsMatchReasonsGo reads this
// file too.
const (
	reasonDesktopReviewRequired    = "desktop_review_required"     // the run's downloads are held for review and no approval was named
	reasonDesktopReviewNotHeld     = "desktop_review_not_held"     // a review was asked of a run whose downloads are allowed
	reasonDesktopReviewPending     = "desktop_review_pending"      // a review of that file is already waiting
	reasonDesktopReviewNotApproved = "desktop_review_not_approved" // the approval named is not an approved review of this file on this run
	reasonDesktopReviewSpent       = "desktop_review_spent"        // the approval already released its file
	reasonFileChangedSinceReview   = "file_changed_since_review"   // the file's bytes are not the ones reviewed; nothing was sent
	reasonDesktopReviewUnavailable = "desktop_review_unavailable"  // the approvals could not be read, raised or spent; nothing was raised or sent
)
