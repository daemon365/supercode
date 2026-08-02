package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/daemon365/supercode/app"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, os.LookupEnv); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "supercode: %v\n", err)
		os.Exit(1)
	}
}
