package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/nejmlabs/things-index/internal/helper"
	"github.com/nejmlabs/things-index/internal/workerapp"
	"github.com/nejmlabs/things-index/internal/workersetup"
)

func main() {
	var err error
	switch {
	case len(os.Args) == 1:
		err = run()
	case len(os.Args) == 2 && os.Args[1] == "--setup":
		err = runSetup()
	default:
		err = errors.New("usage: things-index-worker [--setup]")
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func runSetup() error {
	if runtime.GOOS != "darwin" {
		return errors.New("ThingsIndex worker setup requires macOS")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stateDirectory, err := workerapp.StateDirectory()
	if err != nil {
		return err
	}
	verifier := helper.NewClient(os.Getenv("THINGS_INDEX_THINGS_AUTH_TOKEN"))
	if dbPath := os.Getenv("THINGS_INDEX_THINGS_DB_PATH"); dbPath != "" {
		verifier.DBPath = dbPath
	}
	return workersetup.Run(ctx, workersetup.Config{
		StateDir:    stateDirectory,
		Verifier:    verifier,
		OpenFile:    workersetup.Open,
		OpenBrowser: workersetup.Open,
	})
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return workerapp.Run(ctx)
}
