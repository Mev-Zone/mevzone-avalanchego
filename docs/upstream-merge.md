# Merging upstream avalanchego

This fork (`Mev-Zone/mevzone-avalanchego`) tracks
[`ava-labs/avalanchego`](https://github.com/ava-labs/avalanchego). The MEV
changes are a **thin layer** — historically ~15 files, almost all *new* files
under `graft/coreth/mev/*`, `graft/coreth/miner/bid_simulator*.go`, plus small
edits to a handful of shared files (`graft/coreth/plugin/evm/vm.go`,
`.../config/config.go`, `graft/coreth/miner/{miner,worker}.go`).

Because the fork is so small, merges *should* be cheap. They get expensive only
when the fork drifts far from upstream. This doc explains how to keep them cheap.

## Golden rules

1. **Merge upstream release *tags* directly** (`git merge <tag>`). Never
   cherry-pick or rebase-recreate upstream commits — that gives your "copy" a
   different commit hash than upstream's, git loses the shared ancestry, and the
   *next* merge sees hundreds of commits as divergent. (This is exactly what
   made the `v1.15.0-fuji` merge cost 361 conflicts instead of ~15.)
2. **Merge every release, promptly.** Small, frequent merges = small conflict
   sets. The weekly [drift check](#drift-detection) tells you when a new tag is
   out.
3. **Pick one upstream line and stay on it.** This fork follows the `-fuji`
   release line. Hopping between divergent lines multiplies conflicts *and* can
   silently revert fixes.
4. **Keep the MEV footprint in new files.** New files never conflict. Prefer
   coreth extension points (`extension.LeafRequestConfig`, `extensionConfig`,
   etc.) over editing core files inline.

## The merge procedure

```bash
# 1. Make sure the upstream remote exists and fetch the tag.
git remote get-url upstream || git remote add upstream https://github.com/ava-labs/avalanchego.git
git fetch upstream --tags

# 2. From a clean tree on the fork branch, start the merge.
git switch mevzone && git status   # must be clean
git merge <upstream-tag>           # e.g. v1.15.0-fuji
```

### Resolving conflicts

The strategy that works, given how thin the fork is:

- **Accept upstream ("theirs") for everything except the files the fork
  actually changed.** Find the fork's real delta with:
  ```bash
  git diff --name-only <last-merged-upstream-tag>..HEAD
  ```
  Everything outside that list is pure upstream — take theirs.
  ```bash
  # bulk-accept theirs for a conflicted file
  git checkout --theirs -- <file> && git add -- <file>
  # for upstream deletions (UD/DU status), accept the delete
  git rm -- <file>
  ```
- **Hand-merge only the fork-changed files.** Usually just `vm.go` /
  `config.go` / `miner`. Preserve the MEV hooks; take upstream's changes around
  them.
- **Never hand-merge generated files — regenerate them.** BUILD.bazel, mocks,
  contract bindings (`gen_*_binding.go`), and `.bin` artifacts. Resolve them to
  theirs to clear the conflict, then regenerate (below).

### Regenerate derived files

```bash
task bazel-generate-metadata   # rewrites all BUILD.bazel (gazelle + patches)
go generate ./...              # mocks (go tool mockgen); bindings need solc
go work sync                   # align workspace module deps
```

> `task`/`bazelisk` live in the nix dev shell — run these from `nix develop`
> (see `docs/bazel.md`). Contract bindings additionally need `solc`; skip if the
> fork touched no contracts.

### Verify

The real coherence check when Bazel can't run locally — build **all four**
workspace modules:

```bash
go build ./... \
  && (cd graft/coreth && go build ./...) \
  && (cd graft/evm && go build ./...) \
  && (cd graft/subnet-evm && go build ./...)
```

Then run the relevant tests before committing the merge.

## Drift detection

[`scripts/check_upstream_drift.sh`](../scripts/check_upstream_drift.sh) reports
how far the fork is behind the latest upstream release and the conflict cost of
catching up. It runs weekly via
[`.github/workflows/upstream-drift.yml`](../.github/workflows/upstream-drift.yml),
which opens/updates an `upstream-drift` issue when a new tag is unmerged. Run it
locally any time:

```bash
./scripts/check_upstream_drift.sh            # check latest release
./scripts/check_upstream_drift.sh <tag>      # cost of a specific tag
```

Keep that conflict number small by following the golden rules above.
