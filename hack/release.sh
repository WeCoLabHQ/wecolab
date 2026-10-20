#!/usr/bin/env bash
# Construct a candidate from the checked-out immutable public snapshot, or verify a downloaded release.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
fail() { printf 'release: %s\n' "$*" >&2; exit 1; }
repo=wecolabhq/wecolab
workflow="WeCoLabHQ/wecolab/.github/workflows/release.yml"
sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
source_identity() {
  local commit=$1 identity
  [ "$(git -C "$root" rev-parse HEAD)" = "$commit" ] || fail 'checkout differs from authenticated source snapshot'
  [ -z "$(git -C "$root" status --porcelain)" ] || fail 'source checkout must be clean for an official candidate'
  command -v python3 >/dev/null || fail 'Python 3 required for build identity'
  identity=$(python3 "$root/hack/build-info.py" --root "$root" --format json) || fail 'complete Git history and valid build identity required'
  jq -e --arg commit "$commit" '.sourceRevision == $commit and .dirty == false' <<<"$identity" >/dev/null || fail 'source identity disagrees with clean snapshot'
  printf '%s\n' "$identity"
}
manifest_identity_check() {
  local manifest=$1 identity=$2
  jq -e --argjson identity "$identity" '
    .productVersion == $identity.productVersion and
    .buildNumber == $identity.buildNumber and
    .sourceRevision == $identity.sourceRevision and
    .dirty == $identity.dirty and
    .sourceCommit == $identity.sourceRevision
  ' "$manifest" >/dev/null || fail 'release identity disagrees with authenticated source'
}
archive_version_check() {
  local dir=$1 archive_version
  archive_version=$(tar -xOzf "$dir/source.tar.gz" VERSION) || fail 'source archive is missing root VERSION'
  jq -e --arg product "$archive_version" '.productVersion == $product' "$dir/release.json" >/dev/null || fail 'release product version disagrees with authenticated source archive'
}
manifest_check() {
  local dir=$1 manifest file expected actual
  manifest=$dir/release.json
  [ -f "$manifest" ] || fail 'missing manifest'
  jq -e --arg repo "$repo" '.schema == 1 and .repository == $repo and (.sourceCommit | test("^[0-9a-f]{40}$")) and (.sourceRevision == .sourceCommit) and (.productVersion | type == "string") and (.buildNumber | type == "number" and . > 0 and . == floor) and (.dirty == false) and .apiVersion == "wecolab.io/v1alpha1" and (.migrations == ["archive-id", "vault-key-version", "name-claims", "placement-revision"]) and (.artifacts | length == 7) and ([.artifacts[] | .name] | unique | length == 7)' "$manifest" >/dev/null || fail 'invalid release manifest'
  for file in install.sh VERSION source.tar.gz warden-amd64 warden-arm64 console-amd64 console-arm64; do
    expected=$(jq -er --arg file "$file" '.artifacts[] | select(.name == $file) | .sha256 | select(test("^[a-f0-9]{64}$"))' "$manifest") || fail "missing $file/platform"
    [ -f "$dir/$file" ] && [ ! -L "$dir/$file" ] || fail "missing or unsafe $file"
    actual=$(sha "$dir/$file")
    [ "$expected" = "$actual" ] || fail "digest mismatch for $file"
  done
  jq -e '[.artifacts[] | select(.name | test("^(warden|console)-(amd64|arm64)$")) | .platform] | sort == ["linux/amd64", "linux/amd64", "linux/arm64", "linux/arm64"]' "$manifest" >/dev/null || fail 'unsupported/missing platform'
}
case ${1:-} in
  build)
    [ $# -eq 3 ] || fail 'usage: hack/release.sh build SOURCE_COMMIT OUTPUT_DIR'
    commit=$2 out=$3
    [[ $commit =~ ^[a-f0-9]{40}$ ]] || fail 'source must be the immutable public snapshot SHA'
    identity=$(source_identity "$commit")
    cmp "$root/install.sh" "$root/cmd/console/join.sh" || fail 'installer embed is stale'
    build_flags=$(python3 "$root/hack/build-info.py" --root "$root" --format go-ldflags) || fail 'cannot stamp release binaries'
    mkdir -p "$out"
    cp "$root/install.sh" "$out/install.sh"
    printf '%s\n' "${commit:0:12}" > "$out/VERSION"
    git -C "$root" archive --format=tar HEAD | gzip -n > "$out/source.tar.gz"
    for arch in amd64 arm64; do
      for binary in warden console; do
        (cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.Version=${commit:0:12} $build_flags" -o "$out/$binary-$arch" "./cmd/$binary")
      done
    done
    jq -n --argjson identity "$identity" --arg commit "$commit" --arg repo "$repo" --arg installer "$(sha "$out/install.sh")" --arg version "$(sha "$out/VERSION")" --arg source "$(sha "$out/source.tar.gz")" --arg wa "$(sha "$out/warden-amd64")" --arg wr "$(sha "$out/warden-arm64")" --arg ca "$(sha "$out/console-amd64")" --arg cr "$(sha "$out/console-arm64")" '{
      schema:1,repository:$repo,sourceCommit:$commit,
      productVersion:$identity.productVersion,buildNumber:$identity.buildNumber,
      sourceRevision:$identity.sourceRevision,dirty:$identity.dirty,
      apiVersion:"wecolab.io/v1alpha1",
      migrations:["archive-id","vault-key-version","name-claims","placement-revision"],
      upstream:{
        k3s:{version:"v1.36.4+k3s1",installerCommit:"4dedb15be78017a8ddd5b9e81acd44f3481078ed",installerSHA256:"46177d4c99440b4c0311b67233823a8e8a2fc09693f6c89af1a7161e152fbfad"},
        nebula:"v1.11.2",sops:"v3.13.3",netbird:"v0.80.0"
      },
      artifacts:[
        {name:"install.sh",platform:"linux/amd64,linux/arm64",sha256:$installer},
        {name:"VERSION",platform:"linux/amd64,linux/arm64",sha256:$version},
        {name:"source.tar.gz",platform:"source",sha256:$source},
        {name:"warden-amd64",platform:"linux/amd64",sha256:$wa},
        {name:"warden-arm64",platform:"linux/arm64",sha256:$wr},
        {name:"console-amd64",platform:"linux/amd64",sha256:$ca},
        {name:"console-arm64",platform:"linux/arm64",sha256:$cr}
      ]
    }' > "$out/release.json"
    manifest_check "$out"
    manifest_identity_check "$out/release.json" "$identity"
    ;;
  verify)
    [ $# -eq 3 ] || fail 'usage: hack/release.sh verify RELEASE_DIR SOURCE_COMMIT'
    dir=$2 commit=$3
    [[ $commit =~ ^[a-f0-9]{40}$ ]] || fail 'expected source commit required'
    manifest_check "$dir"
    [ "$(jq -r .sourceCommit "$dir/release.json")" = "$commit" ] || fail 'wrong source commit'
    command -v gh >/dev/null || fail 'GitHub CLI required for authenticated attestation verification'
    gh attestation verify "$dir/release.json" --repo "$repo" --signer-workflow "$workflow" --source-ref refs/heads/main --source-digest "$commit" --deny-self-hosted-runners >/dev/null || fail 'untrusted release provenance'
    archive_version_check "$dir"
    printf 'release: authenticated manifest and all seven artifacts verified; only now may installer run as root\n'
    ;;
  *) fail 'usage: hack/release.sh build SOURCE_COMMIT OUTPUT_DIR | verify RELEASE_DIR SOURCE_COMMIT' ;;
esac
