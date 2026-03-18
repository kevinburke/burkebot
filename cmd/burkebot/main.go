package main

import (
	"fmt"
	"os"
)

const Version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: burkebot <subcommand> [flags]\n\nSubcommands:\n  dashboard    Start the web dashboard\n")
		os.Exit(2)
	}

	switch os.Args[1] {
	case "dashboard":
		runDashboard(os.Args[2:])
	case "--version", "version":
		fmt.Fprintf(os.Stderr, "burkebot version %s\n", Version)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		os.Exit(2)
	}
}
