package main

import (
	"context"
	"os"
	"runtime/debug"

	"github.com/wxia529/joyrun/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], resolvedVersion()))
}

func resolvedVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok &&
		info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}
