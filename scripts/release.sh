#!/usr/bin/env bash
# Cuts a release: scripts/release.sh v1.2.3 [--dry-run]
# Checks that you are on an up-to-date, clean main whose CI is green, then pushes an annotated tag. The tag triggers
# the CI workflow, which publishes ghcr.io/<repo>:1.2.3 (+ :1.2, :1, :stable) and creates the GitHub Release.
set -euo pipefail

tag="${1:-}"
dry=0
[ "${2:-}" = "--dry-run" ] && dry=1
if ! [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "usage: $0 vMAJOR.MINOR.PATCH[-prerelease] [--dry-run]   e.g. v1.2.3 or v1.0.0-rc.1" >&2
  exit 2
fi

branch=$(git rev-parse --abbrev-ref HEAD)
[ "$branch" = main ] || { echo "not on main (on $branch)" >&2; exit 1; }
[ -z "$(git status --porcelain)" ] || { echo "working tree is not clean" >&2; exit 1; }
git fetch -q origin main --tags
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || { echo "main is not at origin/main; pull or push first" >&2; exit 1; }
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then echo "tag $tag already exists" >&2; exit 1; fi

sha=$(git rev-parse HEAD)
status=$(gh run list --branch main --workflow CI --commit "$sha" --limit 1 --json status,conclusion --jq '.[0] | "\(.status) \(.conclusion)"' 2>/dev/null || true)
[ "$status" = "completed success" ] || { echo "CI for ${sha:0:7} is not green yet (got: ${status:-none}); wait for it" >&2; exit 1; }

prev=$(git describe --tags --abbrev=0 2>/dev/null || true)
echo "releasing $tag at ${sha:0:7}${prev:+ (previous: $prev)}"
git log --oneline "${prev:+$prev..}HEAD" | head -30
if [ "$dry" = 1 ]; then echo "dry run: nothing tagged"; exit 0; fi

git tag -a "$tag" -m "$tag"
git push origin "$tag"
echo "pushed $tag; follow it with: gh run watch --exit-status \$(gh run list --branch $tag --limit 1 --json databaseId --jq '.[0].databaseId')"
