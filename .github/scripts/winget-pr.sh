#!/usr/bin/env bash
# Finishes the winget-pkgs pull request a release opened, e.g.
#
#   GH_TOKEN=… .github/scripts/winget-pr.sh checklist 0.10.1
#   GH_TOKEN=… .github/scripts/winget-pr.sh comments 0.10.1
#
# With DRY_RUN=1 it prints what it would change instead of changing it.
#
# checklist: GoReleaser copies the winget-pkgs pull request template with
# every box empty, and reviewers ask for it filled in. Run it only after the
# manifest was validated and installed (the winget workflow does both); the
# box about other open pull requests is ticked only when there are none.
#
# comments: the policy bot labels a first-time contributor's pull request
# Needs-CLA and removes the label on the author's agree comment, which is
# posted while the label is there. winget's install validation runs the
# executable, and ymcp without arguments starts the server and exits 1 with
# no profile; a comment asks reviewers to validate with --version instead.
# Each comment is posted once per pull request.
#
# Edits go through the REST API: gh pr edit and gh pr comment use GraphQL
# queries that need read:org, which the public_repo token doesn't have.
set -euo pipefail

usage="usage: $0 checklist|comments VERSION"
action=${1:?$usage}
version=${2:?$usage}
version=${version#v}
repo=microsoft/winget-pkgs

pr=$(gh pr list --repo "$repo" --author @me --state open --search "xlsft.ymcp $version in:title" \
  --json number,headRefName -q ".[] | select(.headRefName == \"ymcp-$version\") | .number")
if [ -z "$pr" ]; then
  echo "no open $repo pull request for xlsft.ymcp $version" >&2
  exit 1
fi

checklist() {
  local others body new item
  others=$(gh pr list --repo "$repo" --state open --search "xlsft.ymcp in:title" \
    --json number,title -q "[.[] | select(.number != $pr and (.title | test(\"\\\\bxlsft\\\\.ymcp\\\\b\")))] | length")

  local items=(
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
    return
  fi
  if [ "$new" = "$body" ]; then
    echo "$repo#$pr: checklist already ticked"
    return
  fi
  gh api -X PATCH "repos/$repo/pulls/$pr" -f body="$new" >/dev/null
  echo "$repo#$pr: checklist ticked"
  gh pr view "$pr" --repo "$repo" --json body -q .body | grep -E '^ *- \[[ x]\]'
}

comments() {
  local me mine labels
  me=$(gh api user -q .login)
  mine=$(gh api --paginate "repos/$repo/issues/$pr/comments" -q ".[] | select(.user.login == \"$me\") | .body")
  labels=$(gh pr view "$pr" --repo "$repo" --json labels -q '.labels[].name')

  if grep -qx Needs-CLA <<<"$labels"; then
    post "@microsoft-github-policy-service agree" "@microsoft-github-policy-service agree" "$mine"
  fi
  post "ymcp.exe --version" 'The package is a command-line MCP server: run without arguments it starts serving and exits 1 until a YouTrack profile is configured. Please validate it with `ymcp.exe --version`, which prints the version and exits 0.' "$mine"
}

# post MARKER BODY MINE posts BODY unless one of MINE already contains MARKER.
post() {
  if grep -qF -- "$1" <<<"$3"; then
    echo "$repo#$pr: already commented: $1"
  elif [ -n "${DRY_RUN:-}" ]; then
    echo "$repo#$pr: would comment: $2"
  else
    gh api "repos/$repo/issues/$pr/comments" -f body="$2" >/dev/null
    echo "$repo#$pr: commented: $1"
  fi
}

case $action in
  checklist) checklist ;;
  comments) comments ;;
  *) echo "$usage" >&2; exit 2 ;;
esac
