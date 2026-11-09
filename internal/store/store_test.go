package store

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/j0sehernan/dispute-copilot/internal/domain"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	name := "copilot_store_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name

	st, err := Open(ctx, u.String(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		st.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close(context.Background())
	})
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return st
}

func newDispute(t *testing.T, st *Store, job string) uuid.UUID {
	t.Helper()
	d := &domain.Dispute{ID: uuid.New(), Source: domain.SourceUpload, InputKind: domain.InputText, RawInput: "x", DedupeKey: uuid.NewString()}
	id, created, err := st.CreateDispute(context.Background(), d, job)
	if err != nil || !created {
		t.Fatalf("create: %v", err)
	}
	return id
}

func TestClaimIsExclusive(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	const n = 20
	for range n {
		newDispute(t, st, "extract")
	}

	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				j, err := st.ClaimJob(ctx, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if j == nil {
					return
				}
				mu.Lock()
				seen[j.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != n {
		t.Errorf("claimed %d distinct jobs, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("job %d claimed %d times", id, c)
		}
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	newDispute(t, st, "extract")

	j, err := st.ClaimJob(ctx, 50*time.Millisecond)
	if err != nil || j == nil {
		t.Fatalf("claim: %v %v", j, err)
	}
	if again, _ := st.ClaimJob(ctx, time.Minute); again != nil {
		t.Fatal("job with a live lease was claimed twice")
	}
	time.Sleep(100 * time.Millisecond)
	again, err := st.ClaimJob(ctx, time.Minute)
	if err != nil || again == nil || again.ID != j.ID || again.Attempts != 2 {
		t.Fatalf("reclaim = %+v, %v", again, err)
	}

	if err := st.RetryJob(ctx, j.ID, "temporary", time.Hour); err != nil {
		t.Fatal(err)
	}
	if later, _ := st.ClaimJob(ctx, time.Minute); later != nil {
		t.Error("retried job ran before its run_at")
	}
}

func TestStatusTransitions(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id := newDispute(t, st, "extract")

	if err := st.SaveEvidence(ctx, id, nil, "analyze"); err == nil {
		t.Error("saving evidence for a dispute that was not extracted must fail")
	}
	if err := st.StartStep(ctx, id, domain.StatusReceived, domain.StatusExtracting); err != nil {
		t.Fatal(err)
	}
	// Retried job after a crash: already in the target status.
	if err := st.StartStep(ctx, id, domain.StatusReceived, domain.StatusExtracting); err != nil {
		t.Errorf("restart of the same step: %v", err)
	}
	if err := st.MarkFailed(ctx, id, "model output failed validation"); err != nil {
		t.Fatal(err)
	}
	d, err := st.GetDispute(ctx, id)
	if err != nil || d.Status != domain.StatusFailed || d.FailureReason == "" {
		t.Fatalf("dispute = %+v, %v", d, err)
	}
	if err := st.Approve(ctx, id, domain.Review{Reviewer: "a", Decision: domain.RecommendAccept}); err == nil {
		t.Error("approving a failed dispute must fail")
	}
}
