package summarizer

import (
	"strings"
	"testing"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/textutil"
)

func TestSplitAndFilterSentences(t *testing.T) {
	t.Parallel()

	first := "これは十分な長さを持っている最初の文です。"
	second := "改行で区切られる文章にも十分な文字数があります！?"
	short := "短い。"
	tooLong := strings.Repeat("長", MaxSentenceGraphemes+1) + "。"
	input := first + short + "\r\n" + second + "\n" + tooLong

	all := SplitSentences(input)
	if len(all) != 4 {
		t.Fatalf("SplitSentences() returned %d sentences, want 4: %#v", len(all), all)
	}
	if all[1] != short || !strings.HasSuffix(all[2], "！?") {
		t.Fatalf("SplitSentences() did not preserve delimiters: %#v", all)
	}

	candidates := CandidateSentences(input)
	if len(candidates) != 2 || candidates[0] != first || candidates[1] != second {
		t.Fatalf("CandidateSentences() = %#v, want first and second", candidates)
	}
}

func TestCandidateSentencesAcceptsHTML(t *testing.T) {
	t.Parallel()

	html := `<article><p>Goの新しいAPIについて詳しい仕様が公開されました。</p>` +
		`<script>この秘密の文章は絶対に候補へ入れないでください。</script></article>`
	got := CandidateSentences(html)
	if len(got) != 1 || strings.Contains(strings.Join(got, ""), "秘密") {
		t.Fatalf("CandidateSentences(HTML) = %#v", got)
	}
}

func TestSummarizeScoresAndRestoresSourceOrder(t *testing.T) {
	t.Parallel()

	first := "この発表では利用できる機能の概要が簡潔に紹介されています。"
	second := "Go 1.25のAPIは新しいコンパイラとランタイムの仕様を採用しました。"
	third := "開発者は互換性を確認してSDKを更新する必要があり、移行手順も公開されました。"
	result := Summarize(Input{
		Title: "Go 1.25 APIの新仕様と移行手順",
		Body:  first + second + third,
	})

	if strings.Contains(result.KeyPoints, first) {
		t.Fatalf("generic sentence unexpectedly selected: %q", result.KeyPoints)
	}
	secondPos := strings.Index(result.KeyPoints, second)
	thirdPos := strings.Index(result.KeyPoints, third)
	if secondPos < 0 || thirdPos < 0 || secondPos >= thirdPos {
		t.Fatalf("key points were not restored to source order: %q", result.KeyPoints)
	}
	if result.TechnicalBackground != second {
		t.Fatalf("TechnicalBackground = %q, want %q", result.TechnicalBackground, second)
	}
	if result.DeveloperImpact != third {
		t.Fatalf("DeveloperImpact = %q, want %q", result.DeveloperImpact, third)
	}
}

func TestSummarizeRejectsNearDuplicateAndAdvertisement(t *testing.T) {
	t.Parallel()

	first := "Python 3.15の新しいAPIで非同期処理の性能が大幅に改善されました。"
	duplicate := "Python 3.15の新しいAPIにより非同期処理の性能が大幅に改善されました。"
	advertisement := "【PR】今すぐお申し込みいただくと無料登録できるキャンペーン実施中です。"
	different := "利用者は既存コードの互換性と移行ガイドを事前に確認する必要があります。"
	result := Summarize(Input{
		Title: "Python 3.15 APIの性能改善",
		Body:  first + duplicate + advertisement + different,
	})

	if strings.Contains(result.KeyPoints, "キャンペーン") {
		t.Fatalf("advertisement was selected: %q", result.KeyPoints)
	}
	selectedDuplicates := 0
	if strings.Contains(result.KeyPoints, first) {
		selectedDuplicates++
	}
	if strings.Contains(result.KeyPoints, duplicate) {
		selectedDuplicates++
	}
	if selectedDuplicates != 1 {
		t.Fatalf("selected %d near-duplicate sentences, want 1: %q", selectedDuplicates, result.KeyPoints)
	}
	if !strings.Contains(result.KeyPoints, different) {
		t.Fatalf("distinct sentence was not selected: %q", result.KeyPoints)
	}
}

func TestSummarizeExactFallbacks(t *testing.T) {
	t.Parallel()

	result := Summarize(Input{Body: "短い。https://example.com/"})
	if result.TechnicalBackground != "記事本文から詳細な技術的背景を抽出できませんでした" {
		t.Fatalf("TechnicalBackground = %q", result.TechnicalBackground)
	}
	if result.DeveloperImpact != "開発者への具体的な影響は元記事を確認してください" {
		t.Fatalf("DeveloperImpact = %q", result.DeveloperImpact)
	}
	if result.NextCheck != "リリースノートまたは公式ドキュメントの確認を推奨します" {
		t.Fatalf("NextCheck = %q", result.NextCheck)
	}
	if result.NextCheckURL != "" {
		t.Fatalf("NextCheckURL = %q, want empty", result.NextCheckURL)
	}
}

func TestSelectNextCheckPrefersDocumentation(t *testing.T) {
	t.Parallel()

	message, target := SelectNextCheck([]Link{
		{Text: "Release notes", URL: "https://example.com/releases/v2.0"},
		{Text: "公式ドキュメント", URL: "https://docs.example.com/docs/v2/api"},
		{Text: "Issue #42", URL: "https://github.com/example/project/issues/42"},
	})
	if target != "https://docs.example.com/docs/v2/api" {
		t.Fatalf("target = %q, want documentation URL", target)
	}
	if !strings.Contains(message, "公式ドキュメント") {
		t.Fatalf("message = %q, want selected anchor", message)
	}
}

func TestSelectNextCheckRejectsUnsafeURLs(t *testing.T) {
	t.Parallel()

	unsafe := []string{
		"http://localhost/docs/",
		"http://127.0.0.1/docs/",
		"http://10.0.0.1/docs/",
		"http://169.254.1.1/docs/",
		"file:///tmp/docs",
		"https://user:password@example.com/docs/",
	}
	for _, target := range unsafe {
		message, gotURL := SelectNextCheck([]Link{{Text: "公式ドキュメント", URL: target}})
		if message != NextCheckFallback || gotURL != "" {
			t.Errorf("SelectNextCheck(%q) = (%q, %q), want fallback", target, message, gotURL)
		}
	}
}

func TestSummarizeFieldAndArticleLimits(t *testing.T) {
	t.Parallel()

	background := "APIの内部実装とアーキテクチャでは" + strings.Repeat("技", 170) + "。"
	impact := "開発者への影響と移行対応が必要で" + strings.Repeat("影", 170) + "。"
	key := "Go 2.0のコンパイラとSDKを更新しました" + strings.Repeat("要", 170) + "。"
	result := Summarize(Input{
		Title: "Go 2.0のAPIと移行対応",
		Body:  background + impact + key,
		Links: []Link{{Text: strings.Repeat("公式ドキュメント", 20), URL: "https://example.com/docs/v2"}},
	})

	checks := []struct {
		name  string
		value string
		limit int
	}{
		{"KeyPoints", result.KeyPoints, MaxKeyPointsGraphemes},
		{"TechnicalBackground", result.TechnicalBackground, MaxTechnicalBackgroundGraphemes},
		{"DeveloperImpact", result.DeveloperImpact, MaxDeveloperImpactGraphemes},
		{"NextCheck", result.NextCheck, MaxNextCheckGraphemes},
	}
	for _, check := range checks {
		if got := textutil.GraphemeCount(check.value); got > check.limit {
			t.Errorf("%s contains %d graphemes, limit is %d", check.name, got, check.limit)
		}
	}
	if result.QuotedGraphemes > MaxArticleTextGraphemes {
		t.Fatalf("QuotedGraphemes = %d, limit is %d", result.QuotedGraphemes, MaxArticleTextGraphemes)
	}
	if result.TotalTextGraphemes > MaxArticleTextGraphemes {
		t.Fatalf("TotalTextGraphemes = %d, limit is %d", result.TotalTextGraphemes, MaxArticleTextGraphemes)
	}
}

func TestContainsExactTermDoesNotMatchGoInsideGoogle(t *testing.T) {
	t.Parallel()

	if containsExactTerm("google cloudを利用します", "go") {
		t.Fatal("go matched inside google")
	}
	if !containsExactTerm("go 1.25を利用します", "go") {
		t.Fatal("standalone go did not match")
	}
}

func TestCandidateScoringSignals(t *testing.T) {
	t.Parallel()

	makeCandidate := func(index int, text string) candidate {
		normalized := textutil.NormalizeForComparison(text)
		return candidate{
			index:       index,
			text:        text,
			normalized:  normalized,
			compact:     compactComparison(normalized),
			identifiers: uniqueIdentifiers(normalized),
		}
	}

	plain := makeCandidate(0, "一般的な内容について詳しく説明している文章になります。")
	titleMatch := makeCandidate(0, "新しいデータ処理方式の公開について詳しく説明します。")
	if scoreCandidate(titleMatch, "新しいデータ処理方式の公開") <= scoreCandidate(plain, "新しいデータ処理方式の公開") {
		t.Fatal("title bigram overlap did not increase the score")
	}

	technical := makeCandidate(0, "コンパイラとランタイムの内部構造を詳しく説明する文章です。")
	if scoreCandidate(technical, "") <= scoreCandidate(plain, "") {
		t.Fatal("technical terms did not increase the score")
	}

	identifier := makeCandidate(0, "対象製品のバージョンv2.4.1について詳しく説明する文章です。")
	withoutIdentifier := makeCandidate(0, "対象製品の新バージョンについて詳しく説明する文章です。")
	if scoreCandidate(identifier, "") <= scoreCandidate(withoutIdentifier, "") {
		t.Fatal("version identifier did not increase the score")
	}

	early := makeCandidate(0, plain.text)
	late := makeCandidate(9, plain.text)
	if scoreCandidate(early, "") <= scoreCandidate(late, "") {
		t.Fatal("earlier position did not increase the score")
	}
}
