package brief

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/article"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/config"
	discordout "github.com/mosaseka/Discord_ComputerNewsBot/internal/discord"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/storage"
)

func TestFixtureRunIsOfflineDeterministicAndAtMostFive(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loc, _ := time.LoadLocation("Asia/Tokyo")
	publisher := &fakePublisher{}
	service := Service{
		Config: config.Config{Location: loc}, Store: store,
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil)), Publisher: publisher,
	}
	dir := filepath.Join("..", "..", "testdata", "feeds")
	result, err := service.Run(ctx, Request{FixtureDir: dir, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FetchedCount < 6 || result.SelectedCount == 0 || result.SelectedCount > 5 || result.PostedCount != 0 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Brief.PriorityTitle == "" || FormatText(result) == "" {
		t.Fatal("fixture run did not produce a briefing")
	}
	if publisher.calls != 0 {
		t.Fatal("dry-run called the Discord publisher")
	}
}

func TestPublishUsesFakeAndStopsAfterAmbiguousPartialFailure(t *testing.T) {
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run, err := store.BeginRun(ctx, RunManual, "channel", "2026-07-11")
	if err != nil {
		t.Fatal(err)
	}
	result := Result{}
	for i := 0; i < 5; i++ {
		now := time.Date(2026, 7, 11, i, 0, 0, 0, time.UTC)
		a := article.Article{Title: fmt.Sprintf("Article %d", i), URL: fmt.Sprintf("https://example.com/%d", i), PublishedAt: &now, Source: article.Source{Name: "test", Weight: 1}}
		id, err := store.UpsertArticle(ctx, a, i, nil)
		if err != nil {
			t.Fatal(err)
		}
		result.Items = append(result.Items, Item{Article: a, ArticleID: id})
		result.Brief.Items = append(result.Brief.Items, discordout.Item{
			Title: a.Title, URL: a.URL, Source: "test", Published: "2026-07-11",
			Summary: strings.Repeat("要", 700), Background: strings.Repeat("背", 600),
			Impact: strings.Repeat("影", 600), Next: strings.Repeat("次", 600),
		})
	}
	result.Brief.PriorityTitle = "Article 0"
	publisher := &fakePublisher{failAt: 2}
	service := &Service{Store: store, Publisher: publisher}
	posted, err := service.publish(ctx, run, result)
	if err == nil || posted <= 0 || posted >= len(result.Items) {
		t.Fatalf("publish() posted=%d err=%v", posted, err)
	}
	identities, queryErr := store.SentArticleIdentities(ctx, "channel", "2026-07-11", false)
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	if len(identities.URLs) <= posted {
		t.Fatalf("ambiguous batch was not suppressed: posted=%d suppressed=%d", posted, len(identities.URLs))
	}
}

type fakePublisher struct {
	calls  int
	failAt int
}

func (p *fakePublisher) Publish(_ context.Context, _ string, _ string, _ []*discordgo.MessageEmbed) (string, error) {
	p.calls++
	if p.failAt > 0 && p.calls == p.failAt {
		return "", errors.New("ambiguous transport failure")
	}
	return fmt.Sprintf("message-%d", p.calls), nil
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
