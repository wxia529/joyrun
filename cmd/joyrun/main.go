package main

import (
	"context"
	"os"

	"github.com/wxia529/joyrun/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], version))
}
