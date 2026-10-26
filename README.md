# go-i2p-release

Release harness for the [go-i2p](https://github.com/go-i2p) namespace. This tool automates the process of updating all go-i2p repositories to the latest commit, tagging them uniformly, and pushing a fully up-to-date release across the entire namespace.

## Usage

```bash
# Release all repositories at version v0.1.7
./go-i2p-release v0.1.7 /path/to/go-i2p-namespace

# Or use the VERSION environment variable
VERSION=v0.1.7 ./go-i2p-release /path/to/go-i2p-namespace

# Dry run (show what would happen without pushing)
DRY_RUN=true ./go-i2p-release v0.1.7 /path/to/go-i2p-namespace

# Only release libraries, not clients
LIBS=true ./go-i2p-release v0.1.7 /path/to/go-i2p-namespace
```

## How it works

The release harness performs the following steps in order:

1. **Sync all repos to latest** - Fetches and pulls the latest changes from origin for all go-i2p repositories
2. **Collect commit hashes** - Records the HEAD commit hash of each repository
3. **Update go module dependencies** - Updates all repositories to depend on the latest commits of their go-i2p dependencies using `go get` and `go mod tidy`
4. **Tag all repos** - Creates an annotated tag at the specified version for each repository
5. **Push all repos** - Pushes both the main branch and version tags for all repositories
6. **Update to tagged version** - Updates all repositories to use the newly tagged version (rather than commit hashes), commits the changes, and pushes

## Repository order

The release order is carefully maintained to ensure dependencies are released before their dependents:

- Layer 1: Core libraries (logger, elgamal, su3, crypto)
- Layer 2: Common structures (common)
- Layer 3: Network layer (noise, go-nat-listener, path, pool, go-noise)
- Layer 4: Router and clients (go-i2p, go-i2cp)
- Layer 5: Application layer (go-datagrams, go-streaming, go-sam-bridge, i2p-control, i2ptui)

## Requirements

- Go 1.21+
- git
- Network access to GitHub and go module proxies
