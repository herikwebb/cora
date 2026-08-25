package main

import (
	"os"

	"github.com/herikwebb/cora/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
