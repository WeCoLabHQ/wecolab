#!/usr/bin/env bash
# Publish main to the public repository, through the security and privacy gate.
#
#   hack/publish.sh "What changed"          check only
#   hack/publish.sh --push "What changed"   check, then push to the remote named public
#
# The public repository gets snapshots: each publish is one commit holding main's tree, with the message
# given here and the public author ($WECOLAB_PUBLIC_AUTHOR or `git config wecolab.publicAuthor`, as
# "Name <email>"), so the private history (its messages, authors and every tree before this one) never
# leaves. Nothing is pushed unless every check passes:
#   1. main is committed and pushed to the private repository (origin/main), so what is published is
#      recorded there;
#   2. no secrets: gitleaks over the tree, the message and the author;
#   3. nothing private: no line of $WECOLAB_PRIVATE_TERMS (default ~/.config/wecolab/private-terms: fixed
#      strings, matched without regard to case; # starts a comment) in any file, file name, the message
#      or the author. The list lives outside the repository because it is private itself;
#   4. no file the public has no use for: private keys, age or SOPS key files, .env files, anything over
#      5 MB.
set -euo pipefail
cd "$(dirname "$0")/.."
fail() { printf 'publish: %s\n' "$*" >&2; exit 1; }
push=""; [ "${1:-}" = --push ] && { push=1; shift; }
msg=${1:?usage: hack/publish.sh [--push] "message"}
author=${WECOLAB_PUBLIC_AUTHOR:-$(git config --get wecolab.publicAuthor || true)}
[ -n "$author" ] || fail 'no public author: git config wecolab.publicAuthor "Name <email>" (kept out of the repository)'

terms=${WECOLAB_PRIVATE_TERMS:-$HOME/.config/wecolab/private-terms}
command -v gitleaks >/dev/null || fail "gitleaks is not installed (brew install gitleaks)"
[ -r "$terms" ] || fail "no private terms at $terms: without them the privacy check would pass anything"
[[ $author =~ ^([^<>]+)\ \<([^<>@\ ]+@[^<>\ ]+)\>$ ]] || fail "WECOLAB_PUBLIC_AUTHOR must read: Name <email>"
name=${BASH_REMATCH[1]} email=${BASH_REMATCH[2]}

# 1. What is published is main as the private repository has it.
[ -z "$(git status --porcelain)" ] || fail "commit or stash first: the tree has changes"
[ "$(git branch --show-current)" = main ] || fail "publish from main"
git fetch -q origin main
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] || fail "push main to origin first"

work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
mkdir "$work/tree" "$work/meta"
git archive HEAD | tar -x -C "$work/tree"
printf '%s\n' "$msg" > "$work/meta/message"
printf '%s\n' "$author" > "$work/meta/author"
grep -v -e '^[[:space:]]*#' -e '^[[:space:]]*$' "$terms" > "$work/terms" || fail "$terms lists no terms"
bad=0

# 2. Secrets.
for d in tree meta; do
  gitleaks dir --no-banner --redact --log-level warn "$work/$d" || { echo "  secrets: gitleaks found the above in the $d" >&2; bad=1; }
done

# 3. Private terms, in contents, names, the message and the author.
if hits=$(cd "$work" && grep -r -n -i -F -f terms tree meta); then
  printf '  private terms:\n%s\n' "$(sed 's/^/    /' <<<"$hits" | cut -c1-200)" >&2; bad=1
fi
if hits=$(cd "$work/tree" && find . | grep -i -F -f ../terms); then
  printf '  private terms in file names:\n%s\n' "$(sed 's/^/    /' <<<"$hits")" >&2; bad=1
fi

# 4. Files the public has no use for.
if hits=$(cd "$work/tree" && find . -type f \( -name '*.pem' -o -name '*.key' -o -name 'id_rsa*' -o -name 'id_ed25519*' \
    -o -name '*.age' -o -name 'keys.txt' -o -name '.env' -o -name '.env.*' -o -size +5M \) | sort); then
  [ -z "$hits" ] || { printf '  files that should not be public:\n%s\n' "$(sed 's/^/    /' <<<"$hits")" >&2; bad=1; }
fi

[ $bad = 0 ] || fail "the gate refused: nothing was published"
tree=$(git rev-parse 'HEAD^{tree}')
echo "publish: the gate passed for main at $(git rev-parse --short HEAD)"
[ -n "$push" ] || { echo "publish: dry run; add --push to publish"; exit 0; }

git remote get-url public >/dev/null 2>&1 || fail "no remote named public"
parent=()
if git fetch -q public main 2>/dev/null; then
  [ "$(git rev-parse 'FETCH_HEAD^{tree}')" != "$tree" ] || { echo "publish: the public repository has this tree already"; exit 0; }
  parent=(-p FETCH_HEAD)
fi
sha=$(GIT_AUTHOR_NAME=$name GIT_AUTHOR_EMAIL=$email GIT_COMMITTER_NAME=$name GIT_COMMITTER_EMAIL=$email \
  git commit-tree "$tree" ${parent[@]+"${parent[@]}"} -F "$work/meta/message")
git push -q public "$sha:refs/heads/main"
echo "publish: pushed $sha to $(git remote get-url public)"
