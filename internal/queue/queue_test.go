package queue

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type memStore struct {
	mu        sync.Mutex
	pending   []*Job
	completed []int64
	retried   []int64
	failed    []int64
}

func (m *memStore) ClaimJob(context.Context, time.Duration) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 0 {
		return nil, nil
	}
	j := m.pending[0]
	m.pending = m.pending[1:]
	j.Attempts++
	return j, nil
}

func (m *memStore) CompleteJob(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.completed = append(m.completed, id)
	return nil
}

func (m *memStore) RetryJob(_ context.Context, id int64, _ string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retried = append(m.retried, id)
	return nil
}

func (m *memStore) FailJob(_ context.Context, id int64, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failed = append(m.failed, id)
	return nil
}

func TestWorkerOutcomes(t *testing.T) {
	st := &memStore{pending: []*Job{
		{ID: 1, Kind: "ok", MaxAttempts: 3},
		{ID: 2, Kind: "flaky", MaxAttempts: 3},
		{ID: 3, Kind: "flaky", MaxAttempts: 3, Attempts: 2}, // last attempt
		{ID: 4, Kind: "bad", MaxAttempts: 3},
		{ID: 5, Kind: "panics", MaxAttempts: 3},
		{ID: 6, Kind: "unknown", MaxAttempts: 3},
	}}
	for _, j := range st.pending {
		j.DisputeID = uuid.New()
	}

	var mu sync.Mutex
	var hooked []int64
	handlers := map[string]Handler{
		"ok":     func(context.Context, Job) error { return nil },
		"flaky":  func(context.Context, Job) error { return errors.New("timeout") },
		"bad":    func(context.Context, Job) error { return Permanent(errors.New("invalid")) },
		"panics": func(context.Context, Job) error { panic("boom") },
	}
	w := NewWorker(st, handlers, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Concurrency: 2,
		Poll:        10 * time.Millisecond,
		OnFailure: func(_ context.Context, j Job, _ error) {
			mu.Lock()
			hooked = append(hooked, j.ID)
			mu.Unlock()
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	if len(st.completed) != 1 || st.completed[0] != 1 {
		t.Errorf("completed = %v", st.completed)
	}
	if len(st.retried) != 1 || st.retried[0] != 2 {
		t.Errorf("retried = %v, want [2]", st.retried)
	}
	if len(st.failed) != 4 || len(hooked) != 4 {
		t.Errorf("failed = %v, hook = %v, want jobs 3,4,5,6", st.failed, hooked)
	}
}

func TestBackoff(t *testing.T) {
	if backoff(1) != 10*time.Second || backoff(3) != 90*time.Second || backoff(100) != 10*time.Minute {
		t.Errorf("backoff = %v %v %v", backoff(1), backoff(3), backoff(100))
	}
}
