#!/usr/bin/env bash
# Prints code=true or code=false: whether the change touches anything but
# documentation, so CI, E2E and releases can skip a change that doesn't.
#
#   code-changed.sh BASE HEAD   the files changed from their merge base
#   code-changed.sh             since the last v* tag (a push to main)
#
# Documentation is Markdown anywhere, docs/, site/, LICENSE and the issue
# and pull request templates. Everything else counts as code, workflows
# included. Without a base to compare with, the answer is true.
set -euo pipefail

docs='(^|/)[^/]+\.md$|^docs/|^site/|^LICENSE$|^\.github/ISSUE_TEMPLATE/|^\.github/pull_request_template\.md$'

if [ $# -ge 2 ]; then
  range="$1...$2"
else
  last=$(git describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || true)
  [ -n "$last" ] || { echo "code=true"; exit 0; }
  range="$last..HEAD"
fi

if ! files=$(git diff --name-only "$range" 2>/dev/null); then
  echo "code=true" # history we can't compare: run everything
  exit 0
fi
if printf '%s\n' "$files" | grep -v -E "$docs" | grep -q .; then
  echo "code=true"
else
  echo "code=false"
fi
