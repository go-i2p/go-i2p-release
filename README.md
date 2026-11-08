# go-i2p-release

Release harness for the [go-i2p](https://github.com/go-i2p) namespace. It updates
every repository in the namespace to the newest commit of its peers, verifies that
the result builds and tests clean, and checks it in — then, when you are ready,
tags the whole namespace uniformly and pushes a coherent release.

## Install

```bash
go install github.com/go-i2p/go-i2p-release@latest
```

Or build from a clone:

```bash
go build -o go-i2p-release .
```

## Usage

The bare command performs a **tip check-in**: dependencies are bumped to their
branch tips, verified, committed and pushed, and **nothing is tagged**. That is the
default mode, and it is the safe half of a release — land the tips first, then tag
once everything is green.

```bash
# Default: bump to tips, verify (build + test), commit and push. No tags.
go-i2p-release

# Preview the whole run without touching any repository
go-i2p-release --dry-run

# Commit locally only, leaving the push to you
go-i2p-release --push=false

# Tag the entire namespace, pushing branches and tags
go-i2p-release release v0.1.7

# Also rewrite dependencies from pseudo-versions to the tag, then check that in
go-i2p-release release v0.1.7 --post-tag-bump

# Print the resolved release set, order and in-namespace dependencies
go-i2p-release plan
```

The namespace defaults to `$GOPATH/src/github.com/go-i2p`, so no path argument is
needed in the usual layout. Pass `--namespace` to point somewhere else.

### Selecting repositories

```bash
go-i2p-release --libs      # library layers only (everything below go-i2p)
go-i2p-release --clients   # router, clients, tunnels and tools only
go-i2p-release --only logger,common
go-i2p-release --exclude i2ptui
go-i2p-release --all       # every module under the namespace, not just the known set
```

### Configuration

Values resolve in this order: explicit flag, environment variable, config file,
built-in default.

| Flag | Environment | Default | Purpose |
| --- | --- | --- | --- |
| `--namespace` | `GO_I2P_RELEASE_NAMESPACE`, `GO_I2P_DIR` | `$GOPATH/src/github.com/go-i2p` | directory holding the repositories |
| `--prefix` | `GO_I2P_RELEASE_PREFIX` | `github.com/go-i2p` | module prefix shared by the namespace |
| `--dry-run` | `GO_I2P_RELEASE_DRY_RUN`, `DRY_RUN` | `false` | report changes without making them |
| `--push` | `GO_I2P_RELEASE_PUSH` | `true` | publish commits and tags to origin |
| `--verify` | `GO_I2P_RELEASE_VERIFY` | `test` | `none`, `build`, or `test` after bumping |
| `--sync` | `GO_I2P_RELEASE_SYNC` | `true` | fetch and fast-forward before computing tips |
| `--allow-dirty` | `GO_I2P_RELEASE_ALLOW_DIRTY` | `false` | proceed with tracked local changes |
| `--branch` | `GO_I2P_RELEASE_BRANCH` | detected | override each repository's default branch |
| `--message` | `GO_I2P_RELEASE_MESSAGE` | see source | commit message for check-ins |
| `--tag-existing` | `GO_I2P_RELEASE_TAG_EXISTING` | `fail` | `fail`, `skip`, or `recreate` |
| `--post-tag-bump` | `GO_I2P_RELEASE_POST_TAG_BUMP` | `false` | rewrite deps to the tag after tagging |
| `--standalone` | `GO_I2P_RELEASE_STANDALONE` | `true` | resolve each module on its own (`GOWORK=off`) |
| `--private` | `GO_I2P_RELEASE_PRIVATE` | `true` | bypass the proxy for namespace modules |
| `--git-timeout` / `--go-timeout` | `GO_I2P_RELEASE_GIT_TIMEOUT` / `_GO_TIMEOUT` | `2m` / `15m` | per-command timeouts |
| `-v`, `--verbose` | `GO_I2P_RELEASE_VERBOSE` | `false` | echo the output of every command |
| `--quiet` | `GO_I2P_RELEASE_QUIET` | `false` | suppress progress lines |

A documented starting file is written by:

```bash
go-i2p-release init-config          # ./go-i2p-release.yaml
go-i2p-release init-config --stdout
```

The config file is searched for as `go-i2p-release.yaml` in the working directory,
the namespace, `~/.config`, `$HOME`, and `$XDG_CONFIG_HOME/go-i2p-release`.

The pre-cobra environment variables `VERSION`, `DRY_RUN`, `LIBS` and `GO_I2P_DIR`
are still honoured.

## How it works

1. **Discover** — reads each repository's `go.mod` with `golang.org/x/mod/modfile`
   and builds the in-namespace dependency graph.
2. **Order** — topologically sorts that graph so a dependency always precedes its
   dependents. The layer table below is only a tie-breaker, so the order follows
   what the modules actually require rather than a hand-maintained list.
3. **Inspect** — detects each repository's default branch (from `origin/HEAD`,
   falling back to the checked-out branch, then `main`/`master`/`trunk`), fetches
   origin, records the tip, and refuses to run on tracked local changes unless
   `--allow-dirty` is passed. Untracked files never block a run.
4. **Sync** — fast-forwards clean working trees onto their tip.
5. **Bump** — drops local `replace` directives, then `go get`s each in-namespace
   dependency at its origin tip and runs `go mod tidy`.
6. **Verify** — `go build ./...`, and `go test ./...` by default. A failure stops
   the run before anything is committed or tagged, so a broken tip never reaches
   the shared namespace.
7. **Check in** — commits `go.mod`/`go.sum` in each repository that changed.
8. **Push** — publishes each repository's default branch.
9. **Tag** (release only) — creates an annotated tag in every repository and pushes
   it. Existing tags are checked *before* any work begins, so a collision aborts
   with nothing changed.
10. **Post-tag bump** (`--post-tag-bump`) — rewrites dependencies from
    pseudo-versions to the new tag, verifies, checks in and pushes again, so the
    released tree references releases rather than commits.

## Notable behaviours

- **Bumping targets the origin tip, not local `HEAD`.** `go get` resolves commits
  over the network, so an unpushed local commit would fail to fetch.
- **`--standalone` (default) sets `GOWORK=off`.** A `go.work` in the namespace
  usually lists only some modules, which makes `go build ./...` fail in the others;
  pinning each module to itself keeps verification meaningful.
- **`--private` (default) sets `GOPRIVATE`** for the namespace prefix, so freshly
  pushed tips resolve straight from the host instead of waiting on the module proxy.
- **Local `replace` directives are dropped before bumping.** Otherwise the released
  module graph would not match what was tagged.
- **Major versions are preflighted.** A `v2+` tag requires a `/vN` module path in
  every repository, which this namespace does not use; the run stops before pushing
  anything rather than leaving tags that cannot be required.
- **A run reports every failing command.** The exit message lists the concrete
  `git`/`go` invocations that broke, not just a headline error.
- Interrupting with Ctrl-C cancels in-flight commands instead of orphaning them.

## Repository order

Known layers, used as the default release set and as the topological tie-breaker.
Modules outside this list are included with `--all`.

- Layer 0: Core libraries (logger, elgamal, su3, crypto)
- Layer 1: Common structures (common)
- Layer 2: Network layer (noise, go-nat-listener, path, pool, go-noise)
- Layer 3: Router and protocol clients (go-i2p, go-i2cp)
- Layer 4: Application layer (go-datagrams, go-streaming, go-sam-bridge, i2p-control, i2ptui)

## Requirements

- Go 1.21+
- git
- Network access to GitHub and Go module proxies
