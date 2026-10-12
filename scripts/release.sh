#!/usr/bin/env bash
# Cuts a release: scripts/release.sh v1.2.3 [--dry-run]
# Checks that you are on an up-to-date, clean main whose CI is green, then pushes an annotated tag. The tag triggers
# the CI workflow, which publishes ghcr.io/<repo>:1.2.3 (+ :1.2, :1, :stable) and creates the GitHub Release.
set -euo pipefail

usage() { echo "usage: $0 vMAJOR.MINOR.PATCH[-prerelease] [--dry-run] [--allow-older]   e.g. v1.2.3 or v1.0.0-rc.1" >&2; exit 2; }
tag="${1:-}"
dry=0
older=0
[ -n "$tag" ] || usage
shift
for a in "$@"; do
  case "$a" in
    --dry-run) dry=1 ;;
    --allow-older) older=1 ;; # a backport release (v1.1.5 after v1.2.0)
    *) echo "unknown argument: $a" >&2; usage ;; # a mistyped --dry-run must never cut a real release
  esac
done
if ! [[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "not a valid version: $tag (want vMAJOR.MINOR.PATCH[-prerelease], no leading zeros)" >&2
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
# Releasing something older than the newest stable tag would move :stable (and GitHub's "Latest") backwards.
newest=$(git tag -l 'v*' | grep -Ev -- '-' | sort -V | tail -1 || true)
if [ -n "$newest" ] && [ "$older" = 0 ] && [[ "$tag" != *-* ]] && [ "$(printf '%s\n%s\n' "$newest" "$tag" | sort -V | tail -1)" != "$tag" ]; then
  echo "$tag is older than the newest release $newest; releasing it would move :stable backwards (use --allow-older for a backport)" >&2
  exit 1
fi
echo "releasing $tag at ${sha:0:7}${prev:+ (previous: $prev)}"
range=HEAD
[ -z "$prev" ] || range="$prev..HEAD"
git log --oneline -30 "$range"
if [ "$dry" = 1 ]; then echo "dry run: nothing tagged"; exit 0; fi

git tag -a "$tag" -m "$tag"
git push origin "$tag"
echo "pushed $tag; follow it with: gh run watch --exit-status \$(gh run list --branch $tag --limit 1 --json databaseId --jq '.[0].databaseId')"
