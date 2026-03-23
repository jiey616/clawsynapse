#!/usr/bin/env bash

set -euo pipefail

DEFAULT_REPO="yuanjun5681/clawsynapse"

usage() {
	cat <<'EOF'
Build and publish a ClawSynapse GitHub Release.

Usage:
  ./scripts/release.sh --version v0.0.4

Options:
  --version VERSION    Release tag to build and publish
  --repo OWNER/REPO    GitHub repository slug (default: yuanjun5681/clawsynapse)
  --draft              Create or update a draft release
  --prerelease         Mark the release as a prerelease
  --skip-publish       Only build dist/, checksums, and release notes
  -h, --help           Show this help
EOF
}

VERSION=""
REPO="$DEFAULT_REPO"
DRAFT=0
PRERELEASE=0
SKIP_PUBLISH=0

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version)
			VERSION="${2:-}"
			shift 2
			;;
		--repo)
			REPO="${2:-}"
			shift 2
			;;
		--draft)
			DRAFT=1
			shift
			;;
		--prerelease)
			PRERELEASE=1
			shift
			;;
		--skip-publish)
			SKIP_PUBLISH=1
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			echo "unknown option: $1" >&2
			usage >&2
			exit 1
			;;
	esac
done

if [ -z "$VERSION" ]; then
	VERSION="$(git describe --tags --exact-match 2>/dev/null || true)"
fi

if [ -z "$VERSION" ]; then
	echo "release version is required; pass --version <tag>" >&2
	exit 1
fi

if ! git rev-parse -q --verify "refs/tags/${VERSION}" >/dev/null 2>&1; then
	echo "tag not found: ${VERSION}" >&2
	exit 1
fi

make release-prep VERSION="$VERSION"

RELEASE_NOTES_FILE="dist/release-notes-${VERSION}.md"

if [ "$SKIP_PUBLISH" -eq 1 ]; then
	echo "release artifacts prepared in dist/"
	echo "notes: ${RELEASE_NOTES_FILE}"
	exit 0
fi

if ! command -v gh >/dev/null 2>&1; then
	echo "gh CLI is required to publish releases" >&2
	exit 1
fi

files=()
while IFS= read -r file; do
	files+=("$file")
done < <(find dist -maxdepth 1 -type f ! -name 'release-notes-*' | sort)

if [ "${#files[@]}" -eq 0 ]; then
	echo "no release assets found in dist/" >&2
	exit 1
fi

common_flags=(--repo "$REPO" --title "$VERSION" --notes-file "$RELEASE_NOTES_FILE")
if [ "$DRAFT" -eq 1 ]; then
	common_flags+=(--draft)
fi
if [ "$PRERELEASE" -eq 1 ]; then
	common_flags+=(--prerelease)
fi

if gh release view "$VERSION" --repo "$REPO" >/dev/null 2>&1; then
	gh release upload "$VERSION" --repo "$REPO" --clobber "${files[@]}"
	gh release edit "$VERSION" "${common_flags[@]}"
else
	gh release create "$VERSION" "${files[@]}" "${common_flags[@]}"
fi

echo "published release ${VERSION} to ${REPO}"
