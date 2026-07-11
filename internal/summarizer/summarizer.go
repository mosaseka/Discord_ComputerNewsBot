// Package summarizer implements deterministic, extractive summaries for
// Japanese programming news. It does not generate or translate text.
package summarizer

import (
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/textutil"
)

const (
	// Candidate sentence bounds are inclusive.
	MinSentenceGraphemes = 15
	MaxSentenceGraphemes = 220

	// Field limits add up to MaxArticleTextGraphemes. The URL returned in
	// NextCheckURL is metadata rather than reproduced article text.
	MaxKeyPointsGraphemes           = 280
	MaxTechnicalBackgroundGraphemes = 130
	MaxDeveloperImpactGraphemes     = 130
	MaxNextCheckGraphemes           = 60
	MaxArticleTextGraphemes         = 600

	KeyPointsFallback           = "記事本文から要点を抽出できませんでした"
	TechnicalBackgroundFallback = "記事本文から詳細な技術的背景を抽出できませんでした"
	DeveloperImpactFallback     = "開発者への具体的な影響は元記事を確認してください"
	NextCheckFallback           = "リリースノートまたは公式ドキュメントの確認を推奨します"
)

// Link is an article link that may point to primary technical material.
type Link struct {
	Text string
	URL  string
}

// Input is deliberately independent of the article storage model. Body should
// be the best available readability or feed text; an HTML fragment is also
// accepted and stripped before sentence selection.
type Input struct {
	Title string
	Body  string
	Links []Link
}

// Result contains display-ready text plus the separately validated URL for the
// suggested follow-up. QuotedGraphemes counts only text selected from the
// title/body, including repeated selections. TotalTextGraphemes includes the
// generated fallback or instruction strings but excludes NextCheckURL.
type Result struct {
	KeyPoints           string
	TechnicalBackground string
	DeveloperImpact     string
	NextCheck           string
	NextCheckURL        string
	QuotedGraphemes     int
	TotalTextGraphemes  int
}

type candidate struct {
	index       int
	text        string
	normalized  string
	compact     string
	score       int
	identifiers []string
}

var (
	htmlMarkerRE = regexp.MustCompile(`(?i)<(?:!doctype|/?(?:html|body|article|section|main|p|div|br|h[1-6]|ul|ol|li|table|pre|blockquote|span|a|script|style|header|footer|nav))\b`)
	identifierRE = regexp.MustCompile(`(?i)(?:\bCVE-\d{4}-\d{4,7}\b|\bv?\d+\.\d+(?:\.\d+){0,3}(?:[-+][0-9a-z.-]+)?\b|\b(?:API|SDK|CLI)\b)`)
)

// These dictionaries are intentionally fixed and exact. Matching is performed
// against NFKC, lower-case text and does not use stemming or fuzzy expansion.
var technicalTerms = []string{
	"api", "aws", "azure", "c", "c++", "cli", "cloud", "compiler",
	"container", "database", "db", "docker", "github", "gitlab", "go",
	"golang", "google cloud", "java", "javascript", "kubernetes", "linux",
	"mysql", "network", "node.js", "open source", "oss", "postgresql",
	"python", "release", "runtime", "rust", "sdk", "security", "sql",
	"typescript", "windows", "アルゴリズム", "オープンソース", "クラウド",
	"コンテナ", "コンパイラ", "セキュリティ", "データベース", "ネットワーク",
	"フレームワーク", "プログラミング", "ランタイム", "リリース", "脆弱性",
}

var backgroundTerms = []string{
	"api", "sdk", "アーキテクチャ", "アルゴリズム", "暗号", "依存関係",
	"互換性", "原因", "基盤", "権限", "仕組み", "仕様", "実装", "認証",
	"設計", "データベース", "内部", "背景", "パフォーマンス", "プロトコル",
	"コンパイラ", "ランタイム", "脆弱性",
}

var impactTerms = []string{
	"breaking change", "eol", "アップグレード", "影響", "開発者", "更新が必要",
	"サポート終了", "障害", "対策", "対応が必要", "注意", "停止", "できなくな",
	"廃止", "破壊的変更", "非推奨", "変更する必要", "移行", "ユーザー", "利用者",
	"リスク", "修正", "互換性",
}

var advertisementPhrases = []string{
	"[pr]", "【pr】", "pr times", "この記事は広告", "広告です", "広告掲載",
	"スポンサー提供", "スポンサーリンク", "アフィリエイト", "今すぐお申し込み",
	"今すぐ登録", "無料登録はこちら", "購入はこちら", "キャンペーン実施中",
	"セミナーにお申し込み", "求人へのご応募", "エントリーはこちら",
}

// Summarize selects article sentences using only fixed dictionaries and stable
// tie-breakers.
func Summarize(input Input) Result {
	candidates := buildCandidates(input.Title, input.Body)
	keyCandidates := selectKeyPoints(candidates)

	selected := make(map[int]bool, len(keyCandidates))
	for _, item := range keyCandidates {
		selected[item.index] = true
	}

	keyPoints, keyExtracted := renderKeyPoints(keyCandidates)
	if keyPoints == "" {
		keyPoints = KeyPointsFallback
	}

	background, backgroundCandidate, backgroundExtracted := selectSpecialSentence(candidates, backgroundTerms, selected)
	if !backgroundExtracted {
		background = TechnicalBackgroundFallback
	}
	background = limitField(background, MaxTechnicalBackgroundGraphemes)

	impactExcluded := cloneIndexSet(selected)
	if backgroundExtracted {
		impactExcluded[backgroundCandidate.index] = true
	}
	impact, _, impactExtracted := selectSpecialSentence(candidates, impactTerms, impactExcluded)
	if !impactExtracted {
		impact = DeveloperImpactFallback
	}
	impact = limitField(impact, MaxDeveloperImpactGraphemes)

	nextCheck, nextCheckURL := SelectNextCheck(input.Links)
	nextCheck = limitField(nextCheck, MaxNextCheckGraphemes)

	result := Result{
		KeyPoints:           keyPoints,
		TechnicalBackground: background,
		DeveloperImpact:     impact,
		NextCheck:           nextCheck,
		NextCheckURL:        nextCheckURL,
	}
	if keyExtracted {
		result.QuotedGraphemes += textutil.GraphemeCount(result.KeyPoints)
	}
	if backgroundExtracted {
		result.QuotedGraphemes += textutil.GraphemeCount(result.TechnicalBackground)
	}
	if impactExtracted {
		result.QuotedGraphemes += textutil.GraphemeCount(result.DeveloperImpact)
	}
	result.TotalTextGraphemes = textutil.GraphemeCount(result.KeyPoints) +
		textutil.GraphemeCount(result.TechnicalBackground) +
		textutil.GraphemeCount(result.DeveloperImpact) +
		textutil.GraphemeCount(result.NextCheck)
	return result
}

// SplitSentences splits on Japanese full stops, question/exclamation marks,
// their ASCII equivalents, and line boundaries. Delimiters are preserved.
func SplitSentences(text string) []string {
	text = strings.ToValidUTF8(text, "\uFFFD")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	runes := []rune(text)
	result := make([]string, 0, 8)
	var current strings.Builder

	flush := func() {
		sentence := textutil.CollapseWhitespace(current.String())
		current.Reset()
		if sentence != "" {
			result = append(result, sentence)
		}
	}

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if r == '\n' {
			flush()
			continue
		}
		current.WriteRune(r)
		if !isSentenceTerminator(r) {
			continue
		}
		for i+1 < len(runes) && isSentenceTerminator(runes[i+1]) {
			i++
			current.WriteRune(runes[i])
		}
		flush()
	}
	flush()
	return result
}

// CandidateSentences applies the fixed 15-220 grapheme eligibility window.
func CandidateSentences(text string) []string {
	if htmlMarkerRE.MatchString(text) {
		text = textutil.StripHTML(text)
	}

	sentences := SplitSentences(text)
	result := make([]string, 0, len(sentences))
	for _, sentence := range sentences {
		length := textutil.GraphemeCount(sentence)
		if length >= MinSentenceGraphemes && length <= MaxSentenceGraphemes {
			result = append(result, sentence)
		}
	}
	return result
}

func isSentenceTerminator(r rune) bool {
	switch r {
	case '。', '！', '？', '!', '?':
		return true
	default:
		return false
	}
}

func buildCandidates(title, body string) []candidate {
	sentences := CandidateSentences(body)
	result := make([]candidate, 0, len(sentences))
	for index, sentence := range sentences {
		if isURLOnly(sentence) || isAdvertisement(sentence) {
			continue
		}
		normalized := textutil.NormalizeForComparison(sentence)
		item := candidate{
			index:       index,
			text:        sentence,
			normalized:  normalized,
			compact:     compactComparison(normalized),
			identifiers: uniqueIdentifiers(normalized),
		}
		item.score = scoreCandidate(item, title)
		result = append(result, item)
	}
	return result
}

func scoreCandidate(item candidate, title string) int {
	titleNormalized := textutil.NormalizeForComparison(title)
	titleCompact := compactComparison(titleNormalized)
	score := int(diceCoefficient(gramSet(titleCompact), gramSet(item.compact))*24 + 0.5)

	if textutil.GraphemeCount(item.compact) >= 8 && textutil.GraphemeCount(titleCompact) >= 8 &&
		(strings.Contains(item.compact, titleCompact) || strings.Contains(titleCompact, item.compact)) {
		score += 8
	}

	termMatches := countTerms(item.normalized, technicalTerms)
	if termMatches > 6 {
		termMatches = 6
	}
	score += termMatches * 3

	identifierCount := len(item.identifiers)
	if identifierCount > 3 {
		identifierCount = 3
	}
	score += identifierCount * 8
	for _, identifier := range item.identifiers {
		if strings.Contains(titleNormalized, identifier) {
			score += 4
		}
	}

	positionScore := 10 - item.index
	if positionScore > 0 {
		score += positionScore
	}
	length := textutil.GraphemeCount(item.text)
	if length >= 30 && length <= 160 {
		score += 3
	}
	return score
}

func selectKeyPoints(candidates []candidate) []candidate {
	ordered := append([]candidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].score != ordered[j].score {
			return ordered[i].score > ordered[j].score
		}
		if ordered[i].index != ordered[j].index {
			return ordered[i].index < ordered[j].index
		}
		if ordered[i].normalized != ordered[j].normalized {
			return ordered[i].normalized < ordered[j].normalized
		}
		return ordered[i].text < ordered[j].text
	})

	selected := make([]candidate, 0, 2)
	for _, item := range ordered {
		duplicate := false
		for _, existing := range selected {
			if nearDuplicate(item.compact, existing.compact) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		selected = append(selected, item)
		if len(selected) == 2 {
			break
		}
	}

	sort.Slice(selected, func(i, j int) bool {
		return selected[i].index < selected[j].index
	})
	return selected
}

func renderKeyPoints(selected []candidate) (string, bool) {
	if len(selected) == 0 {
		return "", false
	}
	parts := make([]string, 0, len(selected))
	for _, item := range selected {
		parts = append(parts, item.text)
	}
	return limitField(strings.Join(parts, "\n"), MaxKeyPointsGraphemes), true
}

func selectSpecialSentence(candidates []candidate, terms []string, excluded map[int]bool) (string, candidate, bool) {
	if item, ok := bestTermCandidate(candidates, terms, excluded); ok {
		return item.text, item, true
	}
	if len(excluded) > 0 {
		if item, ok := bestTermCandidate(candidates, terms, nil); ok {
			return item.text, item, true
		}
	}
	return "", candidate{}, false
}

func bestTermCandidate(candidates []candidate, terms []string, excluded map[int]bool) (candidate, bool) {
	bestScore := -1
	var best candidate
	found := false
	for _, item := range candidates {
		if excluded != nil && excluded[item.index] {
			continue
		}
		matches := countTerms(item.normalized, terms)
		if matches == 0 {
			continue
		}
		score := matches*100 + len(item.identifiers)*10
		position := 10 - item.index
		if position > 0 {
			score += position
		}
		if !found || score > bestScore || (score == bestScore && item.index < best.index) ||
			(score == bestScore && item.index == best.index && item.normalized < best.normalized) {
			best = item
			bestScore = score
			found = true
		}
	}
	return best, found
}

func cloneIndexSet(input map[int]bool) map[int]bool {
	result := make(map[int]bool, len(input)+1)
	for key, value := range input {
		result[key] = value
	}
	return result
}

func countTerms(normalized string, terms []string) int {
	count := 0
	for _, term := range terms {
		if containsExactTerm(normalized, term) {
			count++
		}
	}
	return count
}

func containsExactTerm(text, term string) bool {
	term = textutil.NormalizeForComparison(term)
	if term == "" {
		return false
	}
	if !isASCIIExpression(term) {
		return strings.Contains(text, term)
	}

	searchFrom := 0
	for searchFrom <= len(text)-len(term) {
		relative := strings.Index(text[searchFrom:], term)
		if relative < 0 {
			return false
		}
		start := searchFrom + relative
		end := start + len(term)
		beforeOK := start == 0 || !isASCIIWordByte(text[start-1])
		afterOK := end == len(text) || !isASCIIWordByte(text[end])
		if beforeOK && afterOK {
			return true
		}
		searchFrom = start + 1
	}
	return false
}

func isASCIIExpression(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

func isASCIIWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

func uniqueIdentifiers(normalized string) []string {
	matches := identifierRE.FindAllString(normalized, -1)
	seen := make(map[string]struct{}, len(matches))
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		match = textutil.NormalizeForComparison(match)
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		result = append(result, match)
	}
	sort.Strings(result)
	return result
}

func compactComparison(s string) string {
	s = textutil.NormalizeForComparison(s)
	var result strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			result.WriteRune(r)
		}
	}
	return result.String()
}

func gramSet(s string) map[string]struct{} {
	clusters := textutil.SplitGraphemes(s)
	result := make(map[string]struct{}, len(clusters))
	if len(clusters) == 1 {
		result[clusters[0]] = struct{}{}
		return result
	}
	for i := 0; i+1 < len(clusters); i++ {
		result[clusters[i]+clusters[i+1]] = struct{}{}
	}
	return result
}

func diceCoefficient(left, right map[string]struct{}) float64 {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	intersection := 0
	for gram := range left {
		if _, ok := right[gram]; ok {
			intersection++
		}
	}
	return 2 * float64(intersection) / float64(len(left)+len(right))
}

func nearDuplicate(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	leftLength := textutil.GraphemeCount(left)
	rightLength := textutil.GraphemeCount(right)
	shorter := left
	longer := right
	shorterLength := leftLength
	if leftLength > rightLength {
		shorter, longer = right, left
		shorterLength = rightLength
	}
	if shorterLength >= 12 && strings.Contains(longer, shorter) {
		return true
	}
	return diceCoefficient(gramSet(left), gramSet(right)) >= 0.84
}

func isURLOnly(sentence string) bool {
	trimmed := strings.Trim(textutil.CollapseWhitespace(sentence), " \t\r\n。！？!?.,、")
	if trimmed == "" {
		return false
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return false
	}
	for _, field := range fields {
		parsed, err := url.ParseRequestURI(strings.Trim(field, "()[]<>"))
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return false
		}
	}
	return true
}

func isAdvertisement(sentence string) bool {
	normalized := textutil.NormalizeForComparison(sentence)
	for _, phrase := range advertisementPhrases {
		if strings.Contains(normalized, textutil.NormalizeForComparison(phrase)) {
			return true
		}
	}
	return false
}

func limitField(s string, limit int) string {
	s = strings.TrimSpace(s)
	return textutil.TruncateGraphemesWithSuffix(s, limit, "…")
}

type nextCheckKind struct {
	priority int
	label    string
	anchors  []string
	paths    []string
}

var nextCheckKinds = []nextCheckKind{
	{800, "公式ドキュメントの確認を推奨します", []string{"公式ドキュメント", "ドキュメント", "documentation", "api reference", "reference"}, []string{"/docs/", "/doc/", "/documentation/", "/reference/"}},
	{760, "リリースノートの確認を推奨します", []string{"リリースノート", "release note", "release notes", "releases"}, []string{"/releases/", "/release-notes/", "/release_notes/"}},
	{740, "移行ガイドの確認を推奨します", []string{"移行ガイド", "migration guide", "upgrade guide"}, []string{"/migration/", "/migrate/", "/upgrade-guide/"}},
	{720, "変更履歴の確認を推奨します", []string{"変更履歴", "changelog", "change log"}, []string{"/changelog/", "/changes/"}},
	{700, "セキュリティアドバイザリの確認を推奨します", []string{"セキュリティアドバイザリ", "security advisory", "advisory"}, []string{"/advisories/", "/advisory/", "/security/"}},
	{680, "Issueの確認を推奨します", []string{"issue", "課題"}, []string{"/issues/", "/issue/"}},
	{660, "Pull Requestの確認を推奨します", []string{"pull request", "プルリクエスト", "pr"}, []string{"/pull/", "/pulls/", "/merge_requests/"}},
	{600, "リポジトリの確認を推奨します", []string{"repository", "リポジトリ", "source code", "ソースコード"}, nil},
}

// SelectNextCheck chooses the highest-priority safe public HTTP(S) link using
// fixed anchor/path terms. It returns the exact fallback and an empty URL when
// no link qualifies.
func SelectNextCheck(links []Link) (message string, targetURL string) {
	type rankedLink struct {
		index    int
		priority int
		kind     nextCheckKind
		anchor   string
		url      string
	}

	var ranked []rankedLink
	for index, link := range links {
		parsed, safeURL, ok := parsePublicHTTPURL(link.URL)
		if !ok {
			continue
		}
		anchor := textutil.CollapseWhitespace(strings.ToValidUTF8(link.Text, "\uFFFD"))
		kind, priority, ok := classifyNextCheck(anchor, parsed)
		if !ok {
			continue
		}
		ranked = append(ranked, rankedLink{index, priority, kind, anchor, safeURL})
	}

	if len(ranked) == 0 {
		return NextCheckFallback, ""
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].priority != ranked[j].priority {
			return ranked[i].priority > ranked[j].priority
		}
		if ranked[i].index != ranked[j].index {
			return ranked[i].index < ranked[j].index
		}
		return ranked[i].url < ranked[j].url
	})

	chosen := ranked[0]
	message = chosen.kind.label
	if chosen.anchor != "" && !isURLOnly(chosen.anchor) {
		suffix := "を確認してください"
		available := MaxNextCheckGraphemes - textutil.GraphemeCount(suffix)
		anchor := textutil.TruncateGraphemesWithSuffix(chosen.anchor, available, "…")
		message = anchor + suffix
	}
	return limitField(message, MaxNextCheckGraphemes), chosen.url
}

func classifyNextCheck(anchor string, parsed *url.URL) (nextCheckKind, int, bool) {
	normalizedAnchor := textutil.NormalizeForComparison(anchor)
	normalizedPath := textutil.NormalizeForComparison(parsed.EscapedPath())
	bestPriority := -1
	var best nextCheckKind
	for _, kind := range nextCheckKinds {
		matched := matchesAnchorTerm(normalizedAnchor, kind.anchors) || matchesAny(normalizedPath, kind.paths)
		if kind.priority == 600 && looksLikeRepositoryURL(parsed) {
			matched = true
		}
		if matched && kind.priority > bestPriority {
			best = kind
			bestPriority = kind.priority
		}
	}
	return best, bestPriority, bestPriority >= 0
}

func matchesAnchorTerm(text string, terms []string) bool {
	for _, term := range terms {
		if containsExactTerm(text, term) {
			return true
		}
	}
	return false
}

func matchesAny(text string, terms []string) bool {
	for _, term := range terms {
		if strings.Contains(text, textutil.NormalizeForComparison(term)) {
			return true
		}
	}
	return false
}

func looksLikeRepositoryURL(parsed *url.URL) bool {
	host := strings.ToLower(parsed.Hostname())
	if host != "github.com" && host != "gitlab.com" && host != "codeberg.org" && host != "bitbucket.org" {
		return false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	return !(host == "github.com" && (parts[0] == "blog" || parts[0] == "topics" || parts[0] == "marketplace"))
}

func parsePublicHTTPURL(raw string) (*url.URL, string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 1024 {
		return nil, "", false
	}
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.User != nil || parsed.Host == "" {
		return nil, "", false
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", false
	}

	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return nil, "", false
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
			ip.IsMulticast() || ip.IsUnspecified() {
			return nil, "", false
		}
	}
	return parsed, parsed.String(), true
}
