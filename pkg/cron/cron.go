package cron

import (
	"context"
	"time"

	"charm.land/log/v2"
	"github.com/robfig/cron/v3"
)

// Scheduler is a cron-like job scheduler.
type Scheduler struct {
	*cron.Cron
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

// NewScheduler returns a new Cron.
//
// A job run is skipped while the previous run of the same job is still going,
// so a job that takes longer than its interval does not pile up runs.
func NewScheduler(ctx context.Context) *Scheduler {
	logger := cronLogger{log.FromContext(ctx).WithPrefix("cron")}
	return &Scheduler{
		Cron:   cron.New(cron.WithLogger(logger)),
		logger: logger,
	}
}

// Shutdonw gracefully shuts down the Scheduler.
func (s *Scheduler) Shutdown() {
	ctx, cancel := context.WithTimeout(s.Cron.Stop(), 30*time.Second)
	defer func() { cancel() }()
	<-ctx.Done()
}

// Start starts the Scheduler.
func (s *Scheduler) Start() {
	s.Cron.Start()
}

// AddFunc adds a job to the Scheduler. name identifies the job in the log.
func (s *Scheduler) AddFunc(name, spec string, fn func()) (int, error) {
	skip := skipLogger{cronLogger{s.logger.logger.With("job", name)}}
	job := cron.NewChain(cron.SkipIfStillRunning(skip)).Then(cron.FuncJob(fn))
	id, err := s.Cron.AddJob(spec, job)
	return int(id), err
}

// Remove removes a job from the Scheduler.
func (s *Scheduler) Remove(id int) {
	s.Cron.Remove(cron.EntryID(id))
}
