package htmlparser

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

func TestParseExtractsReadableContentAndMetadata(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("../../testdata/articles/article.html")
	if err != nil {
		t.Fatal(err)
	}
	pageURL, _ := url.Parse("https://news.example/posts/original")
	result, err := Parse(context.Background(), bytes.NewReader(body), pageURL, int64(len(body)+1))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(result.Title, "Go 1.25") {
		t.Errorf("title = %q", result.Title)
	}
	if !strings.Contains(result.TextContent, "標準ツールチェーン") {
		t.Errorf("text did not contain article body: %q", result.TextContent)
	}
	for _, noise := range []string{"サイト共通ヘッダー", "Cookie設定", "alert(\"本文に残してはいけません\")"} {
		if strings.Contains(result.TextContent, noise) || strings.Contains(result.HTMLContent, noise) {
			t.Errorf("cleaned article retained noise %q", noise)
		}
	}
	if result.CanonicalURL != "https://news.example/canonical/go-1.25" {
		t.Errorf("canonical = %q", result.CanonicalURL)
	}
	if result.ImageURL != "https://news.example/images/go125.png" {
		t.Errorf("image = %q", result.ImageURL)
	}
	if result.Byline != "山田 太郎" {
		t.Errorf("byline = %q", result.Byline)
	}
	if result.SiteName != "Example Tech" {
		t.Errorf("site = %q", result.SiteName)
	}
	wantPublished := time.Date(2026, 7, 10, 9, 30, 0, 0, time.FixedZone("JST", 9*60*60))
	if result.PublishedAt == nil || !result.PublishedAt.Equal(wantPublished) {
		t.Errorf("published = %v", result.PublishedAt)
	}
	if len(result.Links) != 1 || result.Links[0].URL != "https://go.dev/doc/go1.25" {
		t.Errorf("safe links = %#v", result.Links)
	}
	if strings.Contains(result.HTMLContent, "javascript:") || strings.Contains(result.HTMLContent, "127.0.0.1") {
		t.Errorf("unsafe link retained in HTML: %s", result.HTMLContent)
	}
}

func TestExtractMetadataSupportsDirectAndGraphJSONLD(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		json string
		want string
	}{
		{"direct", `{"@type":"TechArticle","headline":"Direct headline","image":"/direct.png"}`, "Direct headline"},
		{"graph", `{"@graph":[{"@type":"WebSite","name":"site"},{"@type":"NewsArticle","headline":"Graph headline","image":{"url":"/graph.png"}}]}`, "Graph headline"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document, err := html.Parse(strings.NewReader(`<html><head><script type="application/ld+json">` + test.json + `</script></head><body></body></html>`))
			if err != nil {
				t.Fatal(err)
			}
			base, _ := url.Parse("https://example.test/article")
			metadata := extractMetadata(document, base)
			if metadata.title != test.want {
				t.Errorf("title = %q, want %q", metadata.title, test.want)
			}
			if metadata.imageURL == "" || !strings.HasPrefix(metadata.imageURL, "https://example.test/") {
				t.Errorf("image URL = %q", metadata.imageURL)
			}
		})
	}
}

func TestParseHonorsLimitContextAndUTF8(t *testing.T) {
	t.Parallel()
	base, _ := url.Parse("https://example.test/article")
	if _, err := Parse(context.Background(), strings.NewReader(strings.Repeat("x", 20)), base, 10); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("size error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Parse(canceled, strings.NewReader("<html></html>"), base, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("context error = %v", err)
	}
	if _, err := ParseBytes(context.Background(), []byte{0xff, 0xfe}, base); !errors.Is(err, ErrInvalidHTML) {
		t.Fatalf("UTF-8 error = %v", err)
	}
}

func TestParseRejectsUnsafeBaseURL(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"file:///tmp/article", "http://127.0.0.1/article", "https://user:pass@example.test/article"} {
		base, _ := url.Parse(raw)
		if _, err := ParseBytes(context.Background(), []byte("<html></html>"), base); !errors.Is(err, ErrInvalidPageURL) {
			t.Errorf("ParseBytes(%q) error = %v", raw, err)
		}
	}
}

func TestParseRemovesClassAndRoleBasedNoise(t *testing.T) {
	base, _ := url.Parse("https://example.test/article")
	input := `<html><head><title>Technical article</title></head><body><article>
<p>Goのコンパイラ実装について十分な長さを持つ技術本文です。アーキテクチャとAPIの互換性を詳しく説明します。</p>
<div class="advertisement">広告だけの固有テキスト</div>
<section id="related-articles">関連記事だけの固有テキスト</section>
<div class="social-share">共有ボタンだけの固有テキスト</div>
<div role="comment">コメントだけの固有テキスト</div>
<div aria-label="cookie consent">Cookie通知だけの固有テキスト</div>
<p>開発者は更新前に公式ドキュメントを確認する必要があります。</p>
</article></body></html>`
	result, err := Parse(context.Background(), strings.NewReader(input), base, int64(len(input)+1))
	if err != nil {
		t.Fatal(err)
	}
	for _, noise := range []string{"広告だけ", "関連記事だけ", "共有ボタンだけ", "コメントだけ", "Cookie通知だけ"} {
		if strings.Contains(result.TextContent, noise) || strings.Contains(result.HTMLContent, noise) {
			t.Fatalf("noise %q remained in output", noise)
		}
	}
}
