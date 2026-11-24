// Package httpclient provides the only outbound HTTP client used by the bot.
// It combines SSRF checks, redirect validation, bounded reads, media-type
// checks, character-set conversion, and polite per-host rate limiting.
package httpclient

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/security"
	"golang.org/x/net/html/charset"
)

const (
	defaultFeedMaxBytes    int64 = 2 << 20
	defaultArticleMaxBytes int64 = 2 << 20
	defaultRobotsMaxBytes  int64 = 512 << 10
	maxHostGates                 = 1024
)

var (
	// ErrTooLarge indicates that a response exceeded its uncompressed limit.
	ErrTooLarge = errors.New("HTTP response is too large")
	// ErrInvalidMIME indicates that a response media type did not match the
	// requested resource kind.
	ErrInvalidMIME = errors.New("invalid response media type")
	// ErrInvalidFeed indicates that an XML response was not RSS, Atom, or RDF.
	ErrInvalidFeed = errors.New("invalid feed document")
	// ErrUnexpectedStatus indicates a non-success status for a feed or article.
	ErrUnexpectedStatus = errors.New("unexpected HTTP status")
)

// Config controls the safe HTTP boundary. Zero fields receive production
// defaults through withDefaults.
type Config struct {
	UserAgent             string
	FeedMaxBytes          int64
	ArticleMaxBytes       int64
	RobotsMaxBytes        int64
	DialTimeout           time.Duration
	ResponseHeaderTimeout time.Duration
	RequestTimeout        time.Duration
	MaxRedirects          int
	GlobalConcurrency     int
	PerHostConcurrency    int
	MinHostDelay          time.Duration
	RobotsCacheTTL        time.Duration
	MaxCrawlDelay         time.Duration
}

// DefaultConfig returns the production HTTP limits.
func DefaultConfig() Config {
	return Config{
		UserAgent:             "DiscordComputerNewsBot/1.0 (+https://github.com/mosaseka/Discord_ComputerNewsBot)",
		FeedMaxBytes:          defaultFeedMaxBytes,
		ArticleMaxBytes:       defaultArticleMaxBytes,
		RobotsMaxBytes:        defaultRobotsMaxBytes,
		DialTimeout:           5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		RequestTimeout:        20 * time.Second,
		MaxRedirects:          5,
		GlobalConcurrency:     4,
		PerHostConcurrency:    1,
		MinHostDelay:          time.Second,
		RobotsCacheTTL:        time.Hour,
		MaxCrawlDelay:         5 * time.Second,
	}
}

// Response is a fully buffered and bounded HTTP response. Body is UTF-8 for
// successful feed, article, and robots responses.
type Response struct {
	URL         *url.URL
	StatusCode  int
	ContentType string
	Header      http.Header
	Body        []byte
}

// StatusError records a non-2xx feed or article response.
type StatusError struct {
	URL        string
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%v: %s returned HTTP %d", ErrUnexpectedStatus, e.URL, e.StatusCode)
}

func (e *StatusError) Unwrap() error { return ErrUnexpectedStatus }

// Client is safe for concurrent use.
type Client struct {
	config Config
	policy *security.Policy
	http   *http.Client
	rate   *rateTransport

	robotsMu    sync.Mutex
	robotsCache map[string]robotsCacheEntry
	robotsLoads map[string]*robotsLoad
}

// New constructs the shared HTTP client. A nil policy creates the default
// double-resolution security policy.
func New(config Config, policy *security.Policy) (*Client, error) {
	config = withDefaults(config)
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	if policy == nil {
		policy = security.NewPolicy(nil, &net.Dialer{Timeout: config.DialTimeout, KeepAlive: 30 * time.Second})
	}

	transport := &http.Transport{
		Proxy:                 nil, // Never inherit HTTP(S)_PROXY at this trust boundary.
		DialContext:           policy.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   config.DialTimeout,
		ResponseHeaderTimeout: config.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
	rate := newRateTransport(transport, config.GlobalConcurrency, config.PerHostConcurrency, config.MinHostDelay)
	c := &Client{
		config:      config,
		policy:      policy,
		rate:        rate,
		robotsCache: make(map[string]robotsCacheEntry),
		robotsLoads: make(map[string]*robotsLoad),
	}
	c.http = &http.Client{
		Transport: rate,
		Timeout:   config.RequestTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if resourceFromContext(req.Context()) == resourceGemini {
				return http.ErrUseLastResponse
			}
			if len(via) > config.MaxRedirects {
				return fmt.Errorf("stopped after %d redirects", config.MaxRedirects)
			}
			if err := policy.CheckURL(req.Context(), req.URL); err != nil {
				return fmt.Errorf("validate redirect target: %w", err)
			}
			if resourceFromContext(req.Context()) == resourceArticle {
				// Article redirects stay within the origin whose robots policy was
				// checked before fetching. Following a cross-origin redirect based on
				// incidental cache state would make behavior concurrency-dependent.
				if len(via) == 0 || !sameOrigin(via[0].URL, req.URL) {
					return &RobotsError{Decision: RobotsDecision{Status: RobotsUnavailable, Reason: "cross-origin article redirects are not followed"}}
				}
				decision, cached := c.checkRobotsCached(req.URL)
				if !cached {
					return &RobotsError{Decision: RobotsDecision{Status: RobotsUnavailable, Reason: "redirect target robots.txt is not cached"}}
				}
				if !decision.Allowed {
					return &RobotsError{Decision: decision}
				}
				if decision.CrawlDelay > 0 {
					c.rate.setHostDelay(req.URL.Hostname(), decision.CrawlDelay)
				}
			}
			return nil
		},
	}
	return c, nil
}

func sameOrigin(a, b *url.URL) bool {
	if a == nil || b == nil || !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	aHost := strings.TrimSuffix(strings.ToLower(a.Hostname()), ".")
	bHost := strings.TrimSuffix(strings.ToLower(b.Hostname()), ".")
	return aHost != "" && aHost == bHost && effectivePort(a) == effectivePort(b)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return ""
}

// CloseIdleConnections closes cached connections owned by the client.
func (c *Client) CloseIdleConnections() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

// ValidateURL applies the same scheme, userinfo, DNS, and public-address policy
// used immediately before outbound requests. It is useful for canonical,
// image, and follow-up URLs discovered while parsing an article.
func (c *Client) ValidateURL(ctx context.Context, rawURL string) (*url.URL, error) {
	if c == nil || c.policy == nil {
		return nil, errors.New("HTTP client is not initialized")
	}
	return c.policy.ValidateURL(ctx, rawURL)
}

// FetchFeed retrieves a bounded RSS, Atom, or RDF document and returns UTF-8.
func (c *Client) FetchFeed(ctx context.Context, rawURL string) (*Response, error) {
	return c.fetch(ctx, rawURL, resourceFeed)
}

// FetchArticle checks robots.txt and then retrieves a bounded HTML/XHTML page.
// Callers should treat a RobotsError as an instruction to retain feed content.
func (c *Client) FetchArticle(ctx context.Context, rawURL string) (*Response, error) {
	decision := c.CheckRobots(ctx, rawURL)
	if !decision.Allowed {
		return nil, &RobotsError{Decision: decision}
	}
	if decision.CrawlDelay > 0 {
		if u, err := url.Parse(rawURL); err == nil {
			c.rate.setHostDelay(u.Hostname(), decision.CrawlDelay)
		}
	}
	return c.fetch(ctx, rawURL, resourceArticle)
}

// FetchRobots retrieves a robots.txt URL. Unlike feed/article helpers it
// returns 4xx and 5xx statuses to allow RFC 9309 fallback handling.
func (c *Client) FetchRobots(ctx context.Context, rawURL string) (*Response, error) {
	return c.fetch(ctx, rawURL, resourceRobots)
}

type resourceKind uint8

type resourceContextKey struct{}

const (
	resourceFeed resourceKind = iota
	resourceArticle
	resourceRobots
	resourceGemini
)

func (c *Client) fetch(ctx context.Context, rawURL string, kind resourceKind) (*Response, error) {
	if c == nil || c.http == nil || c.policy == nil {
		return nil, errors.New("HTTP client is not initialized")
	}
	u, err := c.policy.ValidateURL(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, resourceContextKey{}, kind)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", c.config.UserAgent)
	req.Header.Set("Accept", acceptHeader(kind))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", u.Redacted(), err)
	}
	defer resp.Body.Close()

	limit := c.limit(kind)
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("%w: declared length %d exceeds %d", ErrTooLarge, resp.ContentLength, limit)
	}
	if kind != resourceRobots && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		return nil, &StatusError{URL: resp.Request.URL.Redacted(), StatusCode: resp.StatusCode}
	}

	body, err := readLimited(resp.Body, limit)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", resp.Request.URL.Redacted(), err)
	}
	mediaType, err := responseMediaType(resp.Header.Get("Content-Type"), body)
	if err != nil {
		return nil, err
	}
	result := &Response{
		URL:         cloneURL(resp.Request.URL),
		StatusCode:  resp.StatusCode,
		ContentType: mediaType,
		Header:      resp.Header.Clone(),
	}

	// Error responses to robots.txt are interpreted by CheckRobots. Their HTML
	// bodies are neither useful nor exposed to downstream parsing.
	if kind == resourceRobots && (resp.StatusCode < 200 || resp.StatusCode > 299) {
		return result, nil
	}
	if err := validateMIME(kind, mediaType); err != nil {
		return nil, err
	}
	body, err = convertToUTF8(body, resp.Header.Get("Content-Type"), limit)
	if err != nil {
		return nil, fmt.Errorf("convert response to UTF-8: %w", err)
	}
	if kind == resourceFeed {
		body = normalizeXMLDeclaration(body)
		if err := validateFeedRoot(body); err != nil {
			return nil, err
		}
	}
	result.Body = body
	return result, nil
}

func resourceFromContext(ctx context.Context) resourceKind {
	if kind, ok := ctx.Value(resourceContextKey{}).(resourceKind); ok {
		return kind
	}
	return resourceFeed
}

func (c *Client) limit(kind resourceKind) int64 {
	switch kind {
	case resourceFeed:
		return c.config.FeedMaxBytes
	case resourceArticle:
		return c.config.ArticleMaxBytes
	default:
		return c.config.RobotsMaxBytes
	}
}

func withDefaults(config Config) Config {
	d := DefaultConfig()
	if config.UserAgent == "" {
		config.UserAgent = d.UserAgent
	}
	if config.FeedMaxBytes == 0 {
		config.FeedMaxBytes = d.FeedMaxBytes
	}
	if config.ArticleMaxBytes == 0 {
		config.ArticleMaxBytes = d.ArticleMaxBytes
	}
	if config.RobotsMaxBytes == 0 {
		config.RobotsMaxBytes = d.RobotsMaxBytes
	}
	if config.DialTimeout == 0 {
		config.DialTimeout = d.DialTimeout
	}
	if config.ResponseHeaderTimeout == 0 {
		config.ResponseHeaderTimeout = d.ResponseHeaderTimeout
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = d.RequestTimeout
	}
	if config.MaxRedirects == 0 {
		config.MaxRedirects = d.MaxRedirects
	}
	if config.GlobalConcurrency == 0 {
		config.GlobalConcurrency = d.GlobalConcurrency
	}
	if config.PerHostConcurrency == 0 {
		config.PerHostConcurrency = d.PerHostConcurrency
	}
	if config.MinHostDelay == 0 {
		config.MinHostDelay = d.MinHostDelay
	}
	if config.RobotsCacheTTL == 0 {
		config.RobotsCacheTTL = d.RobotsCacheTTL
	}
	if config.MaxCrawlDelay == 0 {
		config.MaxCrawlDelay = d.MaxCrawlDelay
	}
	return config
}

func validateConfig(c Config) error {
	if strings.TrimSpace(c.UserAgent) == "" {
		return errors.New("HTTP user agent must not be empty")
	}
	if c.FeedMaxBytes <= 0 || c.ArticleMaxBytes <= 0 || c.RobotsMaxBytes <= 0 {
		return errors.New("HTTP byte limits must be positive")
	}
	if c.DialTimeout <= 0 || c.ResponseHeaderTimeout <= 0 || c.RequestTimeout <= 0 {
		return errors.New("HTTP timeouts must be positive")
	}
	if c.MaxRedirects <= 0 || c.GlobalConcurrency <= 0 || c.PerHostConcurrency <= 0 {
		return errors.New("HTTP redirect and concurrency limits must be positive")
	}
	if c.MinHostDelay < 0 || c.RobotsCacheTTL <= 0 || c.MaxCrawlDelay <= 0 {
		return errors.New("HTTP delay and robots cache settings are invalid")
	}
	return nil
}

func acceptHeader(kind resourceKind) string {
	switch kind {
	case resourceFeed:
		return "application/atom+xml, application/rss+xml, application/rdf+xml, application/xml, text/xml;q=0.9"
	case resourceArticle:
		return "text/html, application/xhtml+xml;q=0.9"
	default:
		return "text/plain, */*;q=0.1"
	}
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("%w: response exceeds %d bytes", ErrTooLarge, max)
	}
	return body, nil
}

func responseMediaType(header string, body []byte) (string, error) {
	if header == "" {
		header = http.DetectContentType(body)
	}
	mediaType, _, err := mime.ParseMediaType(header)
	if err != nil {
		return "", fmt.Errorf("%w: parse Content-Type %q: %v", ErrInvalidMIME, header, err)
	}
	return strings.ToLower(mediaType), nil
}

func validateMIME(kind resourceKind, mediaType string) error {
	valid := false
	switch kind {
	case resourceFeed:
		valid = mediaType == "application/atom+xml" || mediaType == "application/rss+xml" || mediaType == "application/rdf+xml" || mediaType == "application/xml" || mediaType == "text/xml"
	case resourceArticle:
		valid = mediaType == "text/html" || mediaType == "application/xhtml+xml"
	case resourceRobots:
		valid = mediaType == "text/plain" || mediaType == "application/octet-stream"
	}
	if !valid {
		return fmt.Errorf("%w: %q is not accepted for this resource", ErrInvalidMIME, mediaType)
	}
	return nil
}

func convertToUTF8(body []byte, contentType string, max int64) ([]byte, error) {
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return nil, err
	}
	return readLimited(r, max)
}

var xmlEncoding = regexp.MustCompile(`(?i)(<\?xml\s+[^>]*encoding\s*=\s*["'])[^"']+(["'])`)

func normalizeXMLDeclaration(body []byte) []byte {
	return xmlEncoding.ReplaceAll(body, []byte(`${1}UTF-8${2}`))
}

func validateFeedRoot(body []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	for {
		tok, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%w: XML has no root element", ErrInvalidFeed)
			}
			return fmt.Errorf("%w: parse XML root: %v", ErrInvalidFeed, err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch strings.ToLower(start.Name.Local) {
		case "rss", "feed", "rdf":
			return nil
		default:
			return fmt.Errorf("%w: unexpected root element %q", ErrInvalidFeed, start.Name.Local)
		}
	}
}

func cloneURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	copy := *u
	if u.User != nil {
		user := *u.User
		copy.User = &user
	}
	return &copy
}

type rateTransport struct {
	base      http.RoundTripper
	global    chan struct{}
	perHost   int
	baseDelay time.Duration
	mu        sync.Mutex
	hostGates map[string]*hostGate
}

type hostGate struct {
	sem   chan struct{}
	mu    sync.Mutex
	next  time.Time
	delay time.Duration
}

func newRateTransport(base http.RoundTripper, global, perHost int, delay time.Duration) *rateTransport {
	return &rateTransport{
		base:      base,
		global:    make(chan struct{}, global),
		perHost:   perHost,
		baseDelay: delay,
		hostGates: make(map[string]*hostGate),
	}
}

func (t *rateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	gate := t.gate(req.URL.Hostname())
	if err := acquire(req.Context(), gate.sem); err != nil {
		return nil, err
	}
	releaseHost := true
	hostAttempted := false
	defer func() {
		if releaseHost {
			if hostAttempted {
				gate.advance(t.baseDelay)
			}
			<-gate.sem
		}
	}()

	if err := acquire(req.Context(), t.global); err != nil {
		return nil, err
	}
	releaseGlobal := true
	defer func() {
		if releaseGlobal {
			<-t.global
		}
	}()
	// Reserve the host start time only after global capacity is available.
	// Otherwise global contention can make the reservation stale and allow two
	// same-host requests to start without the configured gap.
	if err := gate.wait(req.Context(), t.baseDelay); err != nil {
		return nil, err
	}
	hostAttempted = true

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	release := func() {
		// Waiting from response-body release is deliberately conservative. It
		// guarantees the origin observes at least the configured interval even
		// when the first request paid connection setup and the next reuses it.
		gate.advance(t.baseDelay)
		<-t.global
		<-gate.sem
	}
	resp.Body = &releaseBody{ReadCloser: resp.Body, release: release}
	releaseGlobal = false
	releaseHost = false
	return resp, nil
}

func (t *rateTransport) gate(host string) *hostGate {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	t.mu.Lock()
	defer t.mu.Unlock()
	if gate := t.hostGates[host]; gate != nil {
		return gate
	}
	// Keep state bounded for feeds that rotate through arbitrary hosts. Once
	// the cap is reached, new hosts share a conservative overflow gate.
	if len(t.hostGates) >= maxHostGates {
		host = "__overflow__"
		if gate := t.hostGates[host]; gate != nil {
			return gate
		}
	}
	gate := &hostGate{sem: make(chan struct{}, t.perHost), delay: t.baseDelay}
	t.hostGates[host] = gate
	return gate
}

func (t *rateTransport) setHostDelay(host string, delay time.Duration) {
	gate := t.gate(host)
	gate.mu.Lock()
	if delay > gate.delay {
		gate.delay = delay
		// The robots request was made using the base delay. Make the first
		// article request honor the newly discovered crawl-delay too.
		if next := time.Now().Add(delay); next.After(gate.next) {
			gate.next = next
		}
	}
	gate.mu.Unlock()
}

func (g *hostGate) wait(ctx context.Context, base time.Duration) error {
	g.mu.Lock()
	wait := time.Until(g.next)
	delay := g.delay
	if delay < base {
		delay = base
	}
	g.mu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	g.mu.Lock()
	g.next = time.Now().Add(delay)
	g.mu.Unlock()
	return nil
}

func (g *hostGate) advance(base time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delay := g.delay
	if delay < base {
		delay = base
	}
	if next := time.Now().Add(delay); next.After(g.next) {
		g.next = next
	}
}

func acquire(ctx context.Context, sem chan struct{}) error {
	select {
	case sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}
