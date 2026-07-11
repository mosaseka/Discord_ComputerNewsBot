package config

import (
	"strings"
	"testing"
)

func TestDecodeFeedsStrictAndValidates(t *testing.T) {
	valid := `feeds:
  - name: Example
    url: https://example.com/feed.xml
    enabled: true
    weight: 1.2
    language: ja
    category: general
    is_official: false
`
	feeds, err := DecodeFeeds(strings.NewReader(valid))
	if err != nil {
		t.Fatalf("DecodeFeeds() error = %v", err)
	}
	if len(feeds) != 1 || feeds[0].Name != "Example" {
		t.Fatalf("unexpected feeds: %#v", feeds)
	}

	unknown := strings.Replace(valid, "    enabled: true\n", "    enabled: true\n    typo: true\n", 1)
	if _, err := DecodeFeeds(strings.NewReader(unknown)); err == nil {
		t.Fatal("DecodeFeeds() accepted an unknown key")
	}
}

func TestDecodeFeedsRejectsDuplicateAndBadWeight(t *testing.T) {
	input := `feeds:
  - name: Same
    url: https://example.com/a
    enabled: true
    weight: 0
    language: ja
    category: general
    is_official: false
`
	if _, err := DecodeFeeds(strings.NewReader(input)); err == nil {
		t.Fatal("DecodeFeeds() accepted zero weight")
	}
}

func TestDecodeFeedsRejectsEquivalentURLsAndExtraDocument(t *testing.T) {
	duplicate := `feeds:
  - {name: A, url: "https://EXAMPLE.com:443/feed/", enabled: true, weight: 1, language: ja, category: general, is_official: false}
  - {name: B, url: "https://example.com/feed", enabled: true, weight: 1, language: ja, category: general, is_official: false}
`
	if _, err := DecodeFeeds(strings.NewReader(duplicate)); err == nil {
		t.Fatal("DecodeFeeds() accepted equivalent duplicate URLs")
	}
	extra := `feeds:
  - {name: A, url: "https://example.com/feed", enabled: true, weight: 1, language: ja, category: general, is_official: false}
---
unexpected: true
`
	if _, err := DecodeFeeds(strings.NewReader(extra)); err == nil {
		t.Fatal("DecodeFeeds() accepted a second YAML document")
	}
}

func TestLoadFixtureForcesDryRun(t *testing.T) {
	t.Setenv("DATABASE_PATH", "test.db")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("DISCORD_CHANNEL_ID", "")
	cfg, err := Load(Mode{Once: true, Fixture: "fixtures"})
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.Mode.DryRun {
		t.Fatal("fixture mode did not force dry-run")
	}
}

func TestLoadRejectsPostingWithoutDiscord(t *testing.T) {
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("DISCORD_CHANNEL_ID", "")
	if _, err := Load(Mode{Once: true}); err == nil {
		t.Fatal("Load() accepted posting mode without Discord credentials")
	}
}

func TestLoadRejectsDryRunWithoutOnce(t *testing.T) {
	if _, err := Load(Mode{DryRun: true}); err == nil {
		t.Fatal("Load() accepted --dry-run without --once")
	}
}
