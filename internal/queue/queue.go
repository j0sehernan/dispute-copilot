// Package queue runs background jobs stored in PostgreSQL. Workers claim
// jobs with SELECT ... FOR UPDATE SKIP LOCKED, so any number of worker
// processes can share the table. A claimed job holds a lease; if a worker
// dies, the lease expires and another worker picks the job up again. That
// makes delivery at-least-once, so handlers must be idempotent.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Job struct {
	ID          int64
	Kind        string
	DisputeID   uuid.UUID
	Attempts    int
	MaxAttempts int
}

type Store interface {
	ClaimJob(ctx context.Context, lease time.Duration) (*Job, error)
	CompleteJob(ctx context.Context, id int64) error
	RetryJob(ctx context.Context, id int64, reason string, delay time.Duration) error
	FailJob(ctx context.Context, id int64, reason string) error
}

type Handler func(ctx context.Context, job Job) error

// FailureHook runs once a job has failed for good.
type FailureHook func(ctx context.Context, job Job, err error)

type permanentError struct{ err error }

func (p permanentError) Error() string { return p.err.Error() }
func (p permanentError) Unwrap() error { return p.err }

// Permanent marks an error that retrying will not fix.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

type Worker struct {
	store       Store
	handlers    map[string]Handler
	onFailure   FailureHook
	log         *slog.Logger
	concurrency int
	lease       time.Duration
	poll        time.Duration
	wake        <-chan struct{}
	// JobTimeout bounds a single handler run. It must be shorter than the
	// lease, or a slow job could run twice.
	jobTimeout time.Duration
}

type Options struct {
	Concurrency int
	Lease       time.Duration
	Poll        time.Duration
	// Wake, if set, signals that new jobs are available (LISTEN/NOTIFY).
	// Polling still runs as a fallback.
	Wake      <-chan struct{}
	OnFailure FailureHook
}

func NewWorker(store Store, handlers map[string]Handler, log *slog.Logger, opts Options) *Worker {
	w := &Worker{
		store:       store,
		handlers:    handlers,
		onFailure:   opts.OnFailure,
		log:         log,
		concurrency: max(opts.Concurrency, 1),
		lease:       opts.Lease,
		poll:        opts.Poll,
		wake:        opts.Wake,
	}
	if w.lease <= 0 {
		w.lease = 10 * time.Minute
	}
	if w.poll <= 0 {
		w.poll = 2 * time.Second
	}
	w.jobTimeout = w.lease - w.lease/10
	return w
}

// Run blocks until ctx is cancelled, then waits for running jobs to finish.
func (w *Worker) Run(ctx context.Context) {
	// Fan the wake signal out to all goroutines.
	signals := make([]chan struct{}, w.concurrency)
	for i := range signals {
		signals[i] = make(chan struct{}, 1)
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
				for _, s := range signals {
					select {
					case s <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for i := range w.concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx, signals[i])
		}()
	}
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context, wake <-chan struct{}) {
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()
	for {
		// Drain the queue before going back to sleep.
		for ctx.Err() == nil {
			worked, err := w.runOne(ctx)
			if err != nil {
				w.log.Error("queue: claim failed", "err", err)
				break
			}
			if !worked {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
	}
}

func (w *Worker) runOne(ctx context.Context) (bool, error) {
	job, err := w.store.ClaimJob(ctx, w.lease)
	if err != nil || job == nil {
		return false, err
	}

	log := w.log.With("job_id", job.ID, "kind", job.Kind, "dispute_id", job.DisputeID, "attempt", job.Attempts)
	handler, ok := w.handlers[job.Kind]
	if !ok {
		w.fail(context.WithoutCancel(ctx), log, *job, fmt.Errorf("no handler for job kind %q", job.Kind))
		return true, nil
	}

	// Jobs run on a context that is not cancelled by shutdown, so a deploy
	// does not cut an LLM call in half. The timeout still applies.
	jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.jobTimeout)
	defer cancel()

	start := time.Now()
	err = safeRun(jobCtx, handler, *job)
	elapsed := time.Since(start)
	observeJob(job.Kind, elapsed, err)

	switch {
	case err == nil:
		log.Info("job done", "duration_ms", elapsed.Milliseconds())
		if err := w.store.CompleteJob(jobCtx, job.ID); err != nil {
			log.Error("queue: complete failed", "err", err)
		}
	case IsPermanent(err) || job.Attempts >= job.MaxAttempts:
		w.fail(jobCtx, log, *job, err)
	default:
		delay := backoff(job.Attempts)
		log.Warn("job failed, will retry", "err", err, "retry_in", delay.String())
		if err := w.store.RetryJob(jobCtx, job.ID, err.Error(), delay); err != nil {
			log.Error("queue: retry failed", "err", err)
		}
	}
	return true, nil
}

func (w *Worker) fail(ctx context.Context, log *slog.Logger, job Job, err error) {
	log.Error("job failed", "err", err)
	if ferr := w.store.FailJob(ctx, job.ID, err.Error()); ferr != nil {
		log.Error("queue: fail failed", "err", ferr)
	}
	if w.onFailure != nil {
		w.onFailure(ctx, job, err)
	}
}

func safeRun(ctx context.Context, h Handler, job Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = Permanent(fmt.Errorf("panic: %v", r))
		}
	}()
	return h(ctx, job)
}

// backoff is 10s, 40s, 90s, ... capped at 10 minutes.
func backoff(attempt int) time.Duration {
	d := time.Duration(math.Pow(float64(attempt), 2)) * 10 * time.Second
	return min(d, 10*time.Minute)
}
