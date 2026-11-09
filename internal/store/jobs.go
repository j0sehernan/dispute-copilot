package store

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/j0sehernan/dispute-copilot/internal/queue"
)

const jobsChannel = "jobs"

// enqueue adds a job inside the caller's transaction. The NOTIFY is sent by
// Postgres only when the transaction commits.
func enqueue(ctx context.Context, tx dbtx, kind string, disputeID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `INSERT INTO jobs (kind, dispute_id) VALUES ($1, $2)`, kind, disputeID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, jobsChannel, kind)
	return err
}

// ClaimJob takes the oldest runnable job: queued and due, or running with
// an expired lease (its worker died).
func (s *Store) ClaimJob(ctx context.Context, lease time.Duration) (*queue.Job, error) {
	var j queue.Job
	err := s.pool.QueryRow(ctx, `
		UPDATE jobs SET
			status = 'running',
			attempts = attempts + 1,
			locked_until = now() + $1::interval,
			updated_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE (status = 'queued' AND run_at <= now())
			   OR (status = 'running' AND locked_until < now())
			ORDER BY run_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, kind, dispute_id, attempts, max_attempts`, lease,
	).Scan(&j.ID, &j.Kind, &j.DisputeID, &j.Attempts, &j.MaxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (s *Store) CompleteJob(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = 'done', locked_until = NULL, updated_at = now() WHERE id = $1`, id)
	return err
}

func (s *Store) RetryJob(ctx context.Context, id int64, reason string, delay time.Duration) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = 'queued', locked_until = NULL, last_error = $2,
			run_at = now() + $3::interval, updated_at = now()
		WHERE id = $1`, id, truncate(reason, 1000), delay)
	return err
}

func (s *Store) FailJob(ctx context.Context, id int64, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE jobs SET status = 'failed', locked_until = NULL, last_error = $2, updated_at = now()
		WHERE id = $1`, id, truncate(reason, 1000))
	return err
}

// ListenJobs returns a channel that receives a value whenever a job is
// enqueued. It keeps one connection open and reconnects on errors. The
// worker still polls, so a lost notification only adds latency.
func (s *Store) ListenJobs(ctx context.Context) <-chan struct{} {
	wake := make(chan struct{}, 1)
	go func() {
		for ctx.Err() == nil {
			err := s.listen(ctx, wake)
			if ctx.Err() != nil {
				return
			}
			s.log.Warn("listen: connection lost, reconnecting", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	return wake
}

func (s *Store) listen(ctx context.Context, wake chan<- struct{}) error {
	pooled, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// A LISTEN connection keeps session state, so it leaves the pool for good.
	conn := pooled.Hijack()
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, "LISTEN "+jobsChannel); err != nil {
		return err
	}
	s.log.Debug("listening for jobs", slog.String("channel", jobsChannel))
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
