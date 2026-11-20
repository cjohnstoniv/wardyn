#!/bin/sh
# Copyright 2025 The Wardyn Authors
# SPDX-License-Identifier: Apache-2.0
#
# Build /<dir>/asciinema.deb: Debian's asciinema package with ONE change, its
# Depends line no longer names python3-pkg-resources.
#
# WHY. asciinema 2.4.0 never imports pkg_resources (nothing under its python
# tree mentions it; this script refuses to build if that stops being true), but
# Debian declares the dependency, and python3-pkg-resources drags in
# python3-jaraco.context, -jaraco.functools, -autocommand, -more-itertools,
# -inflect and -typeguard. A scanner flags CVE-2026-23949 (jaraco.context
# tarball path traversal) on that chain; Debian trixie ships no setuptools fix
# (tracker: setuptools 78.1.1-0.1 "vulnerable"), and nothing in the image can
# reach the vulnerable jaraco.context.tarball() code. Removing the chain is the
# fix, and an honest one: dpkg records stay intact, the package list is simply
# shorter, and `apt` stays consistent for the images built FROM agent-base.
#
# The rebuilt package carries a "+wardyn1" version suffix so the dpkg record
# says it is not Debian's byte-for-byte package. Corresponding source is
# Debian's asciinema source package at the base version plus this one-line
# Depends change.
#
# Usage: asciinema-deb.sh <empty work dir>   (root, after `apt-get update`)
set -eu
work=${1:?usage: asciinema-deb.sh <work dir>}
mkdir -p "$work"
chmod 0755 "$work"
cd "$work"
apt-get download asciinema
dpkg-deb --raw-extract ./asciinema_*.deb src
if grep -rIl 'pkg_resources' src/usr; then
  echo "asciinema now uses pkg_resources: its python3-pkg-resources dependency is real, do not drop it" >&2
  exit 1
fi
grep -qx 'Depends: python3:any, python3-pkg-resources' src/DEBIAN/control || {
  echo "asciinema's Depends line changed upstream: re-check it before dropping a dependency" >&2
  exit 1
}
sed -i -e 's/^Depends: python3:any, python3-pkg-resources$/Depends: python3:any/' \
       -e 's/^Version: \(.*\)$/Version: \1+wardyn1/' src/DEBIAN/control
dpkg-deb --root-owner-group --build src asciinema.deb
