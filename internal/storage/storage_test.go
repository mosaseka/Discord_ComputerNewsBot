package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/article"
)

func TestMigrationDeliveryAndDailyDeduplication(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "nested", "bot.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	a := article.Article{
		GUID: "guid", Title: "Go release", URL: "https://example.com/go?utm_source=test",
		PublishedAt: &now, Source: article.Source{Name: "Example", Weight: 1},
	}
	articleID, err := store.UpsertArticle(ctx, a, 42, map[string]int{"total": 42})
	if err != nil {
		t.Fatalf("UpsertArticle() error = %v", err)
	}
	run, err := store.BeginRun(ctx, "scheduled", "channel", "2026-07-11")
	if err != nil {
		t.Fatalf("BeginRun() error = %v", err)
	}
	claimed, err := store.ClaimDailyBrief(ctx, "channel", "2026-07-11", run.ID)
	if err != nil || !claimed {
		t.Fatalf("ClaimDailyBrief() = %v, %v", claimed, err)
	}
	claimed, err = store.ClaimDailyBrief(ctx, "channel", "2026-07-11", run.ID)
	if err != nil || claimed {
		t.Fatalf("duplicate ClaimDailyBrief() = %v, %v", claimed, err)
	}

	batch, err := store.ReserveBatch(ctx, run, 0, "012345678901234567890123", []int64{articleID})
	if err != nil {
		t.Fatalf("ReserveBatch() error = %v", err)
	}
	if err := store.MarkBatchSent(ctx, batch.ID, "message"); err != nil {
		t.Fatal(err)
	}
	replacement := a
	replacement.GUID = "replacement-guid"
	replacement.Title = "Completely different title"
	replacement.NormalizedTitle = ""
	replacement.Source.Name = "Replacement source"
	if _, err := store.UpsertArticle(ctx, replacement, 1, nil); err != nil {
		t.Fatal(err)
	}
	sent, err := store.SentArticleURLs(ctx, "channel", "2026-07-11", false)
	if err != nil || len(sent) != 1 {
		t.Fatalf("SentArticleURLs() = %#v, %v", sent, err)
	}
	identities, err := store.SentArticleIdentities(ctx, "channel", "2026-07-11", false)
	if err != nil {
		t.Fatal(err)
	}
	changedURL := a
	changedURL.URL = "https://mirror.example.net/different"
	changedURL.CanonicalURL = ""
	changedURL.NormalizedURL = ""
	if !identities.Contains(changedURL) {
		t.Fatal("sent identity did not match the same GUID/title with a changed URL")
	}
	if err := store.FinishRun(ctx, run.ID, RunResult{Result: "success", FetchedCount: 1, SelectedCount: 1, PostedCount: 1}); err != nil {
		t.Fatal(err)
	}
	status, err := store.GetStatus(ctx)
	if err != nil || status.PostedCount != 1 || status.LastSuccessAt == nil {
		t.Fatalf("GetStatus() = %#v, %v", status, err)
	}
	retryRun, err := store.BeginRun(ctx, "manual", "channel", "2026-07-11")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveBatch(ctx, retryRun, 0, "retry0123456789012345678", []int64{articleID}); !errors.Is(err, ErrDeliveryConflict) {
		t.Fatalf("ReserveBatch() duplicate error = %v, want ErrDeliveryConflict", err)
	}
	var retryBatches int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM delivery_batches WHERE run_id=?", retryRun.ID).Scan(&retryBatches); err != nil {
		t.Fatal(err)
	}
	if retryBatches != 0 {
		t.Fatalf("conflicting reservation did not roll back its batch: %d", retryBatches)
	}
}

func TestSQLitePragmasAreApplied(t *testing.T) {
	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var foreignKeys, busyTimeout int
	var journalMode string
	if err := store.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || busyTimeout != 5000 || journalMode != "wal" {
		t.Fatalf("pragmas: foreign_keys=%d busy_timeout=%d journal_mode=%s", foreignKeys, busyTimeout, journalMode)
	}
}

func TestUnknownDeliveryIsNotAutomaticallyResent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	a := article.Article{Title: "Security", URL: "https://example.com/security", PublishedAt: &now, Source: article.Source{Name: "x", Weight: 1}}
	id, err := store.UpsertArticle(ctx, a, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	run, _ := store.BeginRun(ctx, "manual", "c", "2026-07-11")
	batch, err := store.ReserveBatch(ctx, run, 0, "abcdefghijklmnopqrstuvwx", []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBatchFailed(ctx, batch.ID, "unknown", context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	status, err := store.GetStatus(ctx)
	if err != nil || status.UnknownBatchCount != 1 || status.LastUnknownError == "" {
		t.Fatalf("unknown delivery status = %#v, %v", status, err)
	}
	sent, err := store.SentArticleURLs(ctx, "c", "2026-07-11", false)
	if err != nil || len(sent) != 1 {
		t.Fatalf("ambiguous delivery should be suppressed, got %#v, %v", sent, err)
	}
}

func TestReservationIsAtomicSendingAndSurvivesCrashWindow(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	a := article.Article{Title: "Release", URL: "https://example.com/release", PublishedAt: &now, Source: article.Source{Name: "x", Weight: 1}}
	id, err := store.UpsertArticle(ctx, a, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := store.BeginRun(ctx, "manual", "c", "2026-07-11")
	batch, err := store.ReserveBatch(ctx, first, 0, "atomic-sending", []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if batch.State != "sending" {
		t.Fatalf("ReserveBatch() state = %q, want sending", batch.State)
	}

	// Simulate process loss immediately after the reservation transaction. The
	// next process must treat the outcome as ambiguous rather than retrying.
	identities, err := store.SentArticleIdentities(ctx, "c", "2026-07-11", false)
	if err != nil || !identities.Contains(a) {
		t.Fatalf("reserved delivery was not suppressed: %#v, %v", identities, err)
	}
	status, err := store.GetStatus(ctx)
	if err != nil || status.UnknownBatchCount != 1 || status.LastUnknownError == "" {
		t.Fatalf("reserved delivery was not visible in status: %#v, %v", status, err)
	}
	second, _ := store.BeginRun(ctx, "manual", "c", "2026-07-11")
	if _, err := store.ReserveBatch(ctx, second, 0, "must-not-resend", []int64{id}); !errors.Is(err, ErrDeliveryConflict) {
		t.Fatalf("ambiguous delivery was reservable again: %v", err)
	}

	// Older databases may contain the former transient state. It is also
	// treated conservatively so an upgrade cannot create duplicate posts.
	if _, err := store.db.ExecContext(ctx, "UPDATE delivery_batches SET state='reserved' WHERE id=?", batch.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE deliveries SET state='reserved' WHERE batch_id=?", batch.ID); err != nil {
		t.Fatal(err)
	}
	identities, err = store.SentArticleIdentities(ctx, "c", "2026-07-11", false)
	if err != nil || !identities.Contains(a) {
		t.Fatalf("legacy reserved delivery was not suppressed: %#v, %v", identities, err)
	}
}

func TestDefinitiveFailureCanBeReservedByANewRun(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	a := article.Article{Title: "API", URL: "https://example.com/api", PublishedAt: &now, Source: article.Source{Name: "x", Weight: 1}}
	id, err := store.UpsertArticle(ctx, a, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := store.BeginRun(ctx, "manual", "c", "2026-07-11")
	batch, err := store.ReserveBatch(ctx, first, 0, "first", []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkBatchFailed(ctx, batch.ID, "failed", errors.New("HTTP 400")); err != nil {
		t.Fatal(err)
	}
	second, _ := store.BeginRun(ctx, "manual", "c", "2026-07-11")
	if _, err := store.ReserveBatch(ctx, second, 0, "second", []int64{id}); err != nil {
		t.Fatalf("failed delivery was not retryable: %v", err)
	}
}
