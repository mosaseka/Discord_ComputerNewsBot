package fixture

import (
	"context"
	"path/filepath"
	"testing"
)

func TestLoadOfflineFixtures(t *testing.T) {
	result, err := Load(context.Background(), filepath.Join("..", "..", "testdata", "feeds"))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if result.Now.IsZero() || len(result.Articles) < 6 {
		t.Fatalf("unexpected fixture result: now=%v articles=%d", result.Now, len(result.Articles))
	}
}
