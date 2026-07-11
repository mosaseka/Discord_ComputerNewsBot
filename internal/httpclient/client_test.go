package httpclient

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/security"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
)

type testResolver func(context.Context, string) ([]net.IPAddr, error)

func (f testResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

type testDialer func(context.Context, string, string) (net.Conn, error)

func (f testDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func newLocalClient(t *testing.T, handler http.Handler, mutate func(*Config)) (*Client, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resolver := testResolver(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	})
	baseDialer := &net.Dialer{Timeout: time.Second}
	dialer := testDialer(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return baseDialer.DialContext(ctx, network, serverURL.Host)
	})
	config := DefaultConfig()
	config.MinHostDelay = time.Millisecond
	config.RequestTimeout = time.Second
	config.ResponseHeaderTimeout = time.Second
	config.DialTimeout = time.Second
	if mutate != nil {
		mutate(&config)
	}
	client, err := New(config, security.NewPolicy(resolver, dialer))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(client.CloseIdleConnections)
	return client, "http://safe.test" + portSuffix(serverURL)
}

func portSuffix(u *url.URL) string {
	if u.Port() == "" {
		return ""
	}
	return ":" + u.Port()
}

func TestFetchFeedValidatesMIMEAndRoot(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
		fmt.Fprint(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>News</title></channel></rss>`)
	})
	client, baseURL := newLocalClient(t, handler, nil)
	response, err := client.FetchFeed(context.Background(), baseURL+"/feed")
	if err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if response.ContentType != "application/rss+xml" || !strings.Contains(string(response.Body), "<rss") {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestFetchFeedRejectsHTMLAndNonFeedXML(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		contentType string
		body        string
		want        error
	}{
		{"html", "text/html", "<html><body>not a feed</body></html>", ErrInvalidMIME},
		{"wrong-root", "application/xml", "<document><item/></document>", ErrInvalidFeed},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, baseURL := newLocalClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				fmt.Fprint(w, test.body)
			}), nil)
			_, err := client.FetchFeed(context.Background(), baseURL+"/feed")
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestFetchArticleConvertsCharsetToUTF8(t *testing.T) {
	t.Parallel()
	encoded, _, err := transform.String(japanese.ShiftJIS.NewEncoder(), `<html><head><meta charset="shift_jis"></head><body><article>日本語の記事です。</article></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=shift_jis")
		fmt.Fprint(w, encoded)
	})
	client, baseURL := newLocalClient(t, handler, nil)
	response, err := client.FetchArticle(context.Background(), baseURL+"/article")
	if err != nil {
		t.Fatalf("FetchArticle: %v", err)
	}
	if !strings.Contains(string(response.Body), "日本語の記事です") {
		t.Fatalf("body was not converted to UTF-8: %q", response.Body)
	}
}

func TestFetchFeedConvertsXMLCharsetAndDeclaration(t *testing.T) {
	t.Parallel()
	encoded, _, err := transform.String(japanese.ShiftJIS.NewEncoder(), `<?xml version="1.0" encoding="Shift_JIS"?><rss><channel><title>技術ニュース</title></channel></rss>`)
	if err != nil {
		t.Fatal(err)
	}
	client, baseURL := newLocalClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml; charset=shift_jis")
		fmt.Fprint(w, encoded)
	}), nil)
	response, err := client.FetchFeed(context.Background(), baseURL+"/feed")
	if err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if !strings.Contains(string(response.Body), "技術ニュース") || !strings.Contains(string(response.Body), `encoding="UTF-8"`) {
		t.Fatalf("converted feed = %q", response.Body)
	}
}

func TestLimitAppliesAfterGzipDecompression(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Header().Set("Content-Encoding", "gzip")
		writer := gzip.NewWriter(w)
		fmt.Fprintf(writer, "<rss><channel><description>%s</description></channel></rss>", strings.Repeat("x", 512))
		writer.Close()
	})
	client, baseURL := newLocalClient(t, handler, func(config *Config) {
		config.FeedMaxBytes = 128
	})
	_, err := client.FetchFeed(context.Background(), baseURL+"/feed")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error = %v, want ErrTooLarge", err)
	}
}

func TestRedirectTargetIsRevalidated(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://127.0.0.1/private")
		w.WriteHeader(http.StatusFound)
	})
	client, baseURL := newLocalClient(t, handler, nil)
	_, err := client.FetchFeed(context.Background(), baseURL+"/start")
	if !errors.Is(err, security.ErrUnsafeURL) {
		t.Fatalf("error = %v, want security.ErrUnsafeURL", err)
	}
}

func TestRedirectLimitAllowsExactlyConfiguredHops(t *testing.T) {
	t.Parallel()
	client, baseURL := newLocalClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		step, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/"))
		last, _ := strconv.Atoi(r.URL.Query().Get("last"))
		if step < last {
			http.Redirect(w, r, fmt.Sprintf("/%d?last=%d", step+1, last), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title><link>https://example.com</link><item><title>x</title><link>https://example.com/x</link></item></channel></rss>`)
	}), func(config *Config) {
		config.MaxRedirects = 5
		config.MinHostDelay = time.Millisecond
	})
	if _, err := client.FetchFeed(context.Background(), baseURL+"/0?last=5"); err != nil {
		t.Fatalf("five redirects should succeed: %v", err)
	}
	if _, err := client.FetchFeed(context.Background(), baseURL+"/0?last=6"); err == nil {
		t.Fatal("six redirects should fail")
	}
}

func TestDialTimeLookupRejectsRebinding(t *testing.T) {
	t.Parallel()
	var lookups atomic.Int32
	resolver := testResolver(func(context.Context, string) ([]net.IPAddr, error) {
		if lookups.Add(1) == 1 {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	})
	var dialCalls atomic.Int32
	policy := security.NewPolicy(resolver, testDialer(func(context.Context, string, string) (net.Conn, error) {
		dialCalls.Add(1)
		return nil, errors.New("must not dial")
	}))
	config := DefaultConfig()
	config.MinHostDelay = time.Millisecond
	client, err := New(config, policy)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.FetchFeed(context.Background(), "http://rebind.test/feed")
	if !errors.Is(err, security.ErrUnsafeURL) {
		t.Fatalf("error = %v, want security.ErrUnsafeURL", err)
	}
	if dialCalls.Load() != 0 {
		t.Fatalf("dialer called %d times", dialCalls.Load())
	}
}

func TestRequestTimeoutIncludesResponseHeaders(t *testing.T) {
	t.Parallel()
	client, baseURL := newLocalClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, "<rss/>")
	}), func(config *Config) {
		config.RequestTimeout = 25 * time.Millisecond
		config.ResponseHeaderTimeout = 20 * time.Millisecond
	})
	started := time.Now()
	_, err := client.FetchFeed(context.Background(), baseURL+"/slow")
	if err == nil {
		t.Fatal("FetchFeed succeeded, want timeout")
	}
	if elapsed := time.Since(started); elapsed > 90*time.Millisecond {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestPerHostConcurrencyAndDelay(t *testing.T) {
	t.Parallel()
	var active, maxActive atomic.Int32
	var mu sync.Mutex
	var starts []time.Time
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nowActive := active.Add(1)
		defer active.Add(-1)
		for {
			old := maxActive.Load()
			if nowActive <= old || maxActive.CompareAndSwap(old, nowActive) {
				break
			}
		}
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "application/rss+xml")
		fmt.Fprint(w, "<rss/>")
	})
	client, baseURL := newLocalClient(t, handler, func(config *Config) {
		config.MinHostDelay = 20 * time.Millisecond
	})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := client.FetchFeed(context.Background(), fmt.Sprintf("%s/feed/%d", baseURL, index))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("FetchFeed: %v", err)
		}
	}
	if maxActive.Load() != 1 {
		t.Fatalf("maximum concurrent requests = %d, want 1", maxActive.Load())
	}
	mu.Lock()
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	gap := starts[1].Sub(starts[0])
	mu.Unlock()
	if gap < 20*time.Millisecond {
		t.Fatalf("start gap = %s, want at least 20ms", gap)
	}
}

func TestHostStartDelaySurvivesGlobalContention(t *testing.T) {
	blockStarted := make(chan struct{})
	releaseBlock := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var starts []time.Time
	base := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Hostname() == "block.test" {
			once.Do(func() { close(blockStarted) })
			<-releaseBlock
		} else {
			mu.Lock()
			starts = append(starts, time.Now())
			mu.Unlock()
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: request}, nil
	})
	transport := newRateTransport(base, 1, 1, 50*time.Millisecond)
	client := &http.Client{Transport: transport}
	var wg sync.WaitGroup
	get := func(raw string) {
		defer wg.Done()
		response, err := client.Get(raw)
		if err != nil {
			t.Errorf("GET %s: %v", raw, err)
			return
		}
		response.Body.Close()
	}
	wg.Add(1)
	go get("http://block.test/")
	<-blockStarted
	wg.Add(2)
	go get("http://same.test/a")
	go get("http://same.test/b")
	time.Sleep(70 * time.Millisecond)
	close(releaseBlock)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 2 || starts[1].Sub(starts[0]) < 40*time.Millisecond {
		t.Fatalf("same-host starts = %v; configured delay was not preserved", starts)
	}
}

func TestPerHostAndRobotsStateAreBounded(t *testing.T) {
	transport := newRateTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	}), 1, 1, time.Millisecond)
	for i := 0; i < maxHostGates+200; i++ {
		transport.gate(fmt.Sprintf("host-%d.example", i))
	}
	if got := len(transport.hostGates); got > maxHostGates+1 {
		t.Fatalf("host gate state grew to %d", got)
	}
	c := &Client{robotsCache: make(map[string]robotsCacheEntry)}
	now := time.Now()
	for i := 0; i < maxRobotsOrigins+100; i++ {
		c.robotsCache[fmt.Sprintf("https://host-%d.example", i)] = robotsCacheEntry{expires: now.Add(-time.Minute)}
	}
	c.robotsMu.Lock()
	c.pruneRobotsCacheLocked(now)
	c.robotsMu.Unlock()
	if got := len(c.robotsCache); got != 0 {
		t.Fatalf("expired robots cache retained %d entries", got)
	}
}

func TestRobotsDecisionsAndCache(t *testing.T) {
	t.Parallel()
	var robotsRequests atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			robotsRequests.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "User-agent: DiscordComputerNewsBot\nDisallow: /private\nAllow: /private/release\nCrawl-delay: 0.001\n")
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body>article</body></html>")
	})
	client, baseURL := newLocalClient(t, handler, nil)
	allowed := client.CheckRobots(context.Background(), baseURL+"/private/release")
	if !allowed.Allowed || allowed.Status != RobotsAllowed {
		t.Fatalf("allowed decision = %#v", allowed)
	}
	denied := client.CheckRobots(context.Background(), baseURL+"/private/secret")
	if denied.Allowed || denied.Status != RobotsDisallowed {
		t.Fatalf("denied decision = %#v", denied)
	}
	if robotsRequests.Load() != 1 {
		t.Fatalf("robots.txt requests = %d, want 1", robotsRequests.Load())
	}
}

func TestConcurrentRobotsCacheMissIsCoalesced(t *testing.T) {
	t.Parallel()
	var robotsRequests atomic.Int32
	client, baseURL := newLocalClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" {
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
		robotsRequests.Add(1)
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "User-agent: *\nAllow: /\n")
	}), func(config *Config) { config.MinHostDelay = time.Millisecond })

	const callers = 8
	var wg sync.WaitGroup
	decisions := make(chan RobotsDecision, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decisions <- client.CheckRobots(context.Background(), baseURL+"/article")
		}()
	}
	wg.Wait()
	close(decisions)
	for decision := range decisions {
		if !decision.Allowed {
			t.Fatalf("decision = %#v", decision)
		}
	}
	if got := robotsRequests.Load(); got != 1 {
		t.Fatalf("robots.txt requests = %d, want 1", got)
	}
}

func TestRobotsSafeFallbackStatuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     int
		body       string
		allowed    bool
		wantStatus RobotsStatus
	}{
		{"missing", http.StatusNotFound, "", true, RobotsNotFound},
		{"server-error", http.StatusServiceUnavailable, "", false, RobotsUnavailable},
		{"too-slow", http.StatusOK, "User-agent: *\nCrawl-delay: 6\n", false, RobotsCrawlDelayTooLong},
		{"overflow-delay", http.StatusOK, "User-agent: *\nCrawl-delay: 1e100\n", false, RobotsInvalid},
		{"invalid", http.StatusOK, "this is not a directive\n", false, RobotsInvalid},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client, baseURL := newLocalClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/robots.txt" {
					t.Fatalf("unexpected request path %q", r.URL.Path)
				}
				if test.status == http.StatusOK {
					w.Header().Set("Content-Type", "text/plain")
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.body)
			}), nil)
			decision := client.CheckRobots(context.Background(), baseURL+"/article")
			if decision.Allowed != test.allowed || decision.Status != test.wantStatus {
				t.Fatalf("decision = %#v, want allowed=%v status=%s", decision, test.allowed, test.wantStatus)
			}
		})
	}
}

func TestRobotsPatternNormalizesUnicodeAndPercentEncoding(t *testing.T) {
	if !robotsPatternMatches("/非公開/*", "/%E9%9D%9E%E5%85%AC%E9%96%8B/release") {
		t.Fatal("raw Unicode robots pattern did not match an escaped request path")
	}
	if !robotsPatternMatches("/docs/%7Euser", "/docs/~user") {
		t.Fatal("percent-encoded unreserved octet was not normalized")
	}
}

func TestRobotsRuleSpecificityUsesNormalizedOctets(t *testing.T) {
	groups := []robotsGroup{{agents: []string{"*"}, rules: []robotsRule{
		{allow: false, pattern: "/非公開"},
		{allow: true, pattern: "/%E9%9D%9E%E5%85%AC%E9%96%8B/release"},
	}}}
	allowed, _ := evaluateRobots(groups, "DiscordComputerNewsBot", "/%E9%9D%9E%E5%85%AC%E9%96%8B/release")
	if !allowed {
		t.Fatal("longer normalized Allow rule did not win")
	}
}

func TestRobotsParserAcceptsUTF8BOMWithoutDroppingRules(t *testing.T) {
	groups, err := parseRobots([]byte("\xEF\xBB\xBFUser-agent: *\nDisallow: /private\n"))
	if err != nil {
		t.Fatal(err)
	}
	allowed, _ := evaluateRobots(groups, "DiscordComputerNewsBot/1.0", "/private/data")
	if allowed {
		t.Fatal("BOM caused Disallow rule to be ignored")
	}
}

func TestFetchArticleReturnsTypedRobotsError(t *testing.T) {
	t.Parallel()
	client, baseURL := newLocalClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
	}), nil)
	_, err := client.FetchArticle(context.Background(), baseURL+"/article")
	if !errors.Is(err, ErrRobotsDenied) {
		t.Fatalf("error = %v, want ErrRobotsDenied", err)
	}
	var robotsErr *RobotsError
	if !errors.As(err, &robotsErr) || robotsErr.Decision.Status != RobotsDisallowed {
		t.Fatalf("typed robots error = %#v", robotsErr)
	}
}

func TestArticleRedirectRechecksCachedRobotsRules(t *testing.T) {
	t.Parallel()
	var blockedRequests atomic.Int32
	client, baseURL := newLocalClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "User-agent: *\nAllow: /allowed\nDisallow: /blocked\n")
		case "/allowed":
			http.Redirect(w, r, "/blocked", http.StatusFound)
		case "/blocked":
			blockedRequests.Add(1)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>must not be fetched</body></html>")
		}
	}), func(config *Config) { config.MinHostDelay = time.Millisecond })
	_, err := client.FetchArticle(context.Background(), baseURL+"/allowed")
	if !errors.Is(err, ErrRobotsDenied) {
		t.Fatalf("redirect error = %v, want ErrRobotsDenied", err)
	}
	if blockedRequests.Load() != 0 {
		t.Fatal("robots-disallowed redirect target was fetched")
	}
}

func TestArticleCrossOriginRedirectIsRejectedEvenWhenRobotsAreCached(t *testing.T) {
	t.Parallel()
	var targetURL string
	var targetRequests atomic.Int32
	client, baseURL := newLocalClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "User-agent: *\nAllow: /\n")
		case "/start":
			http.Redirect(w, r, targetURL, http.StatusFound)
		case "/target":
			targetRequests.Add(1)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html><body>must not be fetched</body></html>")
		}
	}), func(config *Config) { config.MinHostDelay = time.Millisecond })
	targetURL = strings.Replace(baseURL, "safe.test", "other.safe.test", 1) + "/target"
	if decision := client.CheckRobots(context.Background(), targetURL); !decision.Allowed {
		t.Fatalf("target robots preload failed: %#v", decision)
	}
	_, err := client.FetchArticle(context.Background(), baseURL+"/start")
	if !errors.Is(err, ErrRobotsDenied) {
		t.Fatalf("cross-origin redirect error = %v, want ErrRobotsDenied", err)
	}
	if targetRequests.Load() != 0 {
		t.Fatal("cross-origin article redirect target was fetched")
	}
}
