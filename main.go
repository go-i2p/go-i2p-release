package main

import (
	"os"
	"runtime/debug"

	"github.com/go-i2p/go-i2p-release/internal/cli"
)

// version is stamped with -ldflags "-X main.version=..." at build time. When the
// binary was installed with `go install`, the module build info is used instead.
var version = "dev"

func main() {
	os.Exit(cli.Execute(resolveVersion(version)))
}

func resolveVersion(v string) string {
	if v != "dev" {
		return v
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return v
	}
	return info.Main.Version
}
