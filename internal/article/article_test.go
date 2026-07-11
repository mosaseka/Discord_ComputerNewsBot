package article

import (
	"testing"
	"time"
)

func TestEffectiveTimePrefersPublished(t *testing.T) {
	published := time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC)
	updated := published.Add(time.Hour)
	a := Article{PublishedAt: &published, UpdatedAt: &updated}
	got, ok := a.EffectiveTime()
	if !ok || !got.Equal(published) || a.UsesUpdatedTime() {
		t.Fatalf("EffectiveTime() = %v, %v; UsesUpdatedTime=%v", got, ok, a.UsesUpdatedTime())
	}
}

func TestBestTextFallbackOrder(t *testing.T) {
	tests := []struct {
		name string
		in   Article
		want string
	}{
		{"body", Article{Body: "body", Content: "content", Description: "description"}, "body"},
		{"content", Article{Content: "content", Description: "description"}, "content"},
		{"description", Article{Description: "description"}, "description"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.BestText(); got != tt.want {
				t.Fatalf("BestText() = %q, want %q", got, tt.want)
			}
		})
	}
}
