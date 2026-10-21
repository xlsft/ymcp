#!/usr/bin/env bash
# Prints the next version (e.g. v0.4.0) from the conventional commits since
# the last v* tag, or nothing when those commits don't call for a release.
#
#   feat           -> minor
#   fix, perf      -> patch
#   type!: / BREAKING CHANGE -> major (minor while on 0.x)
#   anything else (docs, test, ci, build, chore, refactor) -> no release
set -euo pipefail

last=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || true)
range=HEAD
[ -n "$last" ] && range="$last..HEAD"

subjects=$(git log --format=%s "$range")
bodies=$(git log --format=%b "$range")

bump=""
if grep -qE '^[a-z]+(\([^)]*\))?!:' <<<"$subjects" || grep -q '^BREAKING CHANGE' <<<"$bodies"; then
  bump=major
elif grep -qE '^feat(\([^)]*\))?:' <<<"$subjects"; then
  bump=minor
elif grep -qE '^(fix|perf)(\([^)]*\))?:' <<<"$subjects"; then
  bump=patch
fi
[ -z "$bump" ] && exit 0

IFS=. read -r major minor patch <<<"${last#v}"
major=${major:-0} minor=${minor:-0} patch=${patch:-0}
case $bump in
  major) if [ "$major" -eq 0 ]; then minor=$((minor + 1)); else major=$((major + 1)); minor=0; fi; patch=0 ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac
echo "v$major.$minor.$patch"
