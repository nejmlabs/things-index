package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nejmlabs/things-index/internal/queue"
	"github.com/nejmlabs/things-index/internal/retention"
	indexserver "github.com/nejmlabs/things-index/internal/server"
)

const cleanupInterval = 6 * time.Hour

type terminalRetention struct {
	succeeded time.Duration
	failed    time.Duration
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	listenAddress := envOr("THINGS_INDEX_LISTEN_ADDR", "127.0.0.1:8080")
	if err := requireNonPublicAddress(listenAddress); err != nil {
		return err
	}

	queuePath, err := configuredQueuePath()
	if err != nil {
		return err
	}
	store, err := queue.Open(queuePath)
	if err != nil {
		return err
	}
	defer store.Close()
	retentionPolicy, err := configuredTerminalRetention()
	if err != nil {
		return err
	}

	handler, err := indexserver.NewHandler(store, indexserver.Config{
		PublicToken:    os.Getenv("THINGS_INDEX_PUBLIC_TOKEN"),
		WorkerToken:    os.Getenv("THINGS_INDEX_WORKER_TOKEN"),
		AllowedOrigins: commaList(os.Getenv("THINGS_INDEX_ALLOWED_ORIGINS")),
		DashboardToken: os.Getenv("THINGS_INDEX_DASHBOARD_TOKEN"),
	})
	if err != nil {
		return fmt.Errorf("configure server: %w", err)
	}
	if err := pruneQueue(context.Background(), store, retentionPolicy, time.Now()); err != nil {
		return err
	}

	server := &http.Server{
		Addr:              listenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       45 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		cleanQueuePeriodically(ctx, store, retentionPolicy)
	}()
	defer func() {
		stop()
		<-cleanupDone
	}()
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}()

	log.Printf("ThingsIndex server listening on http://%s behind the HTTPS reverse proxy", listenAddress)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve ThingsIndex: %w", err)
	}
	return nil
}

func requireNonPublicAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("THINGS_INDEX_LISTEN_ADDR must be a host:port pair: %w", err)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback() && !ip.IsPrivate()) {
		return errors.New("THINGS_INDEX_LISTEN_ADDR must use a loopback or private IP address; terminate HTTPS at the reverse proxy")
	}
	return nil
}

func commaList(value string) []string {
	var values []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			values = append(values, item)
		}
	}
	return values
}

func configuredQueuePath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("THINGS_INDEX_DB_PATH")); path != "" {
		return path, nil
	}
	configDirectory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user configuration directory: %w", err)
	}
	return filepath.Join(configDirectory, "ThingsIndex", "queue.db"), nil
}

func configuredTerminalRetention() (terminalRetention, error) {
	succeeded, err := retention.ParseDays(
		"THINGS_INDEX_SUCCEEDED_RETENTION_DAYS",
		os.Getenv("THINGS_INDEX_SUCCEEDED_RETENTION_DAYS"),
		7,
	)
	if err != nil {
		return terminalRetention{}, err
	}
	failed, err := retention.ParseDays(
		"THINGS_INDEX_FAILED_RETENTION_DAYS",
		os.Getenv("THINGS_INDEX_FAILED_RETENTION_DAYS"),
		30,
	)
	if err != nil {
		return terminalRetention{}, err
	}
	return terminalRetention{succeeded: succeeded, failed: failed}, nil
}

func pruneQueue(ctx context.Context, store *queue.Store, policy terminalRetention, now time.Time) error {
	pruned, err := store.PruneTerminal(
		ctx,
		retention.Cutoff(now, policy.succeeded),
		retention.Cutoff(now, policy.failed),
	)
	if err != nil {
		return fmt.Errorf("clean terminal capture jobs: %w", err)
	}
	if pruned.Succeeded > 0 || pruned.Failed > 0 {
		log.Printf("pruned %d succeeded and %d failed capture jobs", pruned.Succeeded, pruned.Failed)
	}
	return nil
}

func cleanQueuePeriodically(ctx context.Context, store *queue.Store, policy terminalRetention) {
	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := pruneQueue(ctx, store, policy, now); err != nil && ctx.Err() == nil {
				log.Printf("queue retention: %v", err)
			}
		}
	}
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
