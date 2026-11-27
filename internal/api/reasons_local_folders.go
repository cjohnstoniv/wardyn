// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

const (
	reasonLocalFolderWalled          = "local_folder_walled"           // 422 or 403: an organisation wall on folders from a person's computer refuses this
	reasonLocalFolderWallsUnreadable = "local_folder_walls_unreadable" // 503: the run's folder walls could not be read; try again
)
