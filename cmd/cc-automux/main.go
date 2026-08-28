package main

import (
	"fmt"
	"io"
	"os"

	"github.com/Siriusrry/cc-automux/internal/shim"
	productversion "github.com/Siriusrry/cc-automux/internal/version"
)

func main() {
	handled, exitCode := handleArgs(os.Args[1:], os.Stdout, os.Stderr)
	if handled {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
		return
	}
	shim.Main()
}

func handleArgs(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	if len(args) == 1 && args[0] == "--version" {
		_, _ = fmt.Fprintln(stdout, productversion.Display())
		return true, 0
	}
	_, _ = fmt.Fprintln(stderr, "Usage: cc-automux [--version]")
	return true, 2
}
