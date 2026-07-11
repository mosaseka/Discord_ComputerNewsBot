// Package brief orchestrates collection, enrichment, deterministic selection,
// persistence, summarization, and optional Discord delivery.
package brief

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/article"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/config"
	discordout "github.com/mosaseka/Discord_ComputerNewsBot/internal/discord"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/feed"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/fixture"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/htmlparser"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/httpclient"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/ranking"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/security"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/storage"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/summarizer"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/textutil"
)

const (
	RunScheduled = "scheduled"
	RunManual    = "manual"
	RunPreview   = "preview"
	RunOnce      = "once"
	RunDry       = "dry_run"
	RunFixture   = "fixture"
)

type Request struct {
	Type       string
	ChannelID  string
	DryRun     bool
	FixtureDir string
}

type Item struct {
	Article   article.Article
	Score     ranking.ScoreBreakdown
	Summary   summarizer.Result
	ArticleID int64
}

type Result struct {
	RunID         string
	RunType       string
	Brief         discordout.Briefing
	Items         []Item
	FetchedCount  int
	SelectedCount int
	PostedCount   int
	FeedFailures  int
	Window        time.Duration
	Outcome       string
}

type Service struct {
	Config    config.Config
	Feeds     []config.Feed
	Store     *storage.Store
	HTTP      *httpclient.Client
	Publisher discordout.Publisher
	Logger    *slog.Logger
	Now       func() time.Time
}

func (s *Service) Run(ctx context.Context, request Request) (Result, error) {
	if s.Store == nil {
		return Result{}, errors.New("brief service requires storage")
	}
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if request.Type == "" {
		request.Type = RunManual
	}
	now := s.Now()
	var fixtureResult fixture.Result
	var err error
	if request.FixtureDir != "" {
		request.Type = RunFixture
		request.DryRun = true
		fixtureResult, err = fixture.Load(ctx, request.FixtureDir)
		if err != nil {
			loc := s.Config.Location
			if loc == nil {
				loc = time.UTC
			}
			failedRun, beginErr := s.Store.BeginRun(context.WithoutCancel(ctx), RunFixture, request.ChannelID, now.In(loc).Format("2006-01-02"))
			if beginErr == nil {
				finishErr := s.Store.FinishRun(context.WithoutCancel(ctx), failedRun.ID, storage.RunResult{Result: "failure", Error: err.Error()})
				return Result{RunID: failedRun.ID, RunType: RunFixture, Outcome: "failure"}, errors.Join(err, finishErr)
			}
			return Result{}, errors.Join(err, beginErr)
		}
		now = fixtureResult.Now
	}
	loc := s.Config.Location
	if loc == nil {
		loc = time.UTC
	}
	localDate := now.In(loc).Format("2006-01-02")
	run, err := s.Store.BeginRun(ctx, request.Type, request.ChannelID, localDate)
	if err != nil {
		return Result{}, err
	}
	result := Result{RunID: run.ID, RunType: request.Type, Outcome: "running"}
	finishFailure := func(runErr error) (Result, error) {
		result.Outcome = "failure"
		finishErr := s.Store.FinishRun(context.WithoutCancel(ctx), run.ID, storage.RunResult{
			Result: "failure", FetchedCount: result.FetchedCount, SelectedCount: result.SelectedCount,
			PostedCount: result.PostedCount, Error: runErr.Error(),
		})
		var dailyErr error
		if request.Type == RunScheduled {
			dailyErr = s.Store.CompleteDailyBrief(context.WithoutCancel(ctx), request.ChannelID, localDate, "failure")
		}
		return result, errors.Join(runErr, finishErr, dailyErr)
	}

	if request.Type == RunScheduled {
		claimed, claimErr := s.Store.ClaimDailyBrief(ctx, request.ChannelID, localDate, run.ID)
		if claimErr != nil {
			return finishFailure(claimErr)
		}
		if !claimed {
			result.Outcome = "skipped"
			if err := s.Store.FinishRun(ctx, run.ID, storage.RunResult{Result: "skipped"}); err != nil {
				return result, err
			}
			return result, nil
		}
	}

	var articles []article.Article
	if request.FixtureDir != "" {
		articles = fixtureResult.Articles
		if err := s.recordFixtureFeeds(ctx, run.ID, articles); err != nil {
			return finishFailure(err)
		}
	} else {
		articles, result.FeedFailures, err = s.collect(ctx, run.ID)
		if err != nil {
			return finishFailure(err)
		}
	}
	result.FetchedCount = len(articles)
	if len(articles) == 0 {
		return finishFailure(errors.New("all enabled feeds failed or returned no articles"))
	}

	sortArticles(articles)
	articles = article.DeduplicateExact(articles)
	sortArticles(articles)
	if request.FixtureDir == "" && s.Config.ArticleFetchEnabled && s.HTTP != nil {
		s.enrichTopCandidates(ctx, articles, now)
	}
	articles = s.filterSafeArticles(ctx, articles)

	if request.ChannelID != "" {
		allHistory := request.Type == RunScheduled
		suppressed, suppressErr := s.Store.SentArticleIdentities(ctx, request.ChannelID, localDate, allHistory)
		if suppressErr != nil {
			return finishFailure(suppressErr)
		}
		filtered := articles[:0]
		for _, candidate := range articles {
			candidate.EnsureNormalized()
			if !suppressed.Contains(candidate) {
				filtered = append(filtered, candidate)
			}
		}
		articles = filtered
	}

	articleIDs := make(map[string]int64, len(articles))
	for _, candidate := range articles {
		breakdown := ranking.Score(candidate, now)
		id, upsertErr := s.Store.UpsertArticle(ctx, candidate, breakdown.Total, breakdown)
		if upsertErr != nil {
			return finishFailure(upsertErr)
		}
		candidate.EnsureNormalized()
		articleIDs[candidate.NormalizedURL] = id
		s.Logger.Debug("article scored", "source", candidate.Source.Name, "url", candidate.NormalizedURL,
			"total", breakdown.Total, "feed", breakdown.Feed, "freshness", breakdown.Freshness,
			"official", breakdown.Official, "topic", breakdown.Topic, "high_impact", breakdown.HighImpact,
			"penalty", breakdown.Penalty, "matched_terms", breakdown.MatchedTerms)
	}

	selection := ranking.Select(articles, now, 5)
	result.Window = selection.Window
	result.Items = make([]Item, 0, len(selection.Articles))
	result.Brief.Items = make([]discordout.Item, 0, len(selection.Articles))
	for _, selected := range selection.Articles {
		a := selected.Article
		displayURL := s.safeOutputURL(ctx, a.URL, false)
		if displayURL == "" {
			s.Logger.Warn("selected article has no safe public URL", "source", a.Source.Name, "normalized_url", a.NormalizedURL)
			continue
		}
		summaryLinks := make([]summarizer.Link, 0, len(a.Links))
		for _, link := range a.Links {
			summaryLinks = append(summaryLinks, summarizer.Link{Text: link.Text, URL: link.URL})
		}
		summary := summarizer.Summarize(summarizer.Input{Title: a.Title, Body: a.BestText(), Links: summaryLinks})
		next := summary.NextCheck
		if followURL := s.safeOutputURL(ctx, summary.NextCheckURL, false); followURL != "" {
			next += "\n" + followURL
		} else {
			next += "\n" + displayURL
		}
		published := publicationDisplay(a, loc)
		image := s.safeOutputURL(ctx, a.ImageURL, true)
		a.EnsureNormalized()
		item := Item{Article: a, Score: selected.Breakdown, Summary: summary, ArticleID: articleIDs[a.NormalizedURL]}
		result.Items = append(result.Items, item)
		result.Brief.Items = append(result.Brief.Items, discordout.Item{
			Title: a.Title, URL: displayURL, Source: a.Source.Name, Published: published,
			Summary: summary.KeyPoints, Background: summary.TechnicalBackground,
			Impact: summary.DeveloperImpact, Next: next, ImageURL: image,
		})
	}
	result.SelectedCount = len(result.Items)
	if len(result.Items) > 0 {
		top := result.Items[0]
		result.Brief.PriorityTitle = top.Article.Title
		result.Brief.PriorityURL = result.Brief.Items[0].URL
		result.Brief.PriorityReason = priorityReason(top.Score)
	}

	if !request.DryRun && len(result.Items) > 0 {
		if s.Publisher == nil {
			return finishFailure(errors.New("Discord publisher is not configured"))
		}
		posted, publishErr := s.publish(ctx, run, result)
		result.PostedCount = posted
		if publishErr != nil {
			return finishFailure(publishErr)
		}
	}

	outcome := "success"
	if result.FeedFailures > 0 {
		outcome = "partial_success"
	}
	result.Outcome = outcome
	if err := s.Store.FinishRun(ctx, run.ID, storage.RunResult{
		Result: outcome, FetchedCount: result.FetchedCount, SelectedCount: result.SelectedCount, PostedCount: result.PostedCount,
	}); err != nil {
		return result, err
	}
	if request.Type == RunScheduled {
		if err := s.Store.CompleteDailyBrief(ctx, request.ChannelID, localDate, outcome); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (s *Service) collect(ctx context.Context, runID string) ([]article.Article, int, error) {
	if s.HTTP == nil {
		return nil, 0, errors.New("HTTP client is not configured")
	}
	type fetchResult struct {
		feed     config.Feed
		articles []article.Article
		err      error
	}
	resultCh := make(chan fetchResult, len(s.Feeds))
	var wg sync.WaitGroup
	enabled := 0
	for _, configured := range s.Feeds {
		if !configured.Enabled {
			continue
		}
		enabled++
		wg.Add(1)
		go func(item config.Feed) {
			defer wg.Done()
			response, err := s.HTTP.FetchFeed(ctx, item.URL)
			if err != nil {
				resultCh <- fetchResult{feed: item, err: err}
				return
			}
			source := article.Source{Name: item.Name, URL: item.URL, Weight: item.Weight, Language: item.Language, Category: item.Category, IsOfficial: item.IsOfficial}
			parsed, err := feed.NewParser(50).Parse(ctx, strings.NewReader(string(response.Body)), source)
			resultCh <- fetchResult{feed: item, articles: parsed, err: err}
		}(configured)
	}
	go func() { wg.Wait(); close(resultCh) }()
	if enabled == 0 {
		return nil, 0, errors.New("no enabled feeds")
	}
	var articles []article.Article
	failures := 0
	for fetched := range resultCh {
		status := "success"
		if fetched.err != nil {
			status = "failure"
			failures++
			s.Logger.Warn("feed fetch failed", "feed", fetched.feed.Name, "url", fetched.feed.URL, "error", fetched.err)
		}
		if err := s.Store.RecordFeedFetch(ctx, runID, fetched.feed.Name, fetched.feed.URL, status, len(fetched.articles), fetched.err); err != nil {
			return nil, failures, err
		}
		articles = append(articles, fetched.articles...)
	}
	if failures == enabled {
		return nil, failures, errors.New("all enabled feeds failed")
	}
	return articles, failures, nil
}

func (s *Service) enrichTopCandidates(ctx context.Context, articles []article.Article, now time.Time) {
	eligible := make([]article.Article, 0, len(articles))
	for _, candidate := range articles {
		published, ok := candidate.EffectiveTime()
		if !ok || published.After(now.Add(15*time.Minute)) || now.Sub(published) > 168*time.Hour || !ranking.Eligible(candidate) {
			continue
		}
		if textutil.GraphemeCount(strings.TrimSpace(candidate.Body)) >= 160 || candidate.URL == "" {
			continue
		}
		eligible = append(eligible, candidate)
	}
	ranked := ranking.Rank(eligible, now)
	if len(ranked) > 20 {
		ranked = ranked[:20]
	}
	indexByKey := make(map[string]int, len(articles))
	for i := range articles {
		articles[i].EnsureNormalized()
		indexByKey[articles[i].NormalizedURL] = i
	}
	type enriched struct {
		index   int
		article article.Article
	}
	results := make(chan enriched, len(ranked))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	scheduled := make(map[string]struct{}, len(ranked))
	for _, scored := range ranked {
		candidate := scored.Article
		if _, exists := scheduled[candidate.NormalizedURL]; exists {
			continue
		}
		scheduled[candidate.NormalizedURL] = struct{}{}
		index, ok := indexByKey[candidate.NormalizedURL]
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			updated, err := s.enrichOne(ctx, candidate)
			if err != nil {
				s.Logger.Debug("article enrichment skipped", "source", candidate.Source.Name, "url", candidate.NormalizedURL, "error", err)
				return
			}
			results <- enriched{index: index, article: updated}
		}()
	}
	go func() { wg.Wait(); close(results) }()
	for value := range results {
		articles[value.index] = value.article
	}
}

func (s *Service) enrichOne(ctx context.Context, candidate article.Article) (article.Article, error) {
	response, err := s.HTTP.FetchArticle(ctx, candidate.URL)
	if err != nil {
		return candidate, err
	}
	parsed, err := htmlparser.Parse(ctx, bytes.NewReader(response.Body), response.URL, s.Config.MaxArticleBytes)
	if err != nil {
		return candidate, err
	}
	if parsed.Title != "" && candidate.Title == "" {
		candidate.Title = parsed.Title
	}
	if parsed.TextContent != "" {
		candidate.Body = parsed.TextContent
	}
	if parsed.Description != "" && candidate.Description == "" {
		candidate.Description = parsed.Description
	}
	if parsed.PublishedAt != nil && candidate.PublishedAt == nil {
		candidate.PublishedAt = parsed.PublishedAt
	}
	if parsed.ModifiedAt != nil && candidate.UpdatedAt == nil {
		candidate.UpdatedAt = parsed.ModifiedAt
	}
	if parsed.Byline != "" && len(candidate.Authors) == 0 {
		candidate.Authors = []string{parsed.Byline}
	}
	if parsed.SiteName != "" {
		candidate.SiteName = parsed.SiteName
	}
	if canonical := s.safeOutputURL(ctx, parsed.CanonicalURL, false); canonical != "" {
		candidate.CanonicalURL = canonical
		candidate.NormalizedURL = ""
	}
	if image := s.safeOutputURL(ctx, parsed.ImageURL, true); image != "" {
		candidate.ImageURL = image
	}
	for _, link := range parsed.Links {
		candidate.Links = append(candidate.Links, article.Link{Text: link.Text, URL: link.URL})
	}
	candidate.NormalizedTitle = ""
	candidate.EnsureNormalized()
	return candidate, nil
}

func (s *Service) publish(ctx context.Context, run storage.Run, result Result) (int, error) {
	embeds := discordout.BuildEmbeds(result.Brief)
	batches, err := discordout.PackEmbeds(embeds)
	if err != nil {
		return 0, err
	}
	posted := 0
	itemOffset := 0
	for index, batchEmbeds := range batches {
		remaining := len(result.Items) - itemOffset
		articleCount := len(batchEmbeds)
		if articleCount > remaining {
			articleCount = remaining
		}
		ids := make([]int64, 0, articleCount)
		for i := 0; i < articleCount; i++ {
			ids = append(ids, result.Items[itemOffset+i].ArticleID)
		}
		nonce := discordout.Nonce(run.ChannelID, run.LocalDate, run.ID, index)
		reserved, err := s.Store.ReserveBatch(ctx, run, index, nonce, ids)
		if err != nil {
			return posted, err
		}
		if err := s.Store.MarkBatchSending(ctx, reserved.ID); err != nil {
			return posted, err
		}
		messageID, err := s.Publisher.Publish(ctx, run.ChannelID, nonce, batchEmbeds)
		if err != nil {
			state := "failed"
			if discordout.IsAmbiguous(err) {
				state = "unknown"
			}
			_ = s.Store.MarkBatchFailed(context.WithoutCancel(ctx), reserved.ID, state, err)
			return posted, err
		}
		if err := s.Store.MarkBatchSent(ctx, reserved.ID, messageID); err != nil {
			return posted, err
		}
		posted += articleCount
		itemOffset += articleCount
	}
	return posted, nil
}

func (s *Service) safeOutputURL(ctx context.Context, raw string, httpsOnly bool) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Fixture mode must remain completely offline, including metadata URL
	// validation. Its manifest is trusted test input, so syntax-only validation
	// is sufficient there.
	if s.HTTP != nil && s.Config.Mode.Fixture == "" {
		validated, err := s.HTTP.ValidateURL(ctx, raw)
		if err != nil || httpsOnly && validated.Scheme != "https" {
			return ""
		}
		return validated.String()
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || httpsOnly && u.Scheme != "https" {
		return ""
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !security.IsPublicIP(ip) {
		return ""
	}
	return u.String()
}

func (s *Service) filterSafeArticles(ctx context.Context, articles []article.Article) []article.Article {
	result := make([]article.Article, 0, len(articles))
	originAllowed := make(map[string]bool)
	for _, candidate := range articles {
		u, err := url.Parse(strings.TrimSpace(candidate.URL))
		if err != nil || u.User != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			s.Logger.Warn("article rejected because URL syntax is unsafe", "source", candidate.Source.Name)
			continue
		}
		if ip := net.ParseIP(u.Hostname()); ip != nil && !security.IsPublicIP(ip) {
			s.Logger.Warn("article rejected because URL host is not public", "source", candidate.Source.Name)
			continue
		}
		origin := strings.ToLower(u.Scheme + "://" + u.Host)
		allowed, checked := originAllowed[origin]
		if !checked {
			allowed = true
			if s.HTTP != nil && s.Config.Mode.Fixture == "" {
				_, err = s.HTTP.ValidateURL(ctx, candidate.URL)
				allowed = err == nil
			}
			originAllowed[origin] = allowed
		}
		if !allowed {
			s.Logger.Warn("article rejected because URL did not resolve to a public host", "source", candidate.Source.Name, "host", u.Hostname())
			continue
		}
		result = append(result, candidate)
	}
	return result
}

func (s *Service) recordFixtureFeeds(ctx context.Context, runID string, articles []article.Article) error {
	counts := make(map[string]int)
	urls := make(map[string]string)
	for _, item := range articles {
		counts[item.Source.Name]++
		urls[item.Source.Name] = item.Source.URL
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := s.Store.RecordFeedFetch(ctx, runID, name, urls[name], "fixture", counts[name], nil); err != nil {
			return err
		}
	}
	return nil
}

func sortArticles(articles []article.Article) {
	for i := range articles {
		articles[i].EnsureNormalized()
	}
	sort.SliceStable(articles, func(i, j int) bool {
		if articles[i].Source.Name != articles[j].Source.Name {
			return articles[i].Source.Name < articles[j].Source.Name
		}
		if articles[i].NormalizedURL != articles[j].NormalizedURL {
			return articles[i].NormalizedURL < articles[j].NormalizedURL
		}
		return articles[i].NormalizedTitle < articles[j].NormalizedTitle
	})
}

func publicationDisplay(a article.Article, location *time.Location) string {
	if location == nil {
		location = time.UTC
	}
	if a.PublishedAt != nil {
		return a.PublishedAt.In(location).Format("2006-01-02 15:04 MST")
	}
	if a.UpdatedAt != nil {
		return "取得できず（更新日時: " + a.UpdatedAt.In(location).Format("2006-01-02 15:04 MST") + "）"
	}
	return "取得できませんでした"
}

func priorityReason(score ranking.ScoreBreakdown) string {
	type factor struct {
		label string
		value int
	}
	factors := []factor{
		{"重要シグナル", score.HighImpact}, {"新しさ", score.Freshness}, {"技術関連性", score.Topic},
		{"公式情報", score.Official}, {"情報源weight", score.Feed}, {"国内・学生関連", score.DomesticStudent},
		{"本文充足度", score.ContentQuality},
	}
	sort.SliceStable(factors, func(i, j int) bool { return factors[i].value > factors[j].value })
	parts := make([]string, 0, 3)
	for _, item := range factors {
		if item.value <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (+%d)", item.label, item.value))
		if len(parts) == 3 {
			break
		}
	}
	reason := fmt.Sprintf("総合スコア %d。主な加点: %s。", score.Total, strings.Join(parts, "、"))
	if score.Penalty < 0 {
		reason += fmt.Sprintf(" 減点合計: %d。", score.Penalty)
	}
	return reason
}

func FormatText(result Result) string {
	var out strings.Builder
	for i, item := range result.Brief.Items {
		fmt.Fprintf(&out, "%d. %s\n情報源: %s\nURL: %s\n公開日時: %s\n要点: %s\n技術的背景: %s\n開発者・学生への影響: %s\n次に確認すべき点: %s\n",
			i+1, item.Title, item.Source, item.URL, item.Published, item.Summary, item.Background, item.Impact, item.Next)
		if item.ImageURL != "" {
			fmt.Fprintf(&out, "画像URL: %s\n", item.ImageURL)
		}
		out.WriteByte('\n')
	}
	if result.Brief.PriorityTitle != "" {
		fmt.Fprintf(&out, "今日の最優先トピック: %s\n理由: %s\n", result.Brief.PriorityTitle, result.Brief.PriorityReason)
	}
	if len(result.Brief.Items) == 0 {
		out.WriteString("対象期間に投稿可能な記事はありませんでした。\n")
	}
	return out.String()
}
