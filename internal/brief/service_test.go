package brief

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/article"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/config"
	discordout "github.com/mosaseka/Discord_ComputerNewsBot/internal/discord"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/httpclient"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/security"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/storage"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/summarizer"
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
	client, err := httpclient.New(httpclient.DefaultConfig(), security.NewPolicy(briefResolver(func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("fixture performed DNS resolution")
		return nil, nil
	}), nil))
	if err != nil {
		t.Fatal(err)
	}
	service := Service{
		Config: config.Config{Location: loc, GeminiEnabled: true}, Store: store, HTTP: client,
		Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil)), Publisher: publisher,
		Gemini: fakeGemini(func(summarizer.GeminiInput) (summarizer.Result, error) {
			t.Fatal("fixture called Gemini")
			return summarizer.Result{}, nil
		}),
	}
	dir := filepath.Join("..", "..", "testdata", "feeds")
	result, err := service.Run(ctx, Request{FixtureDir: dir, DryRun: true})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.FetchedCount < 6 || result.SelectedCount == 0 || result.SelectedCount > 5 || result.PostedCount != 0 {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.Brief.PriorityTitle == "" || !strings.Contains(FormatText(result), "抽出型要約") {
		t.Fatal("fixture run did not produce a briefing")
	}
	if publisher.calls != 0 {
		t.Fatal("dry-run called the Discord publisher")
	}
}

func TestGeminiSummarizesOnlySelectedArticlesAndFallsBackPerItem(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 24, 8, 0, 0, 0, time.UTC)
	titles := []string{
		"Goコンパイラの診断メッセージ更新", "Rust標準ライブラリに新しい型",
		"Kubernetesの運用ガイド公開", "Linuxファイルシステムの修正",
		"Pythonランタイムのメモリ改善", "Gitの認証設定を変更", "PostgreSQLのクエリ計画改善",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, `<rss version="2.0"><channel><title>Test</title>`)
		for i, title := range titles {
			fmt.Fprintf(w, `<item><title>%s</title><link>https://articles.test/news/%d</link><pubDate>%s</pubDate><description>%s。設定に関わる変更点を利用者向けに解説しています。</description></item>`,
				title, i, now.Add(-time.Duration(i+1)*time.Hour).Format(time.RFC1123Z), title)
		}
		fmt.Fprint(w, "</channel></rss>")
	}))
	defer server.Close()
	local, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resolver := briefResolver(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "feed.test" && host != "articles.test" {
			return nil, errors.New("unexpected host")
		}
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	})
	client, err := httpclient.New(httpclient.Config{MinHostDelay: time.Millisecond}, security.NewPolicy(resolver, localDialer{address: local.Host}))
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "bot.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var calls []string
	publisher := &fakePublisher{}
	service := Service{
		Config: config.Config{GeminiEnabled: true, Location: time.UTC}, Store: store, HTTP: client, Publisher: publisher,
		Feeds: []config.Feed{{Name: "Test", URL: "http://feed.test/feed", Enabled: true, Weight: 1, Language: "ja"}},
		Now:   func() time.Time { return now },
		Gemini: fakeGemini(func(input summarizer.GeminiInput) (summarizer.Result, error) {
			calls = append(calls, input.Title)
			if !input.FeedOnly || !strings.Contains(input.Text, "利用者向け") {
				t.Fatal("expected RSS excerpt in Gemini input")
			}
			switch len(calls) {
			case 2:
				return summarizer.Result{}, &httpclient.StatusError{StatusCode: http.StatusTooManyRequests}
			case 4:
				return summarizer.Result{}, context.DeadlineExceeded
			default:
				return summarizer.Result{KeyPoints: "生成の要点", DeveloperImpact: "利用者への影響", NextCheck: "設定を確認"}, nil
			}
		}),
	}
	run := func(request Request, want int) Result {
		t.Helper()
		calls = nil
		result, err := service.Run(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != want || result.SelectedCount != want || result.FetchedCount != len(titles) {
			t.Fatalf("Gemini calls = %d, selected = %d, fetched = %d, want %d/%d/%d", len(calls), result.SelectedCount, result.FetchedCount, want, want, len(titles))
		}
		embeds := discordout.BuildEmbeds(result.Brief)
		for i, item := range result.Brief.Items {
			if calls[i] != item.Title || item.Generated != (i != 1 && i != 3) ||
				embeds[i].Footer.Text != item.SummaryLabel() || embeds[i].URL != item.URL || !strings.HasSuffix(item.Next, item.URL) ||
				item.Published == "" {
				t.Fatalf("unexpected summary integration at index %d", i)
			}
			wantKeyPoints := "生成の要点"
			if !item.Generated {
				wantKeyPoints = summarizer.Summarize(summarizer.Input{
					Title: item.Title, Body: result.Items[i].Article.BestText(),
				}).KeyPoints
			}
			if item.Summary != wantKeyPoints {
				t.Fatalf("unexpected summary at index %d", i)
			}
		}
		return result
	}
	dry := run(Request{Type: RunDry, ChannelID: "channel", DryRun: true}, 5)
	if publisher.calls != 0 || !strings.Contains(FormatText(dry), "Gemini生成要約") || !strings.Contains(FormatText(dry), "抽出型要約") {
		t.Fatal("dry-run did not display both methods without publishing")
	}
	posted := run(Request{Type: RunManual, ChannelID: "channel"}, 5)
	if posted.PostedCount != 5 || publisher.calls == 0 {
		t.Fatal("briefing was not posted after per-item fallback")
	}
	preview := run(Request{Type: RunPreview, ChannelID: "channel", DryRun: true}, 2)
	for _, item := range preview.Brief.Items {
		for _, prior := range posted.Brief.Items {
			if item.URL == prior.URL {
				t.Fatal("Gemini was called for a previously posted article")
			}
		}
	}
}

type fakeGemini func(summarizer.GeminiInput) (summarizer.Result, error)

func (f fakeGemini) Summarize(_ context.Context, input summarizer.GeminiInput) (summarizer.Result, error) {
	return f(input)
}

type briefResolver func(context.Context, string) ([]net.IPAddr, error)

func (r briefResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return r(ctx, host)
}

type localDialer struct{ address string }

func (d localDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, d.address)
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

func TestFixtureSafetyRejectsLocalhostWithoutDNS(t *testing.T) {
	service := &Service{Logger: slog.New(slog.NewTextHandler(testWriter{t}, nil))}
	for _, rawURL := range []string{
		"http://localhost/article",
		"https://news.localhost/article",
		"http://127.0.0.1/article",
		"http://[::1]/article",
	} {
		t.Run(rawURL, func(t *testing.T) {
			input := []article.Article{{Title: "unsafe", URL: rawURL, Source: article.Source{Name: "fixture"}}}
			if got := service.filterSafeArticles(context.Background(), input, true); len(got) != 0 {
				t.Fatalf("filterSafeArticles() retained unsafe fixture URL %q", rawURL)
			}
			if got := service.safeOutputURL(context.Background(), rawURL, false, true); got != "" {
				t.Fatalf("safeOutputURL() = %q, want empty", got)
			}
		})
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
