// Package ranking implements the deterministic scoring and selection policy.
package ranking

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/article"
)

const DefaultLimit = 5

var freshnessWindows = [...]time.Duration{
	48 * time.Hour,
	72 * time.Hour,
	120 * time.Hour,
	168 * time.Hour,
}

// Windows returns a copy of the configured candidate windows, shortest first.
func Windows() []time.Duration {
	return append([]time.Duration(nil), freshnessWindows[:]...)
}

// ScoreBreakdown is persisted and safe to include in debug logs. It contains
// terms and numbers only, never the article body.
type ScoreBreakdown struct {
	Feed            int      `json:"feed"`
	Freshness       int      `json:"freshness"`
	Official        int      `json:"official"`
	Topic           int      `json:"topic"`
	HighImpact      int      `json:"high_impact"`
	DomesticStudent int      `json:"domestic_student"`
	ContentQuality  int      `json:"content_quality"`
	Penalty         int      `json:"penalty"`
	Total           int      `json:"total"`
	MatchedTerms    []string `json:"matched_terms,omitempty"`
	RejectionReason string   `json:"rejection_reason,omitempty"`
}

// ScoredArticle keeps the immutable article and its auditable score together.
type ScoredArticle struct {
	Article   article.Article
	Breakdown ScoreBreakdown
}

// Selection reports the widening window used for the result.
type Selection struct {
	Articles   []ScoredArticle
	Window     time.Duration
	Considered int
}

type termGroup struct {
	name  string
	terms []string
}

var topicGroups = []termGroup{
	{"languages", []string{"go", "golang", "python", "typescript", "javascript", "java", "c++", "c言語", "rust", "ruby", "php", "kotlin", "swift"}},
	{"oss", []string{"oss", "open source", "オープンソース", "github", "repository", "リポジトリ"}},
	{"web_backend", []string{"web", "backend", "バックエンド", "フロントエンド", "http", "api"}},
	{"cloud_infra", []string{"cloud", "クラウド", "kubernetes", "docker", "インフラ", "linux", "サーバー"}},
	{"database", []string{"database", "データベース", "sql", "postgresql", "mysql", "sqlite"}},
	{"network", []string{"network", "ネットワーク", "protocol", "プロトコル", "dns"}},
	{"ai_education", []string{"ai coding", "aiコーディング", "copilot", "code assistant", "コード生成", "情報系教育", "プログラミング", "atcoder", "競技プログラミング"}},
	{"toolchain", []string{"compiler", "コンパイラ", "runtime", "ランタイム", "言語処理系", "sdk", "ツールチェーン"}},
}

var highImpactGroups = []termGroup{
	{"security", []string{"security", "セキュリティ", "vulnerability", "脆弱性", "cve", "advisory", "アドバイザリ"}},
	{"release", []string{"release", "リリース", "バージョンアップ", "正式版", "stable", "rc", "beta", "ベータ"}},
	{"breaking_api", []string{"breaking change", "breaking changes", "破壊的変更", "api change", "api changes", "api変更"}},
	{"lifecycle", []string{"deprecated", "deprecation", "非推奨", "廃止", "eol", "end of life", "サポート終了"}},
	{"runtime_compiler", []string{"compiler", "コンパイラ", "runtime", "ランタイム", "言語処理系"}},
}

var domesticStudentTerms = []string{
	"atcoder", "国内イベント", "国内カンファレンス", "日本", "学生", "学生向け", "コンテスト", "競技プログラミング",
}

// Score computes the fixed formula. now is injected so fixtures and tests do
// not depend on the wall clock.
func Score(a article.Article, now time.Time) ScoreBreakdown {
	haystack := normalizedHaystack(a)
	breakdown := ScoreBreakdown{
		Feed: int(math.Round(a.Source.Weight * 10)),
	}
	if a.Source.IsOfficial {
		breakdown.Official = 12
	}
	if published, ok := a.EffectiveTime(); ok {
		age := now.Sub(published).Hours()
		if age < 0 {
			age = 0
		}
		breakdown.Freshness = max(0, 28-int(math.Floor(age/6)))
	}

	matched := make(map[string]struct{})
	topicCount := matchedGroupCount(haystack, topicGroups, matched)
	if topicCount > 8 {
		topicCount = 8
	}
	breakdown.Topic = topicCount * 2
	highCount := matchedGroupCount(haystack, highImpactGroups, matched)
	if highCount > 4 {
		highCount = 4
	}
	breakdown.HighImpact = highCount * 6
	if terms := matchingTerms(haystack, domesticStudentTerms); len(terms) > 0 {
		breakdown.DomesticStudent = 8
		for _, term := range terms {
			matched[term] = struct{}{}
		}
	}

	bodyLength := utf8.RuneCountInString(strings.TrimSpace(stripMarkupApprox(a.BestText())))
	switch {
	case bodyLength >= 160:
		breakdown.ContentQuality += 4
	case bodyLength >= 80:
		breakdown.ContentQuality += 2
	case bodyLength > 0:
		breakdown.ContentQuality++
	}
	if _, ok := a.EffectiveTime(); ok {
		breakdown.ContentQuality += 2
	}
	if normalizedURL(a) != "" {
		breakdown.ContentQuality += 2
	}

	breakdown.Penalty = penalties(haystack, bodyLength, breakdown)
	if !Eligible(a) {
		breakdown.RejectionReason = "non_japanese_irrelevant"
	}
	breakdown.MatchedTerms = make([]string, 0, len(matched))
	for term := range matched {
		breakdown.MatchedTerms = append(breakdown.MatchedTerms, term)
	}
	sort.Strings(breakdown.MatchedTerms)
	breakdown.Total = breakdown.Feed + breakdown.Freshness + breakdown.Official + breakdown.Topic +
		breakdown.HighImpact + breakdown.DomesticStudent + breakdown.ContentQuality + breakdown.Penalty
	return breakdown
}

// Eligible applies the overseas relevance gate. A blank language is accepted
// for manually-constructed and legacy sources; configured sources are expected
// to declare a language.
func Eligible(a article.Article) bool {
	language := strings.ToLower(strings.TrimSpace(a.Source.Language))
	if language == "" || language == "ja" || strings.HasPrefix(language, "ja-") {
		return true
	}
	haystack := normalizedHaystack(a)
	// A generic use of "release" or "EOL" is not enough by itself. Security
	// terms are explicitly relevant; the other approved areas are represented
	// by topicGroups.
	return hasAnyGroup(haystack, topicGroups) || len(matchingTerms(haystack, highImpactGroups[0].terms)) > 0 || len(matchingTerms(haystack, domesticStudentTerms)) > 0
}

func normalizedHaystack(a article.Article) string {
	return strings.ToLower(norm.NFKC.String(strings.Join([]string{
		a.Title,
		stripMarkupApprox(a.BestText()),
		a.Source.Name,
		a.Source.Category,
	}, "\n")))
}

func matchedGroupCount(haystack string, groups []termGroup, matched map[string]struct{}) int {
	count := 0
	for _, group := range groups {
		terms := matchingTerms(haystack, group.terms)
		if len(terms) == 0 {
			continue
		}
		count++
		for _, term := range terms {
			matched[term] = struct{}{}
		}
	}
	return count
}

func hasAnyGroup(haystack string, groups []termGroup) bool {
	for _, group := range groups {
		if len(matchingTerms(haystack, group.terms)) > 0 {
			return true
		}
	}
	return false
}

func matchingTerms(haystack string, terms []string) []string {
	result := make([]string, 0, len(terms))
	for _, term := range terms {
		if containsTerm(haystack, term) {
			result = append(result, term)
		}
	}
	return result
}

func containsTerm(haystack, term string) bool {
	term = strings.ToLower(term)
	for start := 0; ; {
		index := strings.Index(haystack[start:], term)
		if index < 0 {
			return false
		}
		index += start
		end := index + len(term)
		leftOK := index == 0 || !asciiWordByte(haystack[index-1]) || !asciiWordByte(term[0])
		rightOK := end == len(haystack) || !asciiWordByte(haystack[end]) || !asciiWordByte(term[len(term)-1])
		if leftOK && rightOK {
			return true
		}
		start = index + 1
		if start >= len(haystack) {
			return false
		}
	}
}

func asciiWordByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_'
}

func penalties(haystack string, bodyLength int, breakdown ScoreBreakdown) int {
	penalty := 0
	if containsAny(haystack, []string{"求人", "採用情報", "エンジニア募集", "job opening", "career opportunity"}) {
		penalty -= 30
	}
	if containsAny(haystack, []string{"広告", "アフィリエイト", "affiliate", "sponsored", "スポンサー", "pr記事", "購入はこちら"}) {
		penalty -= 25
	}
	seminar := containsAny(haystack, []string{"セミナー", "ウェビナー", "webinar"})
	promotion := containsAny(haystack, []string{"参加者募集", "申込", "申し込み", "無料開催", "開催します", "register now"})
	if seminar && promotion && breakdown.HighImpact == 0 {
		penalty -= 18
	}
	if containsAny(haystack, []string{"衝撃", "驚愕", "必見", "知らないと損", "完全終了", "今すぐ見る"}) {
		penalty -= 12
	}
	if bodyLength < 40 {
		penalty -= 15
	}
	if breakdown.Topic == 0 && breakdown.HighImpact == 0 && breakdown.DomesticStudent == 0 {
		penalty -= 25
	}
	return penalty
}

func containsAny(haystack string, terms []string) bool {
	return len(matchingTerms(haystack, terms)) > 0
}

func stripMarkupApprox(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	inTag := false
	for _, r := range value {
		switch r {
		case '<':
			inTag = true
			b.WriteByte(' ')
		case '>':
			inTag = false
			b.WriteByte(' ')
		default:
			if !inTag {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// Rank returns all input articles in deterministic score order. Rejected
// articles are retained for diagnostics; Select omits them.
func Rank(articles []article.Article, now time.Time) []ScoredArticle {
	ranked := make([]ScoredArticle, 0, len(articles))
	for _, candidate := range articles {
		candidate.EnsureNormalized()
		ranked = append(ranked, ScoredArticle{Article: candidate, Breakdown: Score(candidate, now)})
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return less(ranked[i], ranked[j])
	})
	return ranked
}

func less(left, right ScoredArticle) bool {
	if left.Breakdown.Total != right.Breakdown.Total {
		return left.Breakdown.Total > right.Breakdown.Total
	}
	lt, lok := left.Article.EffectiveTime()
	rt, rok := right.Article.EffectiveTime()
	if lok != rok {
		return lok
	}
	if lok && !lt.Equal(rt) {
		return lt.After(rt)
	}
	if left.Article.Source.IsOfficial != right.Article.Source.IsOfficial {
		return left.Article.Source.IsOfficial
	}
	if left.Article.NormalizedTitle != right.Article.NormalizedTitle {
		return left.Article.NormalizedTitle < right.Article.NormalizedTitle
	}
	if left.Article.NormalizedURL != right.Article.NormalizedURL {
		return left.Article.NormalizedURL < right.Article.NormalizedURL
	}
	return left.Article.Source.Name < right.Article.Source.Name
}

// Select widens the freshness window until it has limit unique eligible
// articles, or reaches seven days. Publication time is required; update time is
// accepted only through Article.EffectiveTime. More than 15 minutes in the
// future is excluded.
func Select(articles []article.Article, now time.Time, limit int) Selection {
	if limit <= 0 {
		limit = DefaultLimit
	}
	exact := article.DeduplicateExact(articles)
	selection := Selection{}
	for _, window := range freshnessWindows {
		candidates := make([]article.Article, 0, len(exact))
		for _, candidate := range exact {
			published, ok := candidate.EffectiveTime()
			if !ok || published.After(now.Add(15*time.Minute)) {
				continue
			}
			if now.Sub(published) > window || !Eligible(candidate) {
				continue
			}
			candidates = append(candidates, candidate)
		}
		ranked := Rank(candidates, now)
		unique := deduplicateNearScored(ranked)
		selection = Selection{Articles: unique, Window: window, Considered: len(candidates)}
		if len(selection.Articles) >= limit {
			selection.Articles = selection.Articles[:limit]
			return selection
		}
	}
	if len(selection.Articles) > limit {
		selection.Articles = selection.Articles[:limit]
	}
	return selection
}

// PriorityReason explains the highest-ranked item using only positive score
// components. It never extrapolates facts from article prose.
func PriorityReason(scored ScoredArticle) string {
	type contribution struct {
		points int
		order  int
		text   string
	}
	candidates := []contribution{
		{scored.Breakdown.HighImpact, 0, "重要度の高い技術語が一致している"},
		{scored.Breakdown.Topic, 1, "主要なプログラミング分野との関連語が多い"},
		{scored.Breakdown.Official, 2, "公式情報源から発信されている"},
		{scored.Breakdown.DomesticStudent, 3, "日本の開発者・学生向けトピックとの関連語が一致している"},
		{scored.Breakdown.Freshness, 4, "公開・更新日時が新しい"},
		{scored.Breakdown.ContentQuality, 5, "本文・日時・URLの情報が比較的充実している"},
	}
	if scored.Breakdown.Feed > 10 {
		candidates = append(candidates, contribution{scored.Breakdown.Feed - 10, 6, "設定された情報源weightが高い"})
	}
	positive := candidates[:0]
	for _, candidate := range candidates {
		if candidate.points > 0 {
			positive = append(positive, candidate)
		}
	}
	sort.SliceStable(positive, func(i, j int) bool {
		if positive[i].points != positive[j].points {
			return positive[i].points > positive[j].points
		}
		return positive[i].order < positive[j].order
	})
	if len(positive) == 0 {
		return "決定論的ランキングで最も高いスコアとなったためです"
	}
	if len(positive) > 2 {
		positive = positive[:2]
	}
	reasons := make([]string, len(positive))
	for i := range positive {
		reasons[i] = positive[i].text
	}
	if len(reasons) == 1 {
		return reasons[0] + "ためです"
	}
	return strings.Join(reasons, "こと、") + "ことからです"
}

func deduplicateNearScored(ranked []ScoredArticle) []ScoredArticle {
	result := make([]ScoredArticle, 0, len(ranked))
	for _, candidate := range ranked {
		duplicate := false
		for _, selected := range result {
			if article.IsNearDuplicate(candidate.Article, selected.Article) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			result = append(result, candidate)
		}
	}
	return result
}

func normalizedURL(a article.Article) string {
	if a.NormalizedURL != "" {
		return a.NormalizedURL
	}
	candidate := a.CanonicalURL
	if candidate == "" {
		candidate = a.URL
	}
	normalized, _ := article.NormalizeURL(candidate)
	return normalized
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}

// IsMostlyJapanese is a small utility for diagnostics; language configuration
// remains authoritative for eligibility.
func IsMostlyJapanese(value string) bool {
	total, japanese := 0, 0
	for _, r := range value {
		if !unicode.IsLetter(r) {
			continue
		}
		total++
		if unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han) {
			japanese++
		}
	}
	return total > 0 && japanese*2 >= total
}
