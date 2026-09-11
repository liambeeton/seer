// Command seer visits a set of Targets once and keeps a Screenshot and the
// server's Response for each.
package main

import (
	"context"
	"os"

	"github.com/liambeeton/seer/internal/cli"
)

func main() {
	os.Exit(cli.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
