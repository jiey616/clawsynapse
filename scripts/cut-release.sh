#!/usr/bin/env bash

set -euo pipefail

DEFAULT_REMOTE="origin"
DEFAULT_BASE_BRANCH="develop"
DEFAULT_RELEASE_BRANCH="main"

usage() {
	cat <<'EOF'
Prepare and cut a ClawSynapse release from develop to main.

Usage:
  ./scripts/cut-release.sh v0.0.4

Options:
  --remote NAME          Git remote name (default: origin)
  --base-branch NAME     Source branch to release from (default: develop)
  --release-branch NAME  Target branch for releases (default: main)
  --no-push              Stop after local merge and tag creation
  -h, --help             Show this help

Behavior:
  1. Verify the worktree is clean.
  2. Fetch remote branches and tags.
  3. Checkout the release branch.
  4. Fast-forward it to the remote release branch.
  5. Fast-forward merge the base branch.
  6. Push the release branch.
  7. Create the release tag.
  8. Push the release tag.
  9. Checkout your original branch again.
EOF
}

VERSION=""
REMOTE="$DEFAULT_REMOTE"
BASE_BRANCH="$DEFAULT_BASE_BRANCH"
RELEASE_BRANCH="$DEFAULT_RELEASE_BRANCH"
NO_PUSH=0

while [ "$#" -gt 0 ]; do
	case "$1" in
		--remote)
			REMOTE="${2:-}"
			shift 2
			;;
		--base-branch)
			BASE_BRANCH="${2:-}"
			shift 2
			;;
		--release-branch)
			RELEASE_BRANCH="${2:-}"
			shift 2
			;;
		--no-push)
			NO_PUSH=1
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		-*)
			echo "unknown option: $1" >&2
			usage >&2
			exit 1
			;;
		*)
			if [ -n "$VERSION" ]; then
				echo "unexpected argument: $1" >&2
				usage >&2
				exit 1
			fi
			VERSION="$1"
			shift
			;;
	esac
done

if [ -z "$VERSION" ]; then
	echo "release version is required" >&2
	usage >&2
	exit 1
fi

case "$VERSION" in
	v[0-9]*.[0-9]*.[0-9]*)
		;;
	*)
		echo "release version must use semantic tag format like v0.0.4" >&2
		exit 1
		;;
esac

current_branch="$(git branch --show-current)"
cleanup() {
	if [ -n "$current_branch" ] && [ "$(git branch --show-current)" != "$current_branch" ]; then
		git checkout "$current_branch" >/dev/null
	fi
}
trap cleanup EXIT

if ! git diff --quiet || ! git diff --cached --quiet; then
	echo "worktree is not clean; commit or stash changes before cutting a release" >&2
	exit 1
fi

if git rev-parse -q --verify "refs/tags/${VERSION}" >/dev/null 2>&1; then
	echo "tag already exists locally: ${VERSION}" >&2
	exit 1
fi

if git ls-remote --tags "$REMOTE" "refs/tags/${VERSION}" | grep -q .; then
	echo "tag already exists on ${REMOTE}: ${VERSION}" >&2
	exit 1
fi

git fetch "$REMOTE" "$BASE_BRANCH" "$RELEASE_BRANCH" --tags

if ! git show-ref --verify --quiet "refs/heads/${RELEASE_BRANCH}"; then
	echo "local branch not found: ${RELEASE_BRANCH}" >&2
	exit 1
fi

if ! git show-ref --verify --quiet "refs/remotes/${REMOTE}/${BASE_BRANCH}"; then
	echo "remote branch not found: ${REMOTE}/${BASE_BRANCH}" >&2
	exit 1
fi

if ! git show-ref --verify --quiet "refs/remotes/${REMOTE}/${RELEASE_BRANCH}"; then
	echo "remote branch not found: ${REMOTE}/${RELEASE_BRANCH}" >&2
	exit 1
fi

git checkout "$RELEASE_BRANCH"
git merge --ff-only "${REMOTE}/${RELEASE_BRANCH}"
git merge --ff-only "${REMOTE}/${BASE_BRANCH}"

if [ "$NO_PUSH" -eq 0 ]; then
	git push "$REMOTE" "$RELEASE_BRANCH"
fi

git tag "$VERSION"

if [ "$NO_PUSH" -eq 0 ]; then
	git push "$REMOTE" "$VERSION"
	echo "release pushed: branch ${RELEASE_BRANCH}, tag ${VERSION}"
	echo "GitHub Actions will publish release assets for ${VERSION}."
else
	echo "local release prepared: branch ${RELEASE_BRANCH}, tag ${VERSION}"
	echo "next steps:"
	echo "  git push ${REMOTE} ${RELEASE_BRANCH}"
	echo "  git push ${REMOTE} ${VERSION}"
fi
