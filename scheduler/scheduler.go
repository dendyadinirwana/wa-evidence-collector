// Package scheduler runs collector jobs on a built-in cron schedule, so the
// binary needs no external cron or systemd timer. It wraps robfig/cron/v3:
//
//   - 5-field cron specs (minute hour day-of-month month day-of-week) plus
//     descriptors such as @daily, @weekly, @every 1h.
//   - SkipIfStillRunning: a slow run is skipped, never overlapped.
//   - Recover: a panicking job does not kill the scheduler.
//   - Stop waits for a running job to finish (bounded by the caller's
//     context).
package scheduler

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/robfig/cron/v3"
)

// Job is one scheduled unit of work. The context is cancelled on shutdown;
// a job should stop early when it fires.
type Job func(ctx context.Context) error

// Scheduler owns the cron engine.
type Scheduler struct {
	cron   *cron.Cron
	logger *log.Logger
}

// New builds a Scheduler. logger defaults to stderr.
func New(logger *log.Logger) *Scheduler {
	if logger == nil {
		logger = log.New(os.Stderr, "scheduler: ", log.LstdFlags)
	}
	c := cron.New(
		cron.WithParser(cron.NewParser(
			cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow|cron.Descriptor,
		)),
		cron.WithChain(
			cron.SkipIfStillRunning(cron.PrintfLogger(logger)),
			cron.Recover(cron.PrintfLogger(logger)),
		),
	)
	return &Scheduler{cron: c, logger: logger}
}

// Add registers job under spec. An invalid spec is an error; nothing is
// scheduled in that case.
func (s *Scheduler) Add(spec string, job Job) error {
	_, err := s.cron.AddFunc(spec, func() {
		if err := job(context.Background()); err != nil {
			s.logger.Printf("scheduled job failed: %v", err)
		}
	})
	if err != nil {
		return fmt.Errorf("invalid schedule %q: %w", spec, err)
	}
	return nil
}

// Start begins firing scheduled jobs. It returns immediately.
func (s *Scheduler) Start() { s.cron.Start() }

// Stop halts the scheduler and waits for any running job to finish, up to
// the caller's context deadline.
func (s *Scheduler) Stop(ctx context.Context) {
	done := s.cron.Stop().Done()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// Entries returns the registered schedule entries (for logging/debugging).
func (s *Scheduler) Entries() []cron.Entry { return s.cron.Entries() }
