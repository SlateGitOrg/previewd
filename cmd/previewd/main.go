// Command previewd runs the preview-environment controller. See
// `previewd -h`.
package main

import (
	"os"

	"github.com/SlateGitOrg/previewd/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.IO{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Getenv: os.Getenv,
	}))
}
