package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/article"
	"github.com/mosaseka/Discord_ComputerNewsBot/migrations"
	_ "modernc.org/sqlite"
)

const timeFormat = time.RFC3339Nano

var ErrDeliveryConflict = errors.New("one or more article deliveries are already reserved or sent")

type Store struct {
	db   *sql.DB
	path string
}

type Run struct {
	ID        string
	Type      string
	StartedAt time.Time
	ChannelID string
	LocalDate string
}

type RunResult struct {
	Result        string
	FetchedCount  int
	SelectedCount int
	PostedCount   int
	Error         string
}

type Status struct {
	LastRunAt         *time.Time
	LastSuccessAt     *time.Time
	LastError         string
	FetchedCount      int
	SelectedCount     int
	PostedCount       int
	UnknownBatchCount int
	LastUnknownError  string
	DatabasePath      string
}

type FeedStatus struct {
	Name      string
	URL       string
	CheckedAt time.Time
	Status    string
	ItemCount int
	LastError string
}

type Batch struct {
	ID        int64
	RunID     string
	ChannelID string
	LocalDate string
	Index     int
	Nonce     string
	State     string
	MessageID string
	LastError string
}

type SentIdentities struct {
	URLs        map[string]struct{}
	Titles      map[string]struct{}
	SourceGUIDs map[string]struct{}
}

func (s SentIdentities) Contains(a article.Article) bool {
	a.EnsureNormalized()
	if _, ok := s.URLs[a.NormalizedURL]; ok && a.NormalizedURL != "" {
		return true
	}
	if _, ok := s.Titles[a.NormalizedTitle]; ok && a.NormalizedTitle != "" {
		return true
	}
	if a.GUID != "" {
		_, ok := s.SourceGUIDs[sourceGUIDKey(a.Source.Name, a.GUID)]
		return ok
	}
	return false
}

func Open(ctx context.Context, path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is empty")
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file::memory:") {
		dir := filepath.Dir(path)
		if dir != "." && dir != "" {
			if err := osMkdirAll(dir); err != nil {
				return nil, fmt.Errorf("create database directory: %w", err)
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply %s: %w", pragma, err)
		}
	}
	if path != ":memory:" && !strings.HasPrefix(path, "file::memory:") {
		if _, err := db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
			db.Close()
			return nil, fmt.Errorf("enable WAL: %w", err)
		}
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// osMkdirAll is a variable only to keep filesystem failures testable.
var osMkdirAll = func(path string) error { return mkdirAll(path) }

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Path() string { return s.path }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
        version TEXT PRIMARY KEY, checksum TEXT NOT NULL, applied_at TEXT NOT NULL
    )`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(entries)
	for _, name := range entries {
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])
		var existing string
		err = s.db.QueryRowContext(ctx, "SELECT checksum FROM schema_migrations WHERE version = ?", name).Scan(&existing)
		if err == nil {
			if existing != checksum {
				return fmt.Errorf("migration %s checksum changed", name)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read migration %s state: %w", name, err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err = tx.ExecContext(ctx, string(body)); err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version, checksum, applied_at) VALUES(?,?,?)", name, checksum, nowString())
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) BeginRun(ctx context.Context, runType, channelID, localDate string) (Run, error) {
	id, err := randomID()
	if err != nil {
		return Run{}, err
	}
	r := Run{ID: id, Type: runType, StartedAt: time.Now().UTC(), ChannelID: channelID, LocalDate: localDate}
	_, err = s.db.ExecContext(ctx, `INSERT INTO runs(id,run_type,started_at,channel_id,local_date)
        VALUES(?,?,?,?,?)`, r.ID, r.Type, r.StartedAt.Format(timeFormat), nullable(channelID), nullable(localDate))
	if err != nil {
		return Run{}, fmt.Errorf("insert run: %w", err)
	}
	return r, nil
}

func (s *Store) FinishRun(ctx context.Context, id string, result RunResult) error {
	_, err := s.db.ExecContext(ctx, `UPDATE runs SET finished_at=?, result=?, fetched_count=?,
        selected_count=?, posted_count=?, last_error=? WHERE id=?`, nowString(), result.Result,
		result.FetchedCount, result.SelectedCount, result.PostedCount, nullable(sanitizeError(result.Error)), id)
	if err != nil {
		return fmt.Errorf("finish run: %w", err)
	}
	return nil
}

func (s *Store) RecordFeedFetch(ctx context.Context, runID, name, rawURL, status string, count int, fetchErr error) error {
	var summary string
	if fetchErr != nil {
		summary = sanitizeError(fetchErr.Error())
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO feed_fetches
        (run_id,feed_name,feed_url,checked_at,status,item_count,last_error) VALUES(?,?,?,?,?,?,?)`,
		runID, name, rawURL, nowString(), status, count, nullable(summary))
	if err != nil {
		return fmt.Errorf("record feed fetch: %w", err)
	}
	return nil
}

func (s *Store) UpsertArticle(ctx context.Context, a article.Article, score int, breakdown any) (int64, error) {
	a.EnsureNormalized()
	if a.NormalizedURL == "" {
		return 0, errors.New("article normalized URL is empty")
	}
	encoded, err := json.Marshal(breakdown)
	if err != nil {
		return 0, fmt.Errorf("marshal score breakdown: %w", err)
	}
	now := nowString()
	_, err = s.db.ExecContext(ctx, `INSERT INTO articles
        (guid,title,original_url,canonical_url,normalized_url,normalized_title,source,published_at,updated_at,
         first_seen_at,last_seen_at,ranking_score,score_breakdown)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
        ON CONFLICT(normalized_url) DO UPDATE SET
          guid=excluded.guid,title=excluded.title,canonical_url=excluded.canonical_url,
          normalized_title=excluded.normalized_title,source=excluded.source,published_at=excluded.published_at,
          updated_at=excluded.updated_at,last_seen_at=excluded.last_seen_at,
          ranking_score=excluded.ranking_score,score_breakdown=excluded.score_breakdown`,
		nullable(a.GUID), a.Title, a.URL, nullable(a.CanonicalURL), a.NormalizedURL, a.NormalizedTitle,
		a.Source.Name, timePtrString(a.PublishedAt), timePtrString(a.UpdatedAt), now, now, score, string(encoded))
	if err != nil {
		return 0, fmt.Errorf("upsert article: %w", err)
	}
	var id int64
	if err := s.db.QueryRowContext(ctx, "SELECT id FROM articles WHERE normalized_url=?", a.NormalizedURL).Scan(&id); err != nil {
		return 0, fmt.Errorf("find upserted article: %w", err)
	}
	return id, nil
}

func (s *Store) SentArticleURLs(ctx context.Context, channelID, localDate string, allHistory bool) (map[string]struct{}, error) {
	identities, err := s.SentArticleIdentities(ctx, channelID, localDate, allHistory)
	if err != nil {
		return nil, err
	}
	return identities.URLs, nil
}

func (s *Store) SentArticleIdentities(ctx context.Context, channelID, localDate string, allHistory bool) (SentIdentities, error) {
	query := `SELECT d.normalized_url,d.normalized_title,d.source,COALESCE(d.article_guid,'')
		FROM deliveries d
		WHERE d.channel_id=? AND d.state IN ('sent','sending','unknown')`
	args := []any{channelID}
	if !allHistory {
		query += " AND d.local_date=?"
		args = append(args, localDate)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return SentIdentities{}, fmt.Errorf("query sent articles: %w", err)
	}
	defer rows.Close()
	out := SentIdentities{
		URLs: make(map[string]struct{}), Titles: make(map[string]struct{}), SourceGUIDs: make(map[string]struct{}),
	}
	for rows.Next() {
		var normalizedURL, normalizedTitle, source, guid string
		if err := rows.Scan(&normalizedURL, &normalizedTitle, &source, &guid); err != nil {
			return SentIdentities{}, err
		}
		if normalizedURL != "" {
			out.URLs[normalizedURL] = struct{}{}
		}
		if normalizedTitle != "" {
			out.Titles[normalizedTitle] = struct{}{}
		}
		if guid != "" {
			out.SourceGUIDs[sourceGUIDKey(source, guid)] = struct{}{}
		}
	}
	return out, rows.Err()
}

func sourceGUIDKey(source, guid string) string {
	return strings.ToLower(strings.TrimSpace(source)) + "\x00" + strings.TrimSpace(guid)
}

func (s *Store) ClaimDailyBrief(ctx context.Context, channelID, localDate, runID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO daily_briefs(channel_id,local_date,run_id,state,created_at)
        VALUES(?,?,?,'running',?) ON CONFLICT(channel_id,local_date) DO NOTHING`, channelID, localDate, runID, nowString())
	if err != nil {
		return false, fmt.Errorf("claim daily brief: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) CompleteDailyBrief(ctx context.Context, channelID, localDate, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE daily_briefs SET state=?,completed_at=?
        WHERE channel_id=? AND local_date=?`, state, nowString(), channelID, localDate)
	return err
}

func (s *Store) ReserveBatch(ctx context.Context, run Run, index int, nonce string, articleIDs []int64) (Batch, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Batch{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `INSERT INTO delivery_batches
        (run_id,channel_id,local_date,batch_index,nonce,state,created_at) VALUES(?,?,?,?,?,'reserved',?)`,
		run.ID, run.ChannelID, run.LocalDate, index, nonce, nowString())
	if err != nil {
		return Batch{}, fmt.Errorf("reserve batch: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Batch{}, err
	}
	for _, articleID := range articleIDs {
		var deliveryResult sql.Result
		deliveryResult, err = tx.ExecContext(ctx, `INSERT INTO deliveries
			(article_id,article_guid,normalized_url,normalized_title,source,batch_id,run_id,channel_id,local_date,run_type,state)
			SELECT id,guid,normalized_url,normalized_title,source,?,?,?,?,?,'reserved' FROM articles WHERE id=?
			ON CONFLICT(article_id,channel_id,local_date) DO UPDATE SET
				article_guid=excluded.article_guid,normalized_url=excluded.normalized_url,
				normalized_title=excluded.normalized_title,source=excluded.source,
				batch_id=excluded.batch_id,run_id=excluded.run_id,run_type=excluded.run_type,
				state='reserved',sent_at=NULL
			WHERE deliveries.state='failed'`,
			id, run.ID, run.ChannelID, run.LocalDate, run.Type, articleID)
		if err != nil {
			return Batch{}, fmt.Errorf("reserve delivery: %w", err)
		}
		rows, rowsErr := deliveryResult.RowsAffected()
		if rowsErr != nil {
			return Batch{}, fmt.Errorf("inspect delivery reservation: %w", rowsErr)
		}
		if rows != 1 {
			return Batch{}, ErrDeliveryConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return Batch{}, err
	}
	return Batch{ID: id, RunID: run.ID, ChannelID: run.ChannelID, LocalDate: run.LocalDate, Index: index, Nonce: nonce, State: "reserved"}, nil
}

func (s *Store) MarkBatchSending(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE delivery_batches SET state='sending' WHERE id=? AND state='reserved'", id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE deliveries SET state='sending' WHERE batch_id=? AND state='reserved'", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkBatchSent(ctx context.Context, id int64, messageID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowString()
	if _, err = tx.ExecContext(ctx, `UPDATE delivery_batches SET state='sent',message_id=?,sent_at=?,last_error=NULL WHERE id=?`, messageID, now, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET state='sent',sent_at=? WHERE batch_id=?`, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkBatchFailed(ctx context.Context, id int64, state string, errValue error) error {
	if state != "failed" && state != "unknown" {
		return fmt.Errorf("invalid batch failure state %q", state)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `UPDATE delivery_batches SET state=?,last_error=? WHERE id=?`, state, sanitizeError(errValue.Error()), id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET state=? WHERE batch_id=?`, state, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetStatus(ctx context.Context) (Status, error) {
	status := Status{DatabasePath: s.path}
	var last, success sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT
        (SELECT started_at FROM runs ORDER BY started_at DESC LIMIT 1),
        (SELECT finished_at FROM runs WHERE result IN ('success','partial_success') ORDER BY finished_at DESC LIMIT 1),
        COALESCE((SELECT last_error FROM runs WHERE last_error IS NOT NULL ORDER BY started_at DESC LIMIT 1),''),
        COALESCE((SELECT fetched_count FROM runs ORDER BY started_at DESC LIMIT 1),0),
        COALESCE((SELECT selected_count FROM runs ORDER BY started_at DESC LIMIT 1),0),
		COALESCE((SELECT posted_count FROM runs ORDER BY started_at DESC LIMIT 1),0),
		COALESCE((SELECT COUNT(*) FROM delivery_batches WHERE state IN ('unknown','sending')),0),
		COALESCE((SELECT COALESCE(NULLIF(last_error,''),'送信処理中または中断') FROM delivery_batches
			WHERE state IN ('unknown','sending') ORDER BY created_at DESC,id DESC LIMIT 1),'')`).Scan(
		&last, &success, &status.LastError, &status.FetchedCount, &status.SelectedCount, &status.PostedCount,
		&status.UnknownBatchCount, &status.LastUnknownError)
	if err != nil {
		return Status{}, fmt.Errorf("read status: %w", err)
	}
	status.LastRunAt = parseNullTime(last)
	status.LastSuccessAt = parseNullTime(success)
	return status, nil
}

func (s *Store) ListFeedStatuses(ctx context.Context) ([]FeedStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.feed_name,f.feed_url,f.checked_at,f.status,f.item_count,COALESCE(f.last_error,'')
        FROM feed_fetches f WHERE f.id=(SELECT f2.id FROM feed_fetches f2 WHERE f2.feed_name=f.feed_name ORDER BY f2.checked_at DESC,f2.id DESC LIMIT 1)
        ORDER BY f.feed_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FeedStatus
	for rows.Next() {
		var item FeedStatus
		var checked string
		if err := rows.Scan(&item.Name, &item.URL, &checked, &item.Status, &item.ItemCount, &item.LastError); err != nil {
			return nil, err
		}
		item.CheckedAt, _ = time.Parse(timeFormat, checked)
		out = append(out, item)
	}
	return out, rows.Err()
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate run ID: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func timePtrString(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format(timeFormat)
}

func nowString() string { return time.Now().UTC().Format(timeFormat) }

func parseNullTime(value sql.NullString) *time.Time {
	if !value.Valid || value.String == "" {
		return nil
	}
	parsed, err := time.Parse(timeFormat, value.String)
	if err != nil {
		return nil
	}
	return &parsed
}

func sanitizeError(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) > 512 {
		value = string(runes[:511]) + "…"
	}
	return value
}
