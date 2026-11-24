package discord

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

func TestTruncatePreservesGraphemeAndUTF8(t *testing.T) {
	input := strings.Repeat("👨‍👩‍👧‍👦日本語", 100)
	got := Truncate(input, 40)
	if !utf8.ValidString(got) || Units(got) > 40 {
		t.Fatalf("invalid truncation %q, units=%d", got, Units(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("missing ellipsis: %q", got)
	}
}

func TestPackEmbedsHonorsAggregateLimit(t *testing.T) {
	var embeds []*discordgo.MessageEmbed
	for i := 0; i < 5; i++ {
		embeds = append(embeds, &discordgo.MessageEmbed{Title: "title", Description: strings.Repeat("あ", 2000)})
	}
	batches, err := PackEmbeds(embeds)
	if err != nil {
		t.Fatalf("PackEmbeds() error = %v", err)
	}
	if len(batches) < 2 {
		t.Fatalf("expected multiple batches, got %d", len(batches))
	}
	for _, batch := range batches {
		total := 0
		for _, embed := range batch {
			total += EmbedCharacters(embed)
		}
		if total > MaxEmbedTotal || len(batch) > MaxEmbedsPerMessage {
			t.Fatalf("invalid batch: embeds=%d chars=%d", len(batch), total)
		}
	}
}

func TestBuildEmbedsRejectsUnsafeImage(t *testing.T) {
	embeds := BuildEmbeds(Briefing{Items: []Item{{Title: "x", URL: "https://example.com/a", ImageURL: "https://127.0.0.1/private"}}})
	if len(embeds) != 1 || embeds[0].Image != nil {
		t.Fatalf("unsafe image was accepted: %#v", embeds)
	}
}

func TestPreviewCompactsBothSummaryMethods(t *testing.T) {
	brief := Briefing{PriorityTitle: "上位トピック"}
	long := strings.Repeat("👨‍👩‍👧‍👦要約", 150)
	for i := range 5 {
		brief.Items = append(brief.Items, Item{
			Title: "記事", URL: "https://example.test/article", Summary: long, Background: long,
			Impact: long, Next: long, Generated: i%2 == 0,
		})
	}
	embeds, err := BuildPreviewEmbeds(brief)
	if err != nil || len(embeds) != 6 {
		t.Fatalf("preview embeds = %d, err = %v", len(embeds), err)
	}
	batches, err := PackEmbeds(embeds)
	if err != nil || len(batches) != 1 {
		t.Fatalf("preview batches = %d, err = %v", len(batches), err)
	}
	for i, item := range brief.Items {
		embed := embeds[i]
		if embed.URL != item.URL || embed.Footer.Text != item.SummaryLabel() ||
			!strings.HasSuffix(embed.Fields[0].Value, "…") || !utf8.ValidString(embed.Fields[0].Value) {
			t.Fatalf("article %d lost its summary or metadata", i)
		}
		if (len(embed.Fields) == 3) != item.Generated {
			t.Fatalf("article %d has unexpected summary fields", i)
		}
	}
}

func TestNonceIsStableAndShort(t *testing.T) {
	a := Nonce("1", "2026-07-11", "run", 0)
	b := Nonce("1", "2026-07-11", "run", 0)
	if a != b || len(a) > 25 {
		t.Fatalf("nonce is not stable/short: %q %q", a, b)
	}
}

func TestBriefCommandContextsOnlyForGlobalScope(t *testing.T) {
	if BriefCommand(true).Contexts == nil {
		t.Fatal("global command is missing guild-only contexts")
	}
	if BriefCommand(false).Contexts != nil {
		t.Fatal("guild command must omit contexts")
	}
}
