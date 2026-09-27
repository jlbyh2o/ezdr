// Command ezdr is the EZDR client and command-line interface for Proxmox VE
// hosts.
package main

import (
	"fmt"
	"os"

	"github.com/jlbyh2o/ezdr/internal/version"
)

const usage = `Usage: ezdr <command>

Commands:
  version   Print version information
  help      Show this help
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("ezdr", version.String())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "ezdr: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}
