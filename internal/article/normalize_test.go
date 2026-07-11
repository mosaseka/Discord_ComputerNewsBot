package article

import "testing"

func TestNormalizeURL(t *testing.T) {
	got, err := NormalizeURL(" HTTPS://Example.COM:443/news/item/?z=2&utm_source=x&REF=top&a=3&a=1#section ")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://example.com/news/item?a=1&a=3&z=2"
	if got != want {
		t.Fatalf("NormalizeURL() = %q, want %q", got, want)
	}

	root, err := NormalizeURL("https://EXAMPLE.com/")
	if err != nil || root != "https://example.com" {
		t.Fatalf("root = %q, %v", root, err)
	}
	escapedSlash, err := NormalizeURL("https://example.com/a%2F")
	if err != nil || escapedSlash != "https://example.com/a%2F" {
		t.Fatalf("escaped slash = %q, %v", escapedSlash, err)
	}
}

func TestNormalizeURLRejectsUnsupportedAndCredentials(t *testing.T) {
	for _, raw := range []string{"/relative", "file:///tmp/a", "https://user:pass@example.com/a", "https:///missing"} {
		if _, err := NormalizeURL(raw); err == nil {
			t.Errorf("NormalizeURL(%q) unexpectedly succeeded", raw)
		}
	}
}

func TestNormalizeTitle(t *testing.T) {
	left := NormalizeTitle("Ｇｏ １．２５：正式版リリース！ - GitHub Blog 日本語")
	right := NormalizeTitle("go 1.25 正式版リリース")
	if left != right {
		t.Fatalf("normalized titles differ: %q != %q", left, right)
	}
}
