// Package main is the entry point for the compiled Pdf Splitter application.
// It supports:
// - Standalone CLI mode for terminal batch automation.
// - Embedded modern Web UI for local desktop use.
// - Full-fledged Web Server for VPS deployment with auto-cleanup and ZIP packaging.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"pdf-plitter/cli"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := cli.Run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "[ERROR] %v\n", err)
		os.Exit(1)
	}
}
