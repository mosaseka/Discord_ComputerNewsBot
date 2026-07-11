package feed

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/article"
)

func TestParseRSS2DescriptionAndOrdering(t *testing.T) {
	input := `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Test</title><link>https://example.com</link>
<item><guid>old</guid><title>古い記事</title><link>https://example.com/old?utm_source=rss</link><pubDate>Thu, 09 Jul 2026 00:00:00 GMT</pubDate><description><![CDATA[<p>説明の<strong>本文</strong>です。</p><script>bad()</script>]]></description></item>
<item><guid>new</guid><title>新しい記事</title><link>https://example.com/new</link><pubDate>Fri, 10 Jul 2026 00:00:00 GMT</pubDate><description>new body</description></item>
</channel></rss>`
	got, err := NewParser(50).Parse(context.Background(), strings.NewReader(input), article.Source{Name: "RSS", Weight: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].GUID != "new" || got[1].GUID != "old" {
		t.Fatalf("unexpected ordering: %#v", got)
	}
	if got[1].Body != "説明の本文です。" {
		t.Fatalf("description body = %q", got[1].Body)
	}
	if got[1].NormalizedURL != "https://example.com/old" {
		t.Fatalf("normalized URL = %q", got[1].NormalizedURL)
	}
}

func TestParseAtom(t *testing.T) {
	input := `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom">
<title>Atom test</title><link href="https://example.com/"/><updated>2026-07-10T00:00:00Z</updated><id>feed</id>
<entry><title>Go release</title><link href="https://example.com/go"/><id>tag:example.com,go</id><published>2026-07-10T01:00:00Z</published><updated>2026-07-10T02:00:00Z</updated><author><name>Taro</name></author><content type="html">&lt;p&gt;Go の本文です。&lt;/p&gt;</content></entry>
</feed>`
	got, err := NewParser(50).Parse(context.Background(), strings.NewReader(input), article.Source{Name: "Atom"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].GUID != "tag:example.com,go" || got[0].Body != "Go の本文です。" {
		t.Fatalf("article = %#v", got[0])
	}
	if len(got[0].Authors) != 1 || got[0].Authors[0] != "Taro" {
		t.Fatalf("authors = %#v", got[0].Authors)
	}
}

func TestParseRSS1RDF(t *testing.T) {
	input := `<?xml version="1.0"?><rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel rdf:about="https://example.com/"><title>RDF</title><link>https://example.com/</link><description>test</description><items><rdf:Seq><rdf:li resource="https://example.com/rdf"/></rdf:Seq></items></channel>
<item rdf:about="https://example.com/rdf"><title>脆弱性情報</title><link>https://example.com/rdf</link><description>詳細です。</description><dc:date>2026-07-10T03:00:00Z</dc:date></item>
</rdf:RDF>`
	got, err := NewParser(50).Parse(context.Background(), strings.NewReader(input), article.Source{Name: "RDF"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "脆弱性情報" {
		t.Fatalf("got = %#v", got)
	}
	date, ok := got[0].EffectiveTime()
	if !ok || !date.Equal(time.Date(2026, 7, 10, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("date = %v, %v", date, ok)
	}
}

func TestParseLimitAndStableMissingDate(t *testing.T) {
	input := `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title><link>https://example.com</link>
<item><title>first</title><link>https://example.com/1</link></item><item><title>second</title><link>https://example.com/2</link></item>
</channel></rss>`
	got, err := NewParser(1).Parse(context.Background(), strings.NewReader(input), article.Source{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "first" {
		t.Fatalf("got = %#v", got)
	}
}

func TestParseNoItemsAndCanceledContext(t *testing.T) {
	empty := `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title><link>https://example.com</link></channel></rss>`
	_, err := NewParser(0).Parse(context.Background(), strings.NewReader(empty), article.Source{Name: "empty"})
	if !errors.Is(err, ErrNoItems) {
		t.Fatalf("error = %v, want ErrNoItems", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = NewParser(0).Parse(ctx, strings.NewReader(empty), article.Source{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
}

func TestParseSkipsMalformedItemButKeepsValidItems(t *testing.T) {
	input := `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title><link>https://example.com</link>
<item><title>bad</title><link>file:///etc/passwd</link></item>
<item><title>valid</title><link>https://example.com/valid</link><pubDate>Fri, 10 Jul 2026 00:00:00 GMT</pubDate></item>
</channel></rss>`
	got, err := NewParser(50).Parse(context.Background(), strings.NewReader(input), article.Source{Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Title != "valid" {
		t.Fatalf("got = %#v", got)
	}
}

func TestParseResolvesRelativeItemLinksAgainstFeedURL(t *testing.T) {
	input := `<?xml version="1.0"?><rss version="2.0"><channel><title>x</title><link>https://example.com</link>
<item><title>relative</title><link>../articles/one</link><pubDate>Fri, 10 Jul 2026 00:00:00 GMT</pubDate></item>
</channel></rss>`
	got, err := NewParser(50).Parse(context.Background(), strings.NewReader(input), article.Source{Name: "test", URL: "https://example.com/news/feed.xml"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].URL != "https://example.com/articles/one" {
		t.Fatalf("relative URL was not resolved: %#v", got)
	}
}
