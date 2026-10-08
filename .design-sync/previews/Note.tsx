/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Note, SetupAccessNote } from '@wardyn/ui';

export const Plain = () => <Note>Source policy</Note>;
export const Warning = () => <Note tone="amber">Source policy</Note>;
export const Error = () => <Note tone="red">Source policy</Note>;
export const Access = () => <SetupAccessNote>Added by you</SetupAccessNote>;
