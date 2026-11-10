// Command fleet is the multi-project agent fleet: a Bubble Tea cockpit, a
// headless daemon and the CLI that leads, workers and external agents use.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := newRootCmd().ExecuteContext(ctx)
	stop()
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "fleet:", err)
		}
		os.Exit(1)
	}
}
