package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/nejmlabs/things-index/internal/serverapp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serverapp.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
