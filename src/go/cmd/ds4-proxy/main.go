// Command ds4-proxy is the Go reimplementation of src/proxy.py.
//
// Task 1: --ports output and the CLI shape. The server bootstrap (serve(),
// rewrite, classifier, idle watch) lands in Task 4.
package main

import (
	"fmt"
	"os"

	"github.com/strml/cc-ds4/src/go/internal/profiles"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--ports" {
		// Python's main() guards on empty served before the --ports branch,
		// exiting 1 with this message on stderr. Mirror it for exit-code
		// parity — an absent set of profile dirs means nothing to serve.
		if len(profiles.Served()) == 0 {
			fmt.Fprintln(os.Stderr, "no profile directories found; nothing to serve")
			os.Exit(1)
		}
		fmt.Print(profiles.Ports())
		return
	}
	// server bootstrap comes in Task 4
}
