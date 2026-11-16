package cron

import (
	"context"

	"charm.land/log/v2"
	"github.com/robfig/cron/v3"
)

// Scheduler runs functions on cron schedules. Every function it runs is
// recovered and skipped while its previous run is still going; see AddFunc.
//
// The cron.Cron is a named field rather than embedded, so that its AddJob
// and Schedule, which would bypass those wrappers, are not promoted.
type Scheduler struct {
	cron   *cron.Cron
	logger cronLogger
}

// cronLogger is a wrapper around the logger to make it compatible with the
// cron logger.
type cronLogger struct {
	logger *log.Logger
}

// Info logs routine messages about cron's operation.
func (l cronLogger) Info(msg string, keysAndValues ...interface{}) {
	l.logger.Debug(msg, keysAndValues...)
}

// Error logs an error condition.
func (l cronLogger) Error(err error, msg string, keysAndValues ...interface{}) {
	l.logger.Error(msg, append(keysAndValues, "err", err)...)
}

// skipLogger logs the runs skipped by cron.SkipIfStillRunning. Unlike
// cronLogger it logs them at Info: a skipped run means a job is taking longer
// than its interval, which an operator should be able to see. Its logger
// names the job, which cron does not pass along.
type skipLogger struct {
	cronLogger
}

// Info logs a skipped run.
func (l skipLogger) Info(_ string, keysAndValues ...interface{}) {
	l.logger.Info("skipping job run, the previous run is still in progress", keysAndValues...)
}

// NewScheduler returns a new Scheduler.
//
// A job that panics is recovered and the panic logged, so it does not take
// the server down, and the job still runs on its next tick. A job run is
// skipped while the previous run of the same job is still going, so a job
// that takes longer than its interval does not pile up runs. AddFunc applies
// both to each job.
func NewScheduler(ctx context.Context) *Scheduler {
	logger := cronLogger{log.FromContext(ctx).WithPrefix("cron")}
	return &Scheduler{
		cron:   cron.New(cron.WithLogger(logger)),
		logger: logger,
	}
}

// Shutdown stops the Scheduler from starting jobs and waits for the running
// ones to end, or for ctx to be done, and returns ctx's error in that case.
// Running jobs are not interrupted: give them a context to cancel first.
func (s *Scheduler) Shutdown(ctx context.Context) error {
	select {
	case <-s.cron.Stop().Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start starts the Scheduler.
func (s *Scheduler) Start() {
	s.cron.Start()
}

// AddFunc adds a job to the Scheduler, to run fn on spec, a standard cron
// spec or a descriptor such as "@every 1h". name identifies the job in the
// log.
func (s *Scheduler) AddFunc(name, spec string, fn func()) error {
	logger := cronLogger{s.logger.logger.With("job", name)}
	// The first wrapper is the outermost. Recover must sit inside
	// SkipIfStillRunning: if it wrapped it, a panic would unwind past the
	// point where SkipIfStillRunning hands back its run token, and every
	// later run of the job would be skipped.
	job := cron.NewChain(
		cron.SkipIfStillRunning(skipLogger{logger}),
		cron.Recover(logger),
	).Then(cron.FuncJob(fn))
	_, err := s.cron.AddJob(spec, job)
	return err
}
