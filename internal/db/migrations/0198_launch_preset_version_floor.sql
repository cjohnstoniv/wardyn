-- Copyright 2026 The Wardyn Authors
-- SPDX-License-Identifier: Apache-2.0

-- The highest version a launch preset reached before it was deleted. A preset recreated under the
-- same name starts above it, so a version names one request for the life of the name and a client
-- holding an older version is refused (preset_version_moved) instead of launching the new content.
CREATE TABLE IF NOT EXISTS launch_preset_version_floor (
    name    TEXT PRIMARY KEY,
    version INTEGER NOT NULL
);
