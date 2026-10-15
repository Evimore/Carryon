// Command carryon backs up what makes a Linux machine yours and restores it on a fresh install.
package main

import (
	"os"

	"github.com/Evimore/carryon/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
