// Command server is the composition root: the only place concrete
// implementations are constructed and injected. Dependencies point inward from
// here — api over files over {storage, metadata}.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mettjs/cairnmark/internal/api"
	"github.com/mettjs/cairnmark/internal/config"
	"github.com/mettjs/cairnmark/internal/files"
	"github.com/mettjs/cairnmark/internal/gc"
	"github.com/mettjs/cairnmark/internal/jobs"
	"github.com/mettjs/cairnmark/internal/metadata/postgres"
	"github.com/mettjs/cairnmark/internal/metrics"
	"github.com/mettjs/cairnmark/internal/storage/s3"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := boot(log); err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
}

func boot(log *slog.Logger) error {
	// One context, cancelled on SIGINT/SIGTERM, drives the HTTP server, the
	// background GC and the extraction-job worker so they shut down together.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	if err := runMigrations(cfg.Postgres.DSN, log); err != nil {
		return err
	}

	pool, err := openPostgres(ctx, cfg.Postgres.DSN)
	if err != nil {
		return err
	}
	defer pool.Close()

	backend, err := s3.New(ctx, s3Options(cfg.Storage))
	if err != nil {
		return err
	}
	log.Info("dependencies ready", "bucket", cfg.Storage.Bucket)

	repo := postgres.New(pool)
	collector := gc.New(backend, repo, log, gc.Options{
		Interval:       cfg.GC.Interval,
		GracePeriod:    cfg.GC.GracePeriod,
		IdempotencyTTL: cfg.GC.IdempotencyTTL,
		JobRetention:   cfg.Jobs.Retention,
	})
	collector.OnSweep(func(s gc.Stats, err error) {
		metrics.ObserveGCSweep(s.Purged, s.Orphans, s.ExpiredKeys, s.ExpiredJobs, err)
	})
	go collector.Run(ctx)

	svc := files.New(backend, repo, files.WithArchiveLimits(files.ArchiveLimits{
		MaxEntryBytes:     cfg.MaxUploadBytes, // an extracted entry is an upload
		MaxEntries:        cfg.Archive.MaxEntries,
		MaxTotalBytes:     cfg.Archive.MaxTotalBytes,
		MaxRatio:          cfg.Archive.MaxRatio,
		Extensions:        cfg.Archive.Extensions,
		MaxDirectoryBytes: cfg.Archive.MaxDirectoryBytes,
	}))

	// The worker is waited for, unlike GC: a sweep interrupted mid-way simply
	// runs again, but a job interrupted mid-run must get its row back to
	// pending, or it sits as running until the reaper's TTL expires.
	worker := jobs.New(repo, svc, log, jobs.Options{
		Concurrency:  cfg.Jobs.Concurrency,
		HeartbeatTTL: cfg.Jobs.HeartbeatTTL,
	})
	worker.OnJob(func(o jobs.Outcome) { metrics.ObserveJobRun(string(o.Status), o.Elapsed) })
	worker.OnSweep(func(s jobs.Sweep, err error) { metrics.ObserveJobSweep(s.Reaped, s.Pending, err) })
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(ctx)
	}()

	handler := api.Router(api.Deps{
		Files:          svc,
		ReadyCheck:     readiness(pool, backend),
		Logger:         log,
		PresignTTL:     cfg.PresignTTL,
		MaxUploadBytes: cfg.MaxUploadBytes,
	})

	// One shutdown budget for the HTTP drain and the worker together: the
	// clock starts when ctx is cancelled — by the signal, or below when run
	// fails without one — and the worker gets whatever the drain leaves of it.
	budgetSpent := make(chan struct{})
	context.AfterFunc(ctx, func() {
		time.AfterFunc(cfg.ShutdownTimeout, func() { close(budgetSpent) })
	})

	err = run(ctx, cfg, handler, log)
	stop() // a listen failure ends run with ctx still live; the worker must be told
	select {
	case <-workerDone:
	case <-budgetSpent:
		log.Warn("jobs worker did not stop within the shutdown timeout; a running job will be reaped")
	}
	return err
}
