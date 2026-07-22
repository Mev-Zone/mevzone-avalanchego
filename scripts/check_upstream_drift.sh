#!/usr/bin/env bash
# Detects how far this fork has drifted from upstream ava-labs/avalanchego
# and reports the conflict cost of merging upstream release tags that are
# not yet in HEAD. Performs a trial (aborted) merge of the newest unmerged
# tag and categorizes the conflicts. Emits a Markdown report to stdout and,
# when running in GitHub Actions, sets step outputs via $GITHUB_OUTPUT.
#
# Usage: scripts/check_upstream_drift.sh [target-tag]
#   target-tag  optional explicit upstream tag to test-merge
#               (default: newest unmerged tag matching $UPSTREAM_TAG_GLOB)
#
# Env:
#   UPSTREAM_URL          upstream git URL (default: ava-labs/avalanchego)
#   UPSTREAM_TAG_GLOB     glob of tags to consider (default: v*)
#   UPSTREAM_TAG_EXCLUDE  ERE of tags to drop (default: pre-release/rc & one-offs;
#                         the fork's "-fuji" release line is deliberately kept)
set -euo pipefail

UPSTREAM_URL="${UPSTREAM_URL:-https://github.com/ava-labs/avalanchego.git}"
TAG_GLOB="${UPSTREAM_TAG_GLOB:-v*}"
TAG_EXCLUDE="${UPSTREAM_TAG_EXCLUDE:--rc(\.|-|$)|backport|-gasprice|-post-upgrade|-startup}"
TARGET_TAG="${1:-}"

log()  { echo "$@" >&2; }
out()  { [ -n "${GITHUB_OUTPUT:-}" ] && echo "$1=$2" >> "$GITHUB_OUTPUT" || true; }

log "Fetching upstream tags from ${UPSTREAM_URL} ..."
git fetch --quiet "${UPSTREAM_URL}" "+refs/tags/*:refs/upstream-tags/*"

# Collect all matching upstream tags, and separately those not yet merged.
# The fork tracks a specific release line, so older mainline tags it never
# adopted will always be "unmerged" — those must NOT trigger a false alarm.
# Drift is judged solely against the *latest* upstream tag by version.
all_tags=""
unmerged=""
while read -r ref; do
  tag="${ref#refs/upstream-tags/}"
  # shellcheck disable=SC2254
  case "$tag" in $TAG_GLOB) ;; *) continue ;; esac
  if printf '%s' "$tag" | grep -Eq -e "$TAG_EXCLUDE"; then continue; fi
  sha="$(git rev-parse -q --verify "${ref}^{commit}" 2>/dev/null)" || continue
  all_tags="${all_tags}${tag}"$'\n'
  if ! git merge-base --is-ancestor "$sha" HEAD 2>/dev/null; then
    unmerged="${unmerged}${tag}"$'\n'
  fi
done < <(git for-each-ref --format='%(refname)' refs/upstream-tags/)

all_sorted="$(printf '%s' "$all_tags" | sed '/^$/d' | sort -V -r)"
unmerged_sorted="$(printf '%s' "$unmerged" | sed '/^$/d' | sort -V -r)"
n_unmerged="$(printf '%s\n' "$unmerged_sorted" | sed '/^$/d' | wc -l | tr -d ' ')"

# Target: explicit arg, else the single latest tag by version.
LATEST="$(printf '%s\n' "$all_sorted" | head -n1)"
TARGET="${TARGET_TAG:-$LATEST}"
TARGET_REF="refs/upstream-tags/${TARGET}"
if [ -z "$TARGET" ] || ! git rev-parse -q --verify "${TARGET_REF}^{commit}" >/dev/null 2>&1; then
  log "Target tag '${TARGET:-<none>}' not found upstream."; exit 1
fi

# If the latest tag is already merged, we are current — regardless of any
# older release-line tags the fork intentionally skipped.
if git merge-base --is-ancestor "${TARGET_REF}^{commit}" HEAD 2>/dev/null; then
  out drift false
  out target "$TARGET"
  out conflicts 0
  echo "## ✅ Upstream drift check — up to date"
  echo
  echo "The latest upstream tag \`${TARGET}\` (matching \`${TAG_GLOB}\`, excluding pre-releases) is already merged."
  if [ "$n_unmerged" -gt 0 ]; then
    echo
    echo "<sub>${n_unmerged} older release-line tag(s) remain unmerged by design; not treated as drift.</sub>"
  fi
  exit 0
fi

base="$(git merge-base HEAD "${TARGET_REF}")"
base_desc="$(git describe --tags --always "$base" 2>/dev/null || echo "$base")"
ahead="$(git rev-list --count "${base}..HEAD")"
behind="$(git rev-list --count "${base}..${TARGET_REF}")"

# Trial merge (never committed).
git merge --no-commit --no-ff "${TARGET_REF}" >/dev/null 2>&1 || true
conflicts="$(git diff --name-only --diff-filter=U | wc -l | tr -d ' ')"
bazel_c="$(git diff --name-only --diff-filter=U | grep -c 'BUILD\.bazel$' || true)"
moddel_c="$(git status --porcelain 2>/dev/null | grep -cE '^(UD|DU) ' || true)"
code_c=$(( conflicts - bazel_c ))
areas="$(git diff --name-only --diff-filter=U | awk -F/ '{if($1=="graft") print $1"/"$2; else if(NF>1) print $1"/"$2; else print $1}' | sort | uniq -c | sort -rn | head -12)"
# clean up the trial merge
git merge --abort >/dev/null 2>&1 || git reset --hard >/dev/null 2>&1 || true

out drift true
out target "$TARGET"
out conflicts "$conflicts"
out behind "$behind"

# Markdown report.
if [ "$conflicts" -eq 0 ]; then icon="🟢"; verdict="merges cleanly (no conflicts)";
elif [ "$conflicts" -le 30 ]; then icon="🟡"; verdict="small conflict set — merge soon";
else icon="🔴"; verdict="large conflict set — drift is accumulating"; fi

cat <<EOF
## ${icon} Upstream drift check

**Newest unmerged upstream tag:** \`${TARGET}\`
**Verdict:** ${verdict}

| Metric | Value |
|---|---|
| Trial-merge conflicts | **${conflicts}** |
| — \`BUILD.bazel\` (regenerable) | ${bazel_c} |
| — code / other | ${code_c} |
| — modify/delete | ${moddel_c} |
| Merge base | \`${base_desc}\` |
| Commits behind (base→${TARGET}) | ${behind} |
| Fork commits ahead (base→HEAD) | ${ahead} |
| Total unmerged upstream tags | ${n_unmerged} |

### All unmerged upstream tags (newest first)
\`\`\`
$(printf '%s\n' "$unmerged_sorted" | head -20)
\`\`\`

### Conflicts by area
\`\`\`
${areas}
\`\`\`

> Trial merge was aborted; no changes were committed. To merge for real:
> \`git merge ${TARGET}\` then resolve, regenerate Bazel metadata, and \`go build ./...\`.
> Merging every upstream release keeps this number small — see \`docs/upstream-merge.md\`.
EOF
