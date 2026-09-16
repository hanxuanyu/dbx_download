// Command dbxdl downloads every DBX architecture package plus the configured
// plugins into a dbx<version>/ directory and packs the result into one tar
// file, reproducing the reference offline-bundle layout.
package main

import (
	"context"
	"os"

	"dbxdl/internal/cli"
)

func main() {
	env := cli.Env{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Stdin:  os.Stdin,
		Args:   os.Args[1:],
	}
	os.Exit(cli.Run(context.Background(), env))
}
