// Package article contains the normalized, transport-independent article model.
package article

import (
	"time"
)

// Source describes the feed that supplied an article. It deliberately mirrors
// the public feed configuration without depending on the config package.
type Source struct {
	Name       string
	URL        string
	Weight     float64
	Language   string
	Category   string
	IsOfficial bool
}

// Link is a link discovered in a feed entry or article body. Text is the
// human-readable anchor text when it was available.
type Link struct {
	Text string
	URL  string
}

// Article is the shared value passed through collection, enrichment, ranking,
// summarization, persistence, and publishing. Content and Description may
// contain markup from a feed. Body is extracted plain text from either the
// feed or an article page. HasFetchedBody distinguishes article-page text.
type Article struct {
	GUID            string
	Title           string
	URL             string
	CanonicalURL    string
	NormalizedURL   string
	NormalizedTitle string
	Source          Source
	Description     string
	Content         string
	Body            string
	HasFetchedBody  bool
	PublishedAt     *time.Time
	UpdatedAt       *time.Time
	Authors         []string
	SiteName        string
	ImageURL        string
	Links           []Link
}

// EffectiveTime returns the publication time, or the update time when a feed
// did not provide a publication time. The bool is false when neither exists.
func (a Article) EffectiveTime() (time.Time, bool) {
	if a.PublishedAt != nil {
		return *a.PublishedAt, true
	}
	if a.UpdatedAt != nil {
		return *a.UpdatedAt, true
	}
	return time.Time{}, false
}

// UsesUpdatedTime reports whether EffectiveTime falls back to UpdatedAt.
func (a Article) UsesUpdatedTime() bool {
	return a.PublishedAt == nil && a.UpdatedAt != nil
}

// BestText returns the richest available article text. It does not strip feed
// markup; the summarizer owns that deterministic cleanup step.
func (a Article) BestText() string {
	if a.Body != "" {
		return a.Body
	}
	if a.Content != "" {
		return a.Content
	}
	return a.Description
}

// EnsureNormalized fills derived comparison fields when they are missing.
// A malformed URL does not prevent title-only processing.
func (a *Article) EnsureNormalized() {
	if a.NormalizedTitle == "" {
		a.NormalizedTitle = NormalizeTitle(a.Title)
	}
	if a.NormalizedURL == "" {
		candidate := a.CanonicalURL
		if candidate == "" {
			candidate = a.URL
		}
		if normalized, err := NormalizeURL(candidate); err == nil {
			a.NormalizedURL = normalized
		}
	}
}
