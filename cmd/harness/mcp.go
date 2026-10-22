package main

import "io"

func runMCP(args []string, stdout, stderr io.Writer) int {
	return runServerCommand("mcp", args, stdout, stderr)
}
