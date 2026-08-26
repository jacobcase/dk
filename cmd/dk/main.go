// Command dk is a DigiKey CLI for searching parts and building order lists.
package main

import (
	"os"

	"github.com/jacobcase/dk/internal/cli"
)

func main() {
	os.Exit(cli.Main())
}
