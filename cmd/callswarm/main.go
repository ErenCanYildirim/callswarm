// Command callswarm runs scripted, concurrent callers against a voice agent.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ErenCanYildirim/callswarm/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// The first Ctrl-C stops the run gracefully: calls in progress hang up
	// and the partial report is printed. A second one exits immediately.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	os.Exit(cli.Run(ctx, os.Args[1:], version, os.Stdout, os.Stderr))
}
