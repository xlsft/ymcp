#!/usr/bin/env bash
# Ticks the checklist of the winget-pkgs pull request a release opened, e.g.
#
#   GH_TOKEN=… .github/scripts/winget-checklist.sh 0.10.0
#
# With DRY_RUN=1 it prints the checklist it would set instead of editing.
#
# GoReleaser copies the winget-pkgs pull request template with every box
# empty, and the policy bot labels the pull request Needs-CLA until the CLA
# box is ticked, even when the license/cla check passes. Run it only after
# the manifest was validated and installed (the winget workflow does both);
# the box about other open pull requests is ticked only when there are none.
set -euo pipefail

version=${1#v}
repo=microsoft/winget-pkgs

pr=$(gh pr list --repo "$repo" --author @me --state open --search "xlsft.ymcp $version in:title" \
  --json number,headRefName -q ".[] | select(.headRefName == \"ymcp-$version\") | .number")
if [ -z "$pr" ]; then
  echo "no open $repo pull request for xlsft.ymcp $version" >&2
  exit 1
fi

others=$(gh pr list --repo "$repo" --state open --search "xlsft.ymcp in:title" \
  --json number,title -q "[.[] | select(.number != $pr and (.title | test(\"\\\\bxlsft\\\\.ymcp\\\\b\")))] | length")

items=(
  "Signed the \[Contributor License Agreement\]"
  "This PR only modifies one \\(1\\) manifest"
  "Validated manifest locally"
  "Tested manifest locally"
  "Manifest conforms to the"
)
if [ "$others" = 0 ]; then
  items+=("Checked that there aren't other open")
else
  echo "$others other open pull request(s) for xlsft.ymcp; leaving that box empty" >&2
fi

body=$(gh pr view "$pr" --repo "$repo" --json body -q .body)
new=$body
for item in "${items[@]}"; do
  new=$(sed -E "s/^- \[ \] ($item)/- [x] \1/" <<<"$new")
done

if [ -n "${DRY_RUN:-}" ]; then
  grep -E '^ *- \[[ x]\]' <<<"$new"
  exit 0
fi
if [ "$new" = "$body" ]; then
  echo "$repo#$pr: checklist already ticked"
  exit 0
fi
gh pr edit "$pr" --repo "$repo" --body "$new" >/dev/null
echo "$repo#$pr: checklist ticked"
gh pr view "$pr" --repo "$repo" --json body -q .body | grep -E '^ *- \[[ x]\]'
