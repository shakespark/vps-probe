#!/bin/sh
# Checks the GitHub release CI built for tag v<VERSION> against a local
# rebuild of the same tag (docs/DESIGN.md §10).
#
#   scripts/release.sh verify   download, rebuild, compare
#   scripts/release.sh sign     the same, then sign the checksum file with the
#                               offline release key, upload the signature and
#                               publish the draft
#
# If the key has a passphrase, run sign in a terminal: ssh-keygen asks for it.
set -eu
cd "$(dirname "$0")/.."

mode=${1:-}
case $mode in
verify | sign) ;;
*)
	echo "usage: $0 verify|sign" >&2
	exit 2
	;;
esac

version=${VERSION:-$(cat VERSION)}
tag=v$version
sums=vps-probe-$version.sha256
key=${SIGNING_KEY:-$HOME/.ssh/vps-probe-release}
signers=${SIGNERS:-internal/release/release-signers}
ns=vps-probe-release

die() {
	echo "release: $*" >&2
	exit 1
}
say() { echo "release: $*"; }

work=$(mktemp -d)
cleanup() {
	git worktree remove --force "$work/src" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

# Signing prerequisites first, so a missing key fails before the slow part.
if [ "$mode" = sign ]; then
	[ -f "$key" ] && [ -f "$key.pub" ] || die "no signing key at $key (and $key.pub)"
	pub=$(awk '{ print $1, $2 }' "$key.pub")
	principal=$(awk -v k="$pub" 'index($0, k) { print $1; exit }' "$signers")
	[ -n "$principal" ] || die "$key.pub is not listed in $signers"
fi

# The tag must be the same commit here and on GitHub, or we would vouch for
# a build of something else.
commit=$(git rev-parse --verify --quiet "$tag^{commit}") || die "no local tag $tag"
remote=$(git ls-remote origin "refs/tags/$tag" "refs/tags/$tag^{}" | awk 'END { print $1 }')
[ -n "$remote" ] || die "tag $tag is not on origin"
[ "$remote" = "$commit" ] || die "tag $tag is $commit here but $remote on origin"

draft=$(gh release view "$tag" --json isDraft --jq .isDraft 2>/dev/null) ||
	die "no GitHub release for $tag yet (is the release workflow still running?)"
if [ "$mode" = sign ] && [ "$draft" != true ]; then
	die "release $tag is already published"
fi

say "downloading the $tag release"
gh release download "$tag" -D "$work/ci" -p "vps-probe-$version-linux-*.tar.gz" -p "$sums"
(cd "$work/ci" && sha256sum --strict --quiet -c "$sums") || die "$sums does not match the downloaded files"

say "rebuilding $tag ($commit) locally"
git worktree add --quiet --detach "$work/src" "$commit"
make -C "$work/src" --no-print-directory dist VERSION="$version" >"$work/build.log" 2>&1 ||
	{ cat "$work/build.log" >&2; die "local build failed"; }

# Same set of tarballs on both sides.
ls "$work/ci" | grep '\.tar\.gz$' | LC_ALL=C sort >"$work/ci.list"
(cd "$work/src/dist" && ls vps-probe-"$version"-linux-*.tar.gz) | LC_ALL=C sort >"$work/local.list"
awk '{ print $2 }' "$work/ci/$sums" | LC_ALL=C sort >"$work/sums.list"
cmp -s "$work/ci.list" "$work/local.list" && cmp -s "$work/ci.list" "$work/sums.list" ||
	die "tarball sets differ: CI $(tr '\n' ' ' <"$work/ci.list")/ local $(tr '\n' ' ' <"$work/local.list")"

# Tarballs are deterministic, so the uncompressed streams must match byte for
# byte (file list, modes, owners, mtimes, contents). The gzip layer may differ
# between gzip versions without meaning anything.
while read -r f; do
	if cmp -s "$work/ci/$f" "$work/src/dist/$f"; then
		say "identical: $f"
		continue
	fi
	gzip -dc "$work/ci/$f" >"$work/ci.tar"
	gzip -dc "$work/src/dist/$f" >"$work/local.tar"
	if cmp -s "$work/ci.tar" "$work/local.tar"; then
		say "identical contents (gzip framing differs): $f"
		continue
	fi
	tar -tvf "$work/ci.tar" >"$work/ci.tv"
	tar -tvf "$work/local.tar" >"$work/local.tv"
	diff -u "$work/ci.tv" "$work/local.tv" >&2 || true
	die "$f: the CI build differs from the local rebuild"
done <"$work/ci.list"
say "the CI artifacts for $tag match a local rebuild of $commit"

[ "$mode" = sign ] || exit 0

ssh-keygen -Y sign -f "$key" -n "$ns" "$work/ci/$sums"
ssh-keygen -Y verify -f "$signers" -I "$principal" -n "$ns" -s "$work/ci/$sums.sig" <"$work/ci/$sums" >/dev/null ||
	die "the new signature does not verify against $signers"
gh release upload "$tag" "$work/ci/$sums.sig" --clobber
gh release edit "$tag" --draft=false >/dev/null
say "published $tag with $sums.sig"
