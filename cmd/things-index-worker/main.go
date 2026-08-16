package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/nejmlabs/things-index/internal/helper"
	"github.com/nejmlabs/things-index/internal/journal"
	"github.com/nejmlabs/things-index/internal/retention"
	"github.com/nejmlabs/things-index/internal/worker"
	"github.com/nejmlabs/things-index/internal/workersetup"
	shortcutasset "github.com/nejmlabs/things-index/shortcuts"
)

const journalCleanupInterval = 6 * time.Hour

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
	stateDirectory, err := configuredStateDirectory()
	if err != nil {
		return err
	}
	helperDirectory := os.Getenv("THINGS_INDEX_HELPER_TEMP_DIR")
	if helperDirectory == "" {
		helperDirectory = filepath.Join(stateDirectory, "HelperRequests")
	}
	return workersetup.Run(ctx, workersetup.Config{
		Shortcut:    shortcutasset.Helper(),
		StateDir:    stateDirectory,
		Verifier:    helper.NewClient(helperDirectory),
		OpenFile:    workersetup.Open,
		OpenBrowser: workersetup.Open,
	})
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	journalPath, err := configuredJournalPath()
	if err != nil {
		return err
	}
	journalStore, err := journal.Open(journalPath)
	if err != nil {
		return err
	}
	defer journalStore.Close()
	journalRetention, err := configuredJournalRetention()
	if err != nil {
		return err
	}

	captureAdapter := helper.NewClient(os.Getenv("THINGS_INDEX_HELPER_TEMP_DIR"))
	if err := captureAdapter.Ping(ctx); err != nil {
		return fmt.Errorf("check %s Shortcut: %w", helper.ShortcutName, err)
	}

	serverClient, err := worker.NewClient(worker.ClientConfig{
		BaseURL: os.Getenv("THINGS_INDEX_SERVER_URL"),
		Token:   os.Getenv("THINGS_INDEX_WORKER_TOKEN"),
	})
	if err != nil {
		return err
	}
	if err := pruneJournal(ctx, journalStore, journalRetention, time.Now()); err != nil {
		return err
	}
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		cleanJournalPeriodically(ctx, journalStore, journalRetention)
	}()
	defer func() {
		stop()
		<-cleanupDone
	}()
	processor := &worker.Processor{Helper: captureAdapter, Journal: journalStore}

	log.Printf("ThingsIndex worker ready with %s Shortcut; polling %s", helper.ShortcutName, os.Getenv("THINGS_INDEX_SERVER_URL"))
	for ctx.Err() == nil {
		lease, err := serverClient.Lease(ctx)
		if err != nil {
			log.Printf("poll server: %v", err)
			if err := wait(ctx, 2*time.Second); err != nil {
				return err
			}
			continue
		}
		if lease == nil {
			continue
		}

		outcome, processErr := processor.Process(ctx, lease.Job)
		if processErr != nil {
			retryable := worker.IsRetryable(processErr)
			if err := serverClient.Fail(ctx, *lease, processErr, retryable); err != nil {
				log.Printf("report failed capture %s: %v", lease.ID, err)
			} else {
				log.Printf("capture %s failed (retryable=%t): %v", lease.ID, retryable, processErr)
			}
			continue
		}

		if err := serverClient.Complete(ctx, *lease, outcome); err != nil {
			// The server may have committed the completion before the connection
			// failed. Leave the journal finalised so a re-lease is harmless.
			log.Printf("report completed capture %s: %v", lease.ID, err)
			continue
		}
		if err := processor.MarkReported(ctx, lease.ID); err != nil {
			log.Printf("mark capture %s reported locally: %v", lease.ID, err)
		}
		log.Printf("captured Things task %s for request %s", outcome.ThingsID, lease.ID)
	}
	return ctx.Err()
}

func configuredJournalPath() (string, error) {
	if path := os.Getenv("THINGS_INDEX_JOURNAL_PATH"); path != "" {
		return path, nil
	}
	stateDirectory, err := configuredStateDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDirectory, "journal.db"), nil
}

func configuredStateDirectory() (string, error) {
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user configuration directory: %w", err)
	}
	return filepath.Join(configDirectory, "ThingsIndex"), nil
}

func configuredJournalRetention() (time.Duration, error) {
	return retention.ParseDays(
		"THINGS_INDEX_JOURNAL_RETENTION_DAYS",
		os.Getenv("THINGS_INDEX_JOURNAL_RETENTION_DAYS"),
		30,
	)
}

func pruneJournal(ctx context.Context, store *journal.Store, keep time.Duration, now time.Time) error {
	count, err := store.PruneReported(ctx, retention.Cutoff(now, keep))
	if err != nil {
		return fmt.Errorf("clean reported capture deliveries: %w", err)
	}
	if count > 0 {
		log.Printf("pruned %d reported capture deliveries", count)
	}
	return nil
}

func cleanJournalPeriodically(ctx context.Context, store *journal.Store, keep time.Duration) {
	ticker := time.NewTicker(journalCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := pruneJournal(ctx, store, keep, now); err != nil && ctx.Err() == nil {
				log.Printf("journal retention: %v", err)
			}
		}
	}
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
