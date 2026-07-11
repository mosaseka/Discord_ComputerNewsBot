// Package feed parses already-fetched RSS, Atom, and RDF streams. Network I/O
// is intentionally outside this package so every caller uses the shared safe
// HTTP client (or an offline fixture reader).
package feed

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/article"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/textutil"
)

const DefaultMaxItems = 50

var ErrNoItems = errors.New("feed contains no items")

// Parser applies deterministic conversion and per-feed limiting.
type Parser struct {
	maxItems int
}

// NewParser returns a parser limited to maxItems newest entries. Values <= 0
// use DefaultMaxItems.
func NewParser(maxItems int) *Parser {
	if maxItems <= 0 {
		maxItems = DefaultMaxItems
	}
	return &Parser{maxItems: maxItems}
}

// Parse reads a feed from r without performing network I/O. It supports the
// RSS, Atom, and RSS 1.0/RDF formats handled by gofeed.
func (p *Parser) Parse(ctx context.Context, r io.Reader, source article.Source) ([]article.Article, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, errors.New("parse feed: nil reader")
	}
	parsed, err := gofeed.NewParser().Parse(r)
	if err != nil {
		return nil, fmt.Errorf("parse feed %q: %w", source.Name, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if parsed == nil || len(parsed.Items) == 0 {
		return nil, fmt.Errorf("parse feed %q: %w", source.Name, ErrNoItems)
	}

	articles := make([]article.Article, 0, len(parsed.Items))
	for _, item := range parsed.Items {
		if item == nil {
			continue
		}
		entry := article.Article{
			GUID:        strings.TrimSpace(item.GUID),
			Title:       strings.TrimSpace(item.Title),
			URL:         resolveURL(source.URL, item.Link),
			Source:      source,
			Description: item.Description,
			Content:     item.Content,
			PublishedAt: cloneTime(item.PublishedParsed),
			UpdatedAt:   cloneTime(item.UpdatedParsed),
			Authors:     itemAuthors(item),
			Links:       itemLinks(item, source.URL),
			SiteName:    strings.TrimSpace(parsed.Title),
		}
		if item.Image != nil {
			entry.ImageURL = resolveURL(source.URL, item.Image.URL)
		}
		if entry.ImageURL == "" {
			for _, enclosure := range item.Enclosures {
				if enclosure != nil && strings.HasPrefix(strings.ToLower(enclosure.Type), "image/") {
					entry.ImageURL = resolveURL(source.URL, enclosure.URL)
					break
				}
			}
		}
		if entry.URL == "" && len(item.Links) > 0 {
			entry.URL = resolveURL(source.URL, item.Links[0])
		}
		entry.Body = textutil.StripHTML(entry.BestText())
		entry.EnsureNormalized()
		// Treat each item as untrusted independently. A malformed item should
		// not abort persistence and ranking for the rest of a valid feed.
		if entry.Title == "" || entry.NormalizedTitle == "" || entry.NormalizedURL == "" {
			continue
		}
		articles = append(articles, entry)
	}
	if len(articles) == 0 {
		return nil, fmt.Errorf("parse feed %q: %w", source.Name, ErrNoItems)
	}

	// gofeed preserves source order, which is the final tie breaker here.
	sort.SliceStable(articles, func(i, j int) bool {
		left, lok := articles[i].EffectiveTime()
		right, rok := articles[j].EffectiveTime()
		if lok != rok {
			return lok
		}
		return lok && !left.Equal(right) && left.After(right)
	})
	if len(articles) > p.maxItems {
		articles = articles[:p.maxItems]
	}
	return articles, nil
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func itemAuthors(item *gofeed.Item) []string {
	result := make([]string, 0, len(item.Authors)+1)
	seen := make(map[string]struct{})
	add := func(person *gofeed.Person) {
		if person == nil {
			return
		}
		name := strings.TrimSpace(person.Name)
		if name == "" {
			name = strings.TrimSpace(person.Email)
		}
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	add(item.Author)
	for _, author := range item.Authors {
		add(author)
	}
	return result
}

func itemLinks(item *gofeed.Item, baseURL string) []article.Link {
	result := make([]article.Link, 0, len(item.Links)+1)
	seen := make(map[string]struct{})
	add := func(raw string) {
		raw = resolveURL(baseURL, raw)
		if raw == "" {
			return
		}
		identity := raw
		if normalized, err := article.NormalizeURL(raw); err == nil {
			identity = normalized
		}
		if _, ok := seen[identity]; ok {
			return
		}
		seen[identity] = struct{}{}
		result = append(result, article.Link{URL: raw})
	}
	add(item.Link)
	for _, link := range item.Links {
		add(link)
	}
	return result
}

func resolveURL(baseRaw, referenceRaw string) string {
	referenceRaw = strings.TrimSpace(referenceRaw)
	if referenceRaw == "" {
		return ""
	}
	reference, err := url.Parse(referenceRaw)
	if err != nil {
		return referenceRaw
	}
	if reference.IsAbs() {
		return reference.String()
	}
	base, err := url.Parse(strings.TrimSpace(baseRaw))
	if err != nil || !base.IsAbs() {
		return referenceRaw
	}
	return base.ResolveReference(reference).String()
}
