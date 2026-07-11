package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNormalizeForComparison(t *testing.T) {
	t.Parallel()

	got := NormalizeForComparison("  ＡＢＣ　ｶﾞ\nFoo\tBAR  ")
	want := "abc ガ foo bar"
	if got != want {
		t.Fatalf("NormalizeForComparison() = %q, want %q", got, want)
	}
}

func TestGraphemeOperationsKeepClustersIntact(t *testing.T) {
	t.Parallel()

	input := "e\u0301👨‍👩‍👧‍👦🇯🇵日"
	if got, want := GraphemeCount(input), 4; got != want {
		t.Fatalf("GraphemeCount() = %d, want %d", got, want)
	}

	got := TruncateGraphemes(input, 3)
	want := "e\u0301👨‍👩‍👧‍👦🇯🇵"
	if got != want {
		t.Fatalf("TruncateGraphemes() = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) {
		t.Fatal("TruncateGraphemes() returned invalid UTF-8")
	}
}

func TestTruncateGraphemesWithSuffixIncludesSuffixInLimit(t *testing.T) {
	t.Parallel()

	got := TruncateGraphemesWithSuffix("あいうえお", 4, "…")
	if want := "あいう…"; got != want {
		t.Fatalf("TruncateGraphemesWithSuffix() = %q, want %q", got, want)
	}
	if count := GraphemeCount(got); count > 4 {
		t.Fatalf("result contains %d graphemes, want at most 4", count)
	}
}

func TestInvalidUTF8IsMadeSafe(t *testing.T) {
	t.Parallel()

	input := string([]byte{'a', 0xff, 'b'})
	got := TruncateGraphemes(input, 3)
	if !utf8.ValidString(got) {
		t.Fatalf("result is invalid UTF-8: %q", got)
	}
	if got != "a�b" {
		t.Fatalf("TruncateGraphemes() = %q, want replacement character", got)
	}
}

func TestStripHTML(t *testing.T) {
	t.Parallel()

	input := `<article><h1> 見出し &amp; 詳細 </h1><p>一つ目<br>二つ目</p>` +
		`<script>alert("secret")</script><style>.hidden{}</style><div> 終わり </div></article>`
	got := StripHTML(input)
	want := "見出し & 詳細\n一つ目\n二つ目\n終わり"
	if got != want {
		t.Fatalf("StripHTML() = %q, want %q", got, want)
	}
	if strings.Contains(got, "secret") || strings.Contains(got, "hidden") {
		t.Fatalf("StripHTML() retained ignored element content: %q", got)
	}
}
