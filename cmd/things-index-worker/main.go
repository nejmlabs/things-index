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
	log.Printf("Starting ThingsIndex worker...")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	journalPath, err := configuredJournalPath()
	if err != nil {
		return fmt.Errorf("configure journal path: %w", err)
	}
	log.Printf("Opening journal at %s", journalPath)
	journalStore, err := journal.Open(journalPath)
	if err != nil {
		return fmt.Errorf("open journal: %w", err)
	}
	defer journalStore.Close()
	journalRetention, err := configuredJournalRetention()
	if err != nil {
		return fmt.Errorf("configure journal retention: %w", err)
	}

	captureAdapter := helper.NewClient(os.Getenv("THINGS_INDEX_THINGS_AUTH_TOKEN"))
	if dbPath := os.Getenv("THINGS_INDEX_THINGS_DB_PATH"); dbPath != "" {
		captureAdapter.DBPath = dbPath
	}
	log.Printf("Checking Things 3 database...")
	if err := captureAdapter.Ping(ctx); err != nil {
		return fmt.Errorf("check Things 3 database: %w", err)
	}
	log.Printf("Things 3 database OK")

	serverURL := os.Getenv("THINGS_INDEX_SERVER_URL")
	log.Printf("Connecting to server at %s...", serverURL)
	serverClient, err := worker.NewClient(worker.ClientConfig{
		BaseURL: serverURL,
		Token:   os.Getenv("THINGS_INDEX_WORKER_TOKEN"),
	})
	if err != nil {
		return fmt.Errorf("create server client: %w", err)
	}
	if err := pruneJournal(ctx, journalStore, journalRetention, time.Now()); err != nil {
		return fmt.Errorf("prune journal: %w", err)
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

	log.Printf("ThingsIndex worker ready (native Things 3 URL & SQLite engine); polling %s", serverURL)
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
		log.Printf("Received leased job: %s (attempts: %d)", lease.ID, lease.Attempts)

		outcome, processErr := processor.Process(ctx, lease.Job)
		var reportErr error
		if processErr != nil {
			log.Printf("Job %s failed: %v", lease.ID, processErr)
			reportErr = serverClient.Fail(ctx, *lease, processErr, worker.IsRetryable(processErr))
		} else {
			log.Printf("Job %s succeeded (things_id=%s)", lease.ID, outcome.ThingsID)
			reportErr = serverClient.Complete(ctx, *lease, outcome)
		}
		if reportErr != nil {
			log.Printf("report job %s: %v", lease.ID, reportErr)
			continue
		}
		if processErr == nil && worker.UsesJournal(lease.Job.Task) {
			if err := journalStore.MarkReported(ctx, lease.ID); err != nil {
				log.Printf("record report state for job %s: %v", lease.ID, err)
			}
		}
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
	return filepath.Join(stateDirectory, "journal.sqlite"), nil
}

func configuredStateDirectory() (string, error) {
	if directory := os.Getenv("THINGS_INDEX_STATE_DIR"); directory != "" {
		return directory, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("lookup home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "things-index"), nil
}

func configuredJournalRetention() (time.Duration, error) {
	return retention.ParseDays("THINGS_INDEX_JOURNAL_RETENTION_DAYS", os.Getenv("THINGS_INDEX_JOURNAL_RETENTION_DAYS"), 7)
}

func pruneJournal(ctx context.Context, journalStore *journal.Store, journalRetention time.Duration, now time.Time) error {
	_, err := journalStore.PruneReported(ctx, retention.Cutoff(now, journalRetention))
	return err
}

func cleanJournalPeriodically(ctx context.Context, journalStore *journal.Store, journalRetention time.Duration) {
	ticker := time.NewTicker(journalCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := pruneJournal(ctx, journalStore, journalRetention, now); err != nil {
				log.Printf("prune journal: %v", err)
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
