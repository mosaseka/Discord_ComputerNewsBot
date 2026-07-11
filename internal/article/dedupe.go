package article

import (
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	cvePattern     = regexp.MustCompile(`(?i)\bCVE[- ]?\d{4}[- ]?\d{4,}\b`)
	versionPattern = regexp.MustCompile(`(?i)(?:\bv|\bversion\s*|\bver\.?\s*|バージョン\s*)?\d+(?:\.\d+){1,3}(?:[-+][0-9a-z.-]+)?`)
)

// DiceSimilarity computes the Sørensen-Dice coefficient over rune bigrams.
// Duplicate bigrams are counted, which prevents repeated text from receiving
// an artificially perfect match.
func DiceSimilarity(left, right string) float64 {
	left = NormalizeTitle(left)
	right = NormalizeTitle(right)
	if left == right {
		if left == "" {
			return 0
		}
		return 1
	}
	lb := runeBigrams(left)
	rb := runeBigrams(right)
	if len(lb) == 0 || len(rb) == 0 {
		return 0
	}
	counts := make(map[string]int, len(lb))
	for _, bigram := range lb {
		counts[bigram]++
	}
	intersection := 0
	for _, bigram := range rb {
		if counts[bigram] > 0 {
			intersection++
			counts[bigram]--
		}
	}
	return 2 * float64(intersection) / float64(len(lb)+len(rb))
}

func runeBigrams(value string) []string {
	runes := []rune(value)
	if len(runes) < 2 {
		return nil
	}
	result := make([]string, 0, len(runes)-1)
	for i := 0; i+1 < len(runes); i++ {
		result = append(result, string(runes[i:i+2]))
	}
	return result
}

// IsExactDuplicate matches the stable identifiers described by the ingestion
// contract. Empty identifiers never match.
func IsExactDuplicate(left, right Article) bool {
	if equalNonEmpty(strings.TrimSpace(left.GUID), strings.TrimSpace(right.GUID)) && sameSource(left.Source, right.Source) {
		return true
	}
	if equalURLIdentity(left.CanonicalURL, right.CanonicalURL) {
		return true
	}
	ln := normalizedURLIdentity(left)
	rn := normalizedURLIdentity(right)
	if equalNonEmpty(ln, rn) {
		return true
	}
	lt := left.NormalizedTitle
	if lt == "" {
		lt = NormalizeTitle(left.Title)
	}
	rt := right.NormalizedTitle
	if rt == "" {
		rt = NormalizeTitle(right.Title)
	}
	return equalNonEmpty(lt, rt)
}

func sameSource(left, right Source) bool {
	leftURL, leftErr := NormalizeURL(left.URL)
	rightURL, rightErr := NormalizeURL(right.URL)
	if leftErr == nil && rightErr == nil && leftURL != "" && rightURL != "" {
		return leftURL == rightURL
	}
	return strings.EqualFold(strings.TrimSpace(left.Name), strings.TrimSpace(right.Name))
}

func equalURLIdentity(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	ln, leftErr := NormalizeURL(left)
	rn, rightErr := NormalizeURL(right)
	return leftErr == nil && rightErr == nil && ln == rn
}

func equalNonEmpty(left, right string) bool {
	return left != "" && right != "" && left == right
}

func normalizedURLIdentity(a Article) string {
	if a.NormalizedURL != "" {
		return a.NormalizedURL
	}
	raw := a.CanonicalURL
	if raw == "" {
		raw = a.URL
	}
	normalized, _ := NormalizeURL(raw)
	return normalized
}

// IsNearDuplicate applies the domain/time thresholds after protecting titles
// that name different CVEs or software versions.
func IsNearDuplicate(left, right Article) bool {
	if identifierConflict(left.Title, right.Title) {
		return false
	}
	leftTime, lok := left.EffectiveTime()
	rightTime, rok := right.EffectiveTime()
	if !lok || !rok {
		return false
	}
	delta := leftTime.Sub(rightTime)
	if delta < 0 {
		delta = -delta
	}
	sameDomain := articleHostname(left) != "" && articleHostname(left) == articleHostname(right)
	threshold := 0.90
	maxDelta := 48 * time.Hour
	if sameDomain {
		threshold = 0.78
		maxDelta = 72 * time.Hour
	}
	if delta > maxDelta {
		return false
	}
	return DiceSimilarity(left.Title, right.Title) >= threshold
}

func articleHostname(a Article) string {
	raw := a.CanonicalURL
	if raw == "" {
		raw = a.NormalizedURL
	}
	if raw == "" {
		raw = a.URL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

func identifierConflict(left, right string) bool {
	lids := extractIdentifiers(left)
	rids := extractIdentifiers(right)
	if (len(lids) == 0) != (len(rids) == 0) {
		return true
	}
	if len(lids) == 0 {
		return false
	}
	if len(lids) != len(rids) {
		return true
	}
	for id := range lids {
		if _, ok := rids[id]; !ok {
			return true
		}
	}
	return false
}

func extractIdentifiers(title string) map[string]struct{} {
	normalized := strings.ToLower(normIdentifierText(title))
	result := make(map[string]struct{})
	for _, match := range cvePattern.FindAllString(normalized, -1) {
		match = strings.NewReplacer(" ", "-", "cve", "CVE").Replace(match)
		result[match] = struct{}{}
	}
	for _, match := range versionPattern.FindAllString(normalized, -1) {
		match = strings.TrimSpace(match)
		match = strings.TrimPrefix(match, "version")
		match = strings.TrimPrefix(match, "ver.")
		match = strings.TrimPrefix(match, "ver")
		match = strings.TrimPrefix(match, "バージョン")
		match = strings.TrimSpace(strings.TrimPrefix(match, "v"))
		// Avoid treating ISO-like publication dates as software versions.
		parts := strings.SplitN(match, ".", 2)
		if len(parts) > 0 && len(parts[0]) == 4 && parts[0] >= "1900" && parts[0] <= "2100" {
			continue
		}
		result["version:"+match] = struct{}{}
	}
	return result
}

func normIdentifierText(value string) string {
	return strings.NewReplacer("－", "-", "‐", "-", "–", "-", "—", "-", "．", ".").Replace(value)
}

// DeduplicateExact merges transitive identifier matches and selects one stable
// representative per component: official, feed weight, date, then URL.
func DeduplicateExact(articles []Article) []Article {
	if len(articles) < 2 {
		return append([]Article(nil), articles...)
	}
	parent := make([]int, len(articles))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(value int) int {
		if parent[value] != value {
			parent[value] = find(parent[value])
		}
		return parent[value]
	}
	union := func(left, right int) {
		lr, rr := find(left), find(right)
		if lr != rr {
			parent[rr] = lr
		}
	}
	for i := range articles {
		for j := i + 1; j < len(articles); j++ {
			if IsExactDuplicate(articles[i], articles[j]) {
				union(i, j)
			}
		}
	}
	representatives := make(map[int]int)
	firstIndex := make(map[int]int)
	for i := range articles {
		root := find(i)
		if _, ok := firstIndex[root]; !ok {
			firstIndex[root] = i
		}
		current, ok := representatives[root]
		if !ok || betterExactRepresentative(articles[i], articles[current]) {
			representatives[root] = i
		}
	}
	type component struct {
		first int
		rep   int
	}
	components := make([]component, 0, len(representatives))
	for root, rep := range representatives {
		components = append(components, component{first: firstIndex[root], rep: rep})
	}
	sort.Slice(components, func(i, j int) bool { return components[i].first < components[j].first })
	result := make([]Article, 0, len(components))
	for _, c := range components {
		candidate := articles[c.rep]
		candidate.EnsureNormalized()
		result = append(result, candidate)
	}
	return result
}

func betterExactRepresentative(candidate, current Article) bool {
	if candidate.Source.IsOfficial != current.Source.IsOfficial {
		return candidate.Source.IsOfficial
	}
	if candidate.Source.Weight != current.Source.Weight {
		return candidate.Source.Weight > current.Source.Weight
	}
	ct, cok := candidate.EffectiveTime()
	kt, kok := current.EffectiveTime()
	if cok != kok {
		return cok
	}
	if cok && !ct.Equal(kt) {
		return ct.After(kt)
	}
	cn := normalizedURLIdentity(candidate)
	kn := normalizedURLIdentity(current)
	if cn != kn {
		return cn < kn
	}
	return candidate.Source.Name < current.Source.Name
}

// DeduplicateNearRanked keeps the first (therefore highest-ranked) article in
// each approximate-title cluster.
func DeduplicateNearRanked(ranked []Article) []Article {
	result := make([]Article, 0, len(ranked))
	for _, candidate := range ranked {
		duplicate := false
		for _, selected := range result {
			if IsNearDuplicate(candidate, selected) {
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

// TimeDistance is exported for diagnostics and tests of near-duplicate policy.
func TimeDistance(left, right Article) time.Duration {
	lt, lok := left.EffectiveTime()
	rt, rok := right.EffectiveTime()
	if !lok || !rok {
		return time.Duration(math.MaxInt64)
	}
	delta := lt.Sub(rt)
	if delta < 0 {
		return -delta
	}
	return delta
}
