// Command lockmq lets an AI agent inspect a RabbitMQ broker through a
// console the human holds: read-only by default, message content masked,
// every content read approved.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at build time (-X main.version=...).
var version = "dev"

// Exit codes: 0 ok, 1 refused/denied/failed, 2 no console, 3 usage or
// configuration error.
const (
	exitOK    = 0
	exitUsage = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		fmt.Fprintln(stdout, "lockmq "+version)
		return exitOK
	}
	fmt.Fprintln(stderr, "usage: lockmq version")
	return exitUsage
}
