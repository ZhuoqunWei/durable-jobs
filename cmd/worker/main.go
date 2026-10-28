package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"durable-jobs/internal/database"
	"durable-jobs/internal/worker"
)

func main() {
	once := flag.Bool("once", false, "recover expired claims, then process at most one due job and exit")
	processor := flag.String("processor", "versioned", "processor mode: versioned (local scripts + built-in) or builtin (no Docker required)")
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger, *once, *processor); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger, once bool, processorName string) error {
	processor, config, err := selectProcessor(processorName)
	if err != nil {
		return err
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return errors.New("DATABASE_URL is required; run make worker from the project folder")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	pool, err := database.Open(startupCtx, databaseURL)
	cancel()
	if err != nil {
		return err
	}
	defer pool.Close()
	store := database.NewJobStore(pool)
	w, err := worker.New(store, processor, config)
	if err != nil {
		return err
	}
	w.SetLogger(logger)
	workerID, _ := os.Hostname()
	if workerID == "" {
		workerID = "local-worker"
	}
	beatCtx, stopBeat := context.WithCancel(ctx)
	beatDone := make(chan struct{})
	go func() {
		defer close(beatDone)
		for {
			callCtx, cancel := context.WithTimeout(beatCtx, 2*time.Second)
			err := store.Heartbeat(callCtx, workerID)
			cancel()
			if err != nil && beatCtx.Err() == nil {
				logger.Error("worker heartbeat failed", "error", err)
			}
			select {
			case <-beatCtx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
	defer func() { stopBeat(); <-beatDone }()
	logger.Info("starting worker", "once", once, "processor", processorName)
	if once {
		worked, err := w.RunOnce(ctx)
		if errors.Is(err, worker.ErrStopWorker) {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		logger.Info("worker iteration ended", "claimed_job", worked, "completion_confirmed", worked && err == nil)
		return err
	}
	return w.Run(ctx, logger)
}

// Preserve the existing local script mode; container validation opts into a
// built-in-only processor rather than exposing the host Docker daemon to a pod.
func selectProcessor(name string) (worker.Processor, worker.Config, error) {
	switch name {
	case "versioned":
		return worker.NewVersionedProcessor(), worker.ScriptConfig(), nil
	case "builtin":
		return worker.CampaignProcessor{}, worker.DefaultConfig(), nil
	default:
		return nil, worker.Config{}, errors.New("processor must be versioned or builtin")
	}
}
