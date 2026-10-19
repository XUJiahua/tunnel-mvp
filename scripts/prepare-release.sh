#!/usr/bin/env bash
set -euo pipefail

tag=${RELEASE_TAG:?RELEASE_TAG is required}
if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo 'Expected a version such as v0.1.0 or v0.2.0-rc.1' >&2
  exit 1
fi

# Never move an existing version tag, even when a workflow is retried.
if git show-ref --verify --quiet "refs/tags/$tag"; then
  if [[ $(git rev-parse "refs/tags/$tag^{commit}") != $(git rev-parse HEAD) ]]; then
    echo "$tag points at another commit. Select that tag as the workflow ref to retry it." >&2
    exit 1
  fi
else
  git config user.name 'github-actions[bot]'
  git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
  git tag --annotate "$tag" --message "Release $tag"
  git push origin "refs/tags/$tag"
fi
