package article

import (
	"testing"
	"time"
)

func TestDiceSimilarity(t *testing.T) {
	if got := DiceSimilarity("Go 1.25 正式版リリース", "Go 1.25 正式版をリリース"); got < 0.75 || got >= 1 {
		t.Fatalf("unexpected similarity: %v", got)
	}
	if got := DiceSimilarity("Go", "Python"); got != 0 {
		t.Fatalf("unrelated similarity: %v", got)
	}
}

func TestExactDuplicateIdentifiers(t *testing.T) {
	tests := []struct {
		name        string
		left, right Article
	}{
		{"guid", Article{GUID: "same"}, Article{GUID: "same"}},
		{"canonical", Article{CanonicalURL: "https://example.com/a/?utm_source=x"}, Article{CanonicalURL: "https://EXAMPLE.com/a"}},
		{"canonical-to-url", Article{URL: "https://mirror.example/a", CanonicalURL: "https://example.com/a"}, Article{URL: "https://example.com/a/"}},
		{"url", Article{URL: "https://example.com/a#x"}, Article{URL: "https://example.com/a/"}},
		{"title", Article{Title: "Go 1.25 リリース - Tech Blog"}, Article{Title: "Ｇｏ １．２５ リリース"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !IsExactDuplicate(tt.left, tt.right) {
				t.Fatal("expected duplicate")
			}
		})
	}
	if IsExactDuplicate(
		Article{GUID: "1", Source: Source{Name: "feed-a"}},
		Article{GUID: "1", Source: Source{Name: "feed-b"}},
	) {
		t.Fatal("feed-scoped GUIDs from different sources must not match")
	}
}

func TestNearDuplicateThresholdAndIdentifierGuard(t *testing.T) {
	now := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	left := Article{Title: "Go 1.24 正式版リリースのお知らせ", URL: "https://example.com/1", PublishedAt: &now}
	rightTime := now.Add(24 * time.Hour)
	right := Article{Title: "Go 1.25 正式版リリースのお知らせ", URL: "https://example.com/2", PublishedAt: &rightTime}
	if IsNearDuplicate(left, right) {
		t.Fatal("different versions must not be merged")
	}
	right.Title = "Go 1.24 正式版リリースのお知らせです"
	if !IsNearDuplicate(left, right) {
		t.Fatalf("same version and domain should be near duplicates; dice=%v", DiceSimilarity(left.Title, right.Title))
	}

	cveA := left
	cveA.Title = "CVE-2026-12345 の脆弱性情報"
	cveB := right
	cveB.Title = "CVE-2026-99999 の脆弱性情報"
	if IsNearDuplicate(cveA, cveB) {
		t.Fatal("different CVEs must not be merged")
	}
	generic := left
	generic.Title = "Widget 正式版リリースのお知らせ"
	versioned := right
	versioned.Title = "Widget v2.0 正式版リリースのお知らせ"
	if IsNearDuplicate(generic, versioned) {
		t.Fatal("a missing version and an explicit version must not be merged")
	}
}

func TestDeduplicateExactUsesStableBestRepresentativeAndTransitiveGroups(t *testing.T) {
	old := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	newer := old.Add(time.Hour)
	articles := []Article{
		{GUID: "g", Title: "alpha", URL: "https://z.example/a", CanonicalURL: "https://z.example/story", PublishedAt: &old, Source: Source{Name: "editorial", Weight: 2}},
		{GUID: "g", Title: "bridge", URL: "https://z.example/b", CanonicalURL: "https://z.example/story", PublishedAt: &newer, Source: Source{Name: "official", Weight: 1, IsOfficial: true}},
		{Title: "bridge", URL: "https://z.example/c", PublishedAt: &newer, Source: Source{Name: "other", Weight: 3}},
		{Title: "unrelated", URL: "https://z.example/d", PublishedAt: &newer},
	}
	got := DeduplicateExact(articles)
	if len(got) != 2 {
		t.Fatalf("DeduplicateExact len = %d, want 2", len(got))
	}
	if got[0].Source.Name != "official" {
		t.Fatalf("representative = %q, want official", got[0].Source.Name)
	}
	if got[1].Title != "unrelated" {
		t.Fatalf("second item = %q", got[1].Title)
	}
}

func TestDeduplicateNearRankedKeepsFirst(t *testing.T) {
	now := time.Now()
	input := []Article{
		{Title: "Python 3.15 正式版リリースのお知らせ", URL: "https://example.com/a", PublishedAt: &now},
		{Title: "Python 3.15 正式版リリースについて", URL: "https://example.com/b", PublishedAt: &now},
	}
	got := DeduplicateNearRanked(input)
	if len(got) != 1 || got[0].URL != input[0].URL {
		t.Fatalf("DeduplicateNearRanked() = %#v", got)
	}
}
