package ranking

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/article"
)

func TestScoreFixedFormula(t *testing.T) {
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	published := now.Add(-12 * time.Hour)
	a := article.Article{
		Title:       "Go GitHub Web Cloud Database Network compiler security release API change AtCoder",
		URL:         "https://example.com/news",
		Body:        strings.Repeat("技術本文", 50), // 200 runes
		PublishedAt: &published,
		Source: article.Source{
			Name: "Official", Weight: 1.2, Language: "ja", IsOfficial: true,
		},
	}
	got := Score(a, now)
	if got.Feed != 12 || got.Freshness != 26 || got.Official != 12 {
		t.Fatalf("base score = %#v", got)
	}
	if got.Topic != 16 || got.HighImpact != 24 || got.DomesticStudent != 8 || got.ContentQuality != 8 || got.Penalty != 0 {
		t.Fatalf("component score = %#v", got)
	}
	if got.Total != 106 {
		t.Fatalf("total = %d, want 106 (%#v)", got.Total, got)
	}
	if len(got.MatchedTerms) == 0 {
		t.Fatal("matched terms were not recorded")
	}
}

func TestFreshnessCalculation(t *testing.T) {
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		age  time.Duration
		want int
	}{
		{"new", 0, 28},
		{"five-hours", 5 * time.Hour, 28},
		{"six-hours", 6 * time.Hour, 27},
		{"thirteen-hours", 13 * time.Hour, 26},
		{"old", 8 * 24 * time.Hour, 0},
		{"slightly-future", -10 * time.Minute, 28},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			date := now.Add(-tt.age)
			got := Score(article.Article{PublishedAt: &date}, now).Freshness
			if got != tt.want {
				t.Fatalf("freshness = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestOfficialBonusAndPenalties(t *testing.T) {
	now := time.Now()
	date := now.Add(-time.Hour)
	plain := article.Article{Title: "Go の実装を詳しく解説", URL: "https://example.com", Body: strings.Repeat("本文", 100), PublishedAt: &date, Source: article.Source{Weight: 1, Language: "ja"}}
	official := plain
	official.Source.IsOfficial = true
	if Score(official, now).Total-Score(plain, now).Total != 12 {
		t.Fatal("official information did not receive exactly +12")
	}

	promotional := plain
	promotional.Title = "必見 PR記事 エンジニア求人 無料セミナーを開催します"
	promotional.Body = "申込はこちら"
	got := Score(promotional, now)
	if got.Penalty > -80 {
		t.Fatalf("expected cumulative promotion penalties, got %#v", got)
	}
}

func TestEligibleNonJapaneseGate(t *testing.T) {
	relevant := article.Article{Title: "Python security release", Source: article.Source{Language: "en"}}
	irrelevant := article.Article{Title: "Restaurant opening and travel", Source: article.Source{Language: "en"}}
	if !Eligible(relevant) {
		t.Fatal("relevant overseas article was rejected")
	}
	if Eligible(irrelevant) {
		t.Fatal("irrelevant overseas article was accepted")
	}
	if got := Score(irrelevant, time.Now()).RejectionReason; got == "" {
		t.Fatal("rejection reason was not recorded")
	}
}

func TestRankStableTieBreakers(t *testing.T) {
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	date := now.Add(-time.Hour)
	base := article.Article{Body: strings.Repeat("Go の技術本文です。", 20), PublishedAt: &date, Source: article.Source{Weight: 1, Language: "ja"}}
	a := base
	a.Title, a.URL, a.Source.Name = "Zulu", "https://example.com/b", "z"
	b := base
	b.Title, b.URL, b.Source.Name = "Alpha", "https://example.com/a", "a"
	input := []article.Article{a, b}
	first := Rank(input, now)
	second := Rank(input, now)
	if len(first) != 2 || first[0].Article.Title != "Alpha" {
		t.Fatalf("unexpected tie order: %#v", first)
	}
	for i := range first {
		if first[i].Article.Title != second[i].Article.Title {
			t.Fatal("order changed between runs")
		}
	}

	newer := date.Add(time.Minute)
	a.PublishedAt = &newer
	if got := Rank([]article.Article{b, a}, now)[0].Article.Title; got != "Zulu" {
		t.Fatalf("newer tie-breaker first = %q", got)
	}
}

func TestSelectWidensWindowAndDoesNotInvent(t *testing.T) {
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	ages := []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour, 60 * time.Hour, 100 * time.Hour, 160 * time.Hour}
	titles := []string{
		"Go compiler release", "Python security advisory", "TypeScript API change",
		"Linux network update", "PostgreSQL database release", "AtCoder contest notice",
	}
	articles := make([]article.Article, 0, len(ages))
	for i, age := range ages {
		date := now.Add(-age)
		articles = append(articles, article.Article{
			Title: titles[i], URL: fmt.Sprintf("https://site%d.example/news", i),
			Body: strings.Repeat("技術的な本文です。", 20), PublishedAt: &date,
			Source: article.Source{Weight: 1, Language: "ja"},
		})
	}
	selected := Select(articles, now, 5)
	if selected.Window != 120*time.Hour || len(selected.Articles) != 5 {
		t.Fatalf("selection = window %v, len %d", selected.Window, len(selected.Articles))
	}

	short := Select(articles[:2], now, 5)
	if len(short.Articles) != 2 || short.Window != 168*time.Hour {
		t.Fatalf("short selection invented items: %#v", short)
	}
}

func TestSelectExcludesMissingAndFarFutureDates(t *testing.T) {
	now := time.Now()
	future := now.Add(16 * time.Minute)
	valid := now.Add(-time.Hour)
	input := []article.Article{
		{Title: "Go valid", URL: "https://a.example/x", PublishedAt: &valid, Source: article.Source{Language: "ja"}},
		{Title: "Go future", URL: "https://b.example/x", PublishedAt: &future, Source: article.Source{Language: "ja"}},
		{Title: "Go missing", URL: "https://c.example/x", Source: article.Source{Language: "ja"}},
	}
	got := Select(input, now, 5)
	if len(got.Articles) != 1 || got.Articles[0].Article.Title != "Go valid" {
		t.Fatalf("got = %#v", got)
	}
}

func TestIsMostlyJapanese(t *testing.T) {
	if !IsMostlyJapanese("Goの新しいリリースです") || IsMostlyJapanese("new Go release") {
		t.Fatal("unexpected Japanese detection")
	}
}

func TestWindowsReturnsCopyAndPriorityReasonUsesScoreOnly(t *testing.T) {
	windows := Windows()
	if len(windows) != 4 || windows[0] != 48*time.Hour || windows[3] != 168*time.Hour {
		t.Fatalf("windows = %#v", windows)
	}
	windows[0] = 0
	if Windows()[0] != 48*time.Hour {
		t.Fatal("Windows exposed mutable policy state")
	}

	scored := ScoredArticle{
		Article: article.Article{Title: "秘密の本文タイトル"},
		Breakdown: ScoreBreakdown{
			HighImpact: 24,
			Official:   12,
			Freshness:  10,
		},
	}
	reason := PriorityReason(scored)
	if !strings.Contains(reason, "重要度の高い技術語") || !strings.Contains(reason, "公式情報源") {
		t.Fatalf("reason = %q", reason)
	}
	if strings.Contains(reason, scored.Article.Title) {
		t.Fatalf("reason copied unverified article prose: %q", reason)
	}
}
