package httpclient

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrRobotsDenied = errors.New("article fetch denied by robots policy")

const maxRobotsOrigins = 512

// RobotsStatus explains why an article page may or may not be fetched.
type RobotsStatus string

const (
	RobotsAllowed           RobotsStatus = "allowed"
	RobotsDisallowed        RobotsStatus = "disallowed"
	RobotsNotFound          RobotsStatus = "not_found"
	RobotsUnavailable       RobotsStatus = "unavailable"
	RobotsInvalid           RobotsStatus = "invalid"
	RobotsInvalidURL        RobotsStatus = "invalid_url"
	RobotsCrawlDelayTooLong RobotsStatus = "crawl_delay_too_long"
)

// RobotsDecision is deliberately value-only so it can be logged or persisted
// without exposing response bodies.
type RobotsDecision struct {
	Allowed    bool
	Status     RobotsStatus
	CrawlDelay time.Duration
	Reason     string
}

// RobotsError reports a safe feed-content fallback from FetchArticle.
type RobotsError struct {
	Decision RobotsDecision
}

func (e *RobotsError) Error() string {
	if e == nil {
		return ErrRobotsDenied.Error()
	}
	if e.Decision.Reason == "" {
		return fmt.Sprintf("%v: %s", ErrRobotsDenied, e.Decision.Status)
	}
	return fmt.Sprintf("%v: %s: %s", ErrRobotsDenied, e.Decision.Status, e.Decision.Reason)
}

func (e *RobotsError) Unwrap() error { return ErrRobotsDenied }

type robotsCacheEntry struct {
	expires time.Time
	record  robotsRecord
}

type robotsLoad struct {
	done   chan struct{}
	record robotsRecord
}

type robotsRecord struct {
	status RobotsStatus
	reason string
	groups []robotsGroup
}

type robotsGroup struct {
	agents []string
	rules  []robotsRule
	delay  time.Duration
}

type robotsRule struct {
	allow   bool
	pattern string
}

// CheckRobots evaluates the origin's cached robots.txt for articleURL. A 4xx
// response allows fetching; transport, 5xx, MIME, and parse failures deny it.
func (c *Client) CheckRobots(ctx context.Context, articleURL string) RobotsDecision {
	if c == nil || c.policy == nil {
		return RobotsDecision{Status: RobotsUnavailable, Reason: "HTTP client is not initialized"}
	}
	u, err := c.policy.ValidateURL(ctx, articleURL)
	if err != nil {
		return RobotsDecision{Status: RobotsInvalidURL, Reason: err.Error()}
	}
	origin := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
	record := c.robotsRecord(ctx, origin)

	switch record.status {
	case RobotsNotFound:
		return RobotsDecision{Allowed: true, Status: RobotsNotFound, Reason: record.reason}
	case RobotsUnavailable, RobotsInvalid:
		return RobotsDecision{Status: record.status, Reason: record.reason}
	}

	allowed, delay := evaluateRobots(record.groups, c.config.UserAgent, escapedPathAndQuery(u))
	if !allowed {
		return RobotsDecision{Status: RobotsDisallowed, CrawlDelay: delay, Reason: "robots.txt disallows this path"}
	}
	if delay > c.config.MaxCrawlDelay {
		return RobotsDecision{
			Status:     RobotsCrawlDelayTooLong,
			CrawlDelay: delay,
			Reason:     fmt.Sprintf("crawl-delay %s exceeds the configured maximum %s", delay, c.config.MaxCrawlDelay),
		}
	}
	return RobotsDecision{Allowed: true, Status: RobotsAllowed, CrawlDelay: delay}
}

// checkRobotsCached evaluates redirect targets without issuing nested HTTP
// requests from http.Client.CheckRedirect (the previous response body and
// per-host semaphore are still held at that point). A missing cache entry is
// handled conservatively by rejecting the redirect.
func (c *Client) checkRobotsCached(u *url.URL) (RobotsDecision, bool) {
	if c == nil || u == nil {
		return RobotsDecision{Status: RobotsUnavailable, Reason: "HTTP client is not initialized"}, false
	}
	origin := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
	c.robotsMu.Lock()
	entry, ok := c.robotsCache[origin]
	if ok && time.Now().After(entry.expires) {
		delete(c.robotsCache, origin)
		ok = false
	}
	c.robotsMu.Unlock()
	if !ok {
		return RobotsDecision{Status: RobotsUnavailable, Reason: "redirect target robots.txt is not cached"}, false
	}
	switch entry.record.status {
	case RobotsNotFound:
		return RobotsDecision{Allowed: true, Status: RobotsNotFound, Reason: entry.record.reason}, true
	case RobotsUnavailable, RobotsInvalid:
		return RobotsDecision{Status: entry.record.status, Reason: entry.record.reason}, true
	}
	allowed, delay := evaluateRobots(entry.record.groups, c.config.UserAgent, escapedPathAndQuery(u))
	if !allowed {
		return RobotsDecision{Status: RobotsDisallowed, CrawlDelay: delay, Reason: "robots.txt disallows redirect path"}, true
	}
	if delay > c.config.MaxCrawlDelay {
		return RobotsDecision{Status: RobotsCrawlDelayTooLong, CrawlDelay: delay, Reason: "crawl-delay exceeds configured maximum"}, true
	}
	return RobotsDecision{Allowed: true, Status: RobotsAllowed, CrawlDelay: delay}, true
}

// robotsRecord coalesces concurrent cache misses for the same origin. This is
// important because article enrichment is concurrent while robots.txt should
// be fetched at most once per origin and cache interval.
func (c *Client) robotsRecord(ctx context.Context, origin string) robotsRecord {
	c.robotsMu.Lock()
	c.pruneRobotsCacheLocked(time.Now())
	if entry, ok := c.robotsCache[origin]; ok && time.Now().Before(entry.expires) {
		c.robotsMu.Unlock()
		return entry.record
	}
	delete(c.robotsCache, origin)
	if inFlight, ok := c.robotsLoads[origin]; ok {
		done := inFlight.done
		c.robotsMu.Unlock()
		select {
		case <-done:
			return inFlight.record
		case <-ctx.Done():
			return robotsRecord{status: RobotsUnavailable, reason: ctx.Err().Error()}
		}
	}
	load := &robotsLoad{done: make(chan struct{})}
	c.robotsLoads[origin] = load
	c.robotsMu.Unlock()

	record := c.loadRobots(ctx, origin)
	c.robotsMu.Lock()
	load.record = record
	c.pruneRobotsCacheLocked(time.Now())
	if len(c.robotsCache) >= maxRobotsOrigins {
		var oldestKey string
		var oldestExpiry time.Time
		for key, entry := range c.robotsCache {
			if oldestKey == "" || entry.expires.Before(oldestExpiry) {
				oldestKey, oldestExpiry = key, entry.expires
			}
		}
		delete(c.robotsCache, oldestKey)
	}
	c.robotsCache[origin] = robotsCacheEntry{expires: time.Now().Add(c.config.RobotsCacheTTL), record: record}
	delete(c.robotsLoads, origin)
	close(load.done)
	c.robotsMu.Unlock()
	return record
}

func (c *Client) pruneRobotsCacheLocked(now time.Time) {
	for origin, entry := range c.robotsCache {
		if !now.Before(entry.expires) {
			delete(c.robotsCache, origin)
		}
	}
}

func (c *Client) cachedRobots(origin string) (robotsRecord, bool) {
	c.robotsMu.Lock()
	defer c.robotsMu.Unlock()
	entry, ok := c.robotsCache[origin]
	if !ok || time.Now().After(entry.expires) {
		delete(c.robotsCache, origin)
		return robotsRecord{}, false
	}
	return entry.record, true
}

func (c *Client) storeRobots(origin string, record robotsRecord) {
	c.robotsMu.Lock()
	c.robotsCache[origin] = robotsCacheEntry{expires: time.Now().Add(c.config.RobotsCacheTTL), record: record}
	c.robotsMu.Unlock()
}

func (c *Client) loadRobots(ctx context.Context, origin string) robotsRecord {
	response, err := c.FetchRobots(ctx, origin+"/robots.txt")
	if err != nil {
		return robotsRecord{status: RobotsUnavailable, reason: err.Error()}
	}
	if response.StatusCode >= 400 && response.StatusCode <= 499 {
		return robotsRecord{status: RobotsNotFound, reason: fmt.Sprintf("robots.txt returned HTTP %d", response.StatusCode)}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return robotsRecord{status: RobotsUnavailable, reason: fmt.Sprintf("robots.txt returned HTTP %d", response.StatusCode)}
	}
	groups, err := parseRobots(response.Body)
	if err != nil {
		return robotsRecord{status: RobotsInvalid, reason: err.Error()}
	}
	return robotsRecord{status: RobotsAllowed, groups: groups}
}

func parseRobots(body []byte) ([]robotsGroup, error) {
	if !utf8.Valid(body) || strings.IndexByte(string(body), 0) >= 0 {
		return nil, errors.New("robots.txt is not valid UTF-8 text")
	}
	text := strings.TrimPrefix(string(body), "\uFEFF")
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 4096), int(defaultRobotsMaxBytes))
	groups := make([]robotsGroup, 0, 4)
	current := robotsGroup{}
	hasDirectives := false
	flush := func() {
		if len(current.agents) > 0 {
			groups = append(groups, current)
		}
		current = robotsGroup{}
		hasDirectives = false
	}
	for scanner.Scan() {
		line := scanner.Text()
		if index := strings.IndexByte(line, '#'); index >= 0 {
			line = line[:index]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			return nil, fmt.Errorf("invalid robots.txt directive %q", line)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		switch key {
		case "user-agent":
			if value == "" {
				return nil, errors.New("robots.txt contains an empty user-agent")
			}
			if hasDirectives {
				flush()
			}
			current.agents = append(current.agents, strings.ToLower(value))
		case "allow", "disallow":
			if len(current.agents) == 0 {
				continue
			}
			hasDirectives = true
			if value != "" {
				current.rules = append(current.rules, robotsRule{allow: key == "allow", pattern: value})
			}
		case "crawl-delay":
			if len(current.agents) == 0 {
				continue
			}
			hasDirectives = true
			seconds, parseErr := strconv.ParseFloat(value, 64)
			maxSeconds := float64(math.MaxInt64) / float64(time.Second)
			if parseErr != nil || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds > maxSeconds {
				return nil, fmt.Errorf("invalid crawl-delay %q", value)
			}
			current.delay = time.Duration(seconds * float64(time.Second))
		default:
			// Sitemap and extension directives do not affect access.
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan robots.txt: %w", err)
	}
	flush()
	return groups, nil
}

func evaluateRobots(groups []robotsGroup, userAgent, requestPath string) (bool, time.Duration) {
	product := strings.ToLower(strings.Fields(userAgent)[0])
	if index := strings.IndexByte(product, '/'); index >= 0 {
		product = product[:index]
	}
	bestAgentLength := -1
	selected := make([]robotsGroup, 0, 2)
	for _, group := range groups {
		groupScore := -1
		for _, agent := range group.agents {
			agent = strings.TrimSpace(strings.ToLower(agent))
			score := -1
			switch {
			case agent == "*":
				score = 0
			case agent != "" && strings.HasPrefix(product, agent):
				score = len(agent)
			}
			if score > groupScore {
				groupScore = score
			}
		}
		if groupScore < 0 {
			continue
		}
		if groupScore > bestAgentLength {
			bestAgentLength = groupScore
			selected = selected[:0]
		}
		if groupScore == bestAgentLength {
			selected = append(selected, group)
		}
	}
	if len(selected) == 0 {
		return true, 0
	}

	allowed := true
	bestRuleLength := -1
	var delay time.Duration
	for _, group := range selected {
		if group.delay > delay {
			delay = group.delay
		}
		for _, rule := range group.rules {
			if !robotsPatternMatches(rule.pattern, requestPath) {
				continue
			}
			normalizedPattern := strings.TrimSuffix(normalizeRobotsOctets(rule.pattern), "$")
			score := len(strings.ReplaceAll(normalizedPattern, "*", ""))
			if score > bestRuleLength || (score == bestRuleLength && rule.allow) {
				bestRuleLength = score
				allowed = rule.allow
			}
		}
	}
	return allowed, delay
}

func robotsPatternMatches(pattern, requestPath string) bool {
	pattern = normalizeRobotsOctets(pattern)
	requestPath = normalizeRobotsOctets(requestPath)
	endAnchored := strings.HasSuffix(pattern, "$")
	if endAnchored {
		pattern = strings.TrimSuffix(pattern, "$")
	}
	parts := strings.Split(pattern, "*")
	var expression strings.Builder
	expression.WriteByte('^')
	for index, part := range parts {
		if index > 0 {
			expression.WriteString(".*")
		}
		expression.WriteString(regexp.QuoteMeta(part))
	}
	if endAnchored {
		expression.WriteByte('$')
	}
	matched, err := regexp.MatchString(expression.String(), requestPath)
	return err == nil && matched
}

func normalizeRobotsOctets(value string) string {
	const hexDigits = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); {
		if value[i] == '%' && i+2 < len(value) {
			high, highOK := fromHex(value[i+1])
			low, lowOK := fromHex(value[i+2])
			if highOK && lowOK {
				decoded := high<<4 | low
				if isURIUnreserved(decoded) {
					out.WriteByte(decoded)
				} else {
					out.WriteByte('%')
					out.WriteByte(hexDigits[decoded>>4])
					out.WriteByte(hexDigits[decoded&0x0f])
				}
				i += 3
				continue
			}
		}
		if value[i] < utf8.RuneSelf {
			if value[i] == '%' {
				out.WriteString("%25")
			} else {
				out.WriteByte(value[i])
			}
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(value[i:])
		for _, current := range []byte(value[i : i+size]) {
			out.WriteByte('%')
			out.WriteByte(hexDigits[current>>4])
			out.WriteByte(hexDigits[current&0x0f])
		}
		i += size
	}
	return out.String()
}

func fromHex(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func isURIUnreserved(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' ||
		value == '-' || value == '.' || value == '_' || value == '~'
}

func escapedPathAndQuery(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// robotsURL is kept small and deterministic for tests and callers which need
// to display the exact policy resource without performing a request.
func robotsURL(u *url.URL) string {
	copy := *u
	copy.Path = path.Clean("/robots.txt")
	copy.RawPath = ""
	copy.RawQuery = ""
	copy.Fragment = ""
	return copy.String()
}
