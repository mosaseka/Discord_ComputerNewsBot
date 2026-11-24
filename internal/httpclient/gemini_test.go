package httpclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/security"
)

func TestGeminiRequestUsesFixedPOSTAndRefusesRedirect(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusTemporaryRedirect, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			client := newGeminiTestClient(t, time.Second, func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodPost || req.URL.String() != geminiEndpoint || req.GetBody != nil ||
					req.Header.Get("X-Goog-Api-Key") != "test" || req.Header.Get("Content-Type") != "application/json" {
					t.Error("unexpected Gemini request")
				}
				payload, err := io.ReadAll(req.Body)
				req.Body.Close()
				if err != nil || string(payload) != "{}" {
					t.Error("unexpected Gemini request body")
				}
				return &http.Response{
					StatusCode: status,
					Header:     http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Location": {"https://other.test/"}},
					Body:       io.NopCloser(strings.NewReader("{}")),
				}, nil
			})
			body, err := client.GenerateGeminiContent(context.Background(), "test", []byte("{}"))
			if calls != 1 {
				t.Fatalf("Gemini requests = %d, want one", calls)
			}
			if status == http.StatusOK {
				if err != nil || string(body) != "{}" {
					t.Fatalf("Gemini success: %v", err)
				}
				return
			}
			var statusErr *StatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != status {
				t.Fatalf("Gemini error = %v, want HTTP %d", err, status)
			}
		})
	}
}

func TestGeminiRequestBounds(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		want        error
	}{
		{"MIME", "text/html", "{}", ErrInvalidMIME},
		{"oversize", "application/json", strings.Repeat(" ", geminiMaxResponseBytes+1), ErrTooLarge},
		{"timeout", "", "", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := newGeminiTestClient(t, 250*time.Millisecond, func(req *http.Request) (*http.Response, error) {
				req.Body.Close()
				if tc.contentType == "" {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {tc.contentType}},
					Body:       io.NopCloser(strings.NewReader(tc.body)),
				}, nil
			})
			if _, err := client.GenerateGeminiContent(context.Background(), "test", []byte("{}")); !errors.Is(err, tc.want) {
				t.Fatalf("Gemini error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGeminiSkipsHTTPWithoutKeyOrWithOversizeRequest(t *testing.T) {
	client := newGeminiTestClient(t, time.Second, func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected HTTP call")
		return nil, nil
	})
	if _, err := client.GenerateGeminiContent(context.Background(), "", []byte("{}")); err == nil {
		t.Fatal("empty key accepted")
	}
	if _, err := client.GenerateGeminiContent(context.Background(), "test", make([]byte, geminiMaxRequestBytes+1)); err == nil {
		t.Fatal("oversize request accepted")
	}
}

func newGeminiTestClient(t *testing.T, timeout time.Duration, transport roundTripFunc) *Client {
	t.Helper()
	cfg := DefaultConfig()
	cfg.MinHostDelay = time.Millisecond
	cfg.RequestTimeout = timeout
	policy := security.NewPolicy(testResolver(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "generativelanguage.googleapis.com" {
			t.Errorf("unexpected host: %s", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	}), testDialer(func(context.Context, string, string) (net.Conn, error) {
		t.Error("unexpected real dial")
		return nil, errors.New("unexpected dial")
	}))
	client, err := New(cfg, policy)
	if err != nil {
		t.Fatal(err)
	}
	client.rate.base = transport
	return client
}
