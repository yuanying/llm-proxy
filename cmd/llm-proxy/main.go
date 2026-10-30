// Command llm-proxy serves one ChatGPT subscription to trusted callers on the
// OpenAI Responses API.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set by the build (-X main.version).
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "version" {
		_, _ = fmt.Fprintln(stdout, version)
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "usage: llm-proxy version")
	return 2
}
