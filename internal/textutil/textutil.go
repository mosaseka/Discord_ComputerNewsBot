// Package textutil contains the Unicode-aware text operations shared by the
// deterministic news pipeline.
package textutil

import (
	stdhtml "html"
	"strings"

	"github.com/clipperhouse/uax29/v2/graphemes"
	nethtml "golang.org/x/net/html"
	"golang.org/x/text/unicode/norm"
)

const replacementCharacter = "\uFFFD"

// NormalizeNFKC replaces invalid UTF-8 and applies Unicode NFKC
// normalization. It deliberately does not change whitespace or case.
func NormalizeNFKC(s string) string {
	return norm.NFKC.String(strings.ToValidUTF8(s, replacementCharacter))
}

// CollapseWhitespace replaces every non-empty run of Unicode whitespace with
// one ASCII space and trims leading and trailing whitespace.
func CollapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// NormalizeForComparison creates a case-folded, whitespace-normalized NFKC
// representation. Callers should retain the original text for display.
func NormalizeForComparison(s string) string {
	return CollapseWhitespace(strings.ToLower(NormalizeNFKC(s)))
}

// SplitGraphemes returns the extended grapheme clusters in s. Invalid UTF-8 is
// replaced before segmentation so that the iterator always receives valid
// input.
func SplitGraphemes(s string) []string {
	s = strings.ToValidUTF8(s, replacementCharacter)
	if s == "" {
		return nil
	}

	result := make([]string, 0, len(s))
	iter := graphemes.FromString(s)
	for iter.Next() {
		result = append(result, iter.Value())
	}
	return result
}

// GraphemeCount returns the number of extended grapheme clusters in s.
func GraphemeCount(s string) int {
	s = strings.ToValidUTF8(s, replacementCharacter)
	count := 0
	iter := graphemes.FromString(s)
	for iter.Next() {
		count++
	}
	return count
}

// TruncateGraphemes returns at most max extended grapheme clusters from the
// start of s. It never splits a combining sequence or emoji cluster.
func TruncateGraphemes(s string, max int) string {
	if max <= 0 || s == "" {
		return ""
	}

	s = strings.ToValidUTF8(s, replacementCharacter)
	iter := graphemes.FromString(s)
	count := 0
	end := 0
	for iter.Next() {
		count++
		if count > max {
			return s[:end]
		}
		end = iter.End()
	}
	return s
}

// TruncateGraphemesWithSuffix truncates s to max grapheme clusters including
// suffix. If suffix itself is too long, suffix is truncated to fit.
func TruncateGraphemesWithSuffix(s string, max int, suffix string) string {
	if max <= 0 {
		return ""
	}
	s = strings.ToValidUTF8(s, replacementCharacter)
	if GraphemeCount(s) <= max {
		return s
	}

	suffix = strings.ToValidUTF8(suffix, replacementCharacter)
	suffixCount := GraphemeCount(suffix)
	if suffixCount >= max {
		return TruncateGraphemes(suffix, max)
	}

	prefix := strings.TrimSpace(TruncateGraphemes(s, max-suffixCount))
	return prefix + suffix
}

// StripHTML converts an HTML fragment to plain text. Script-like content is
// ignored, and common block elements become line boundaries. The result keeps
// meaningful line breaks while collapsing whitespace within each line.
func StripHTML(input string) string {
	input = strings.ToValidUTF8(input, replacementCharacter)
	if input == "" {
		return ""
	}

	tokenizer := nethtml.NewTokenizer(strings.NewReader(input))
	var output strings.Builder
	var skippedTag string
	var skippedDepth int

	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case nethtml.ErrorToken:
			return cleanBlockText(output.String())

		case nethtml.StartTagToken:
			nameBytes, _ := tokenizer.TagName()
			name := strings.ToLower(string(nameBytes))
			if skippedDepth > 0 {
				if name == skippedTag {
					skippedDepth++
				}
				continue
			}
			if isSkippedElement(name) {
				skippedTag = name
				skippedDepth = 1
				continue
			}
			if isBlockElement(name) {
				output.WriteByte('\n')
			}

		case nethtml.SelfClosingTagToken:
			nameBytes, _ := tokenizer.TagName()
			if skippedDepth == 0 && isBlockElement(strings.ToLower(string(nameBytes))) {
				output.WriteByte('\n')
			}

		case nethtml.EndTagToken:
			nameBytes, _ := tokenizer.TagName()
			name := strings.ToLower(string(nameBytes))
			if skippedDepth > 0 {
				if name == skippedTag {
					skippedDepth--
					if skippedDepth == 0 {
						skippedTag = ""
					}
				}
				continue
			}
			if isBlockElement(name) {
				output.WriteByte('\n')
			}

		case nethtml.TextToken:
			if skippedDepth == 0 {
				output.WriteString(stdhtml.UnescapeString(string(tokenizer.Text())))
			}
		}
	}
}

func isSkippedElement(name string) bool {
	switch name {
	case "script", "style", "noscript", "template":
		return true
	default:
		return false
	}
}

func isBlockElement(name string) bool {
	switch name {
	case "address", "article", "aside", "blockquote", "br", "dd", "div",
		"dl", "dt", "figcaption", "figure", "footer", "h1", "h2", "h3",
		"h4", "h5", "h6", "header", "hr", "li", "main", "nav", "ol", "p",
		"pre", "section", "table", "tbody", "td", "tfoot", "th", "thead", "tr", "ul":
		return true
	default:
		return false
	}
}

func cleanBlockText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		line = CollapseWhitespace(line)
		if line == "" {
			continue
		}
		cleaned = append(cleaned, line)
	}
	return strings.Join(cleaned, "\n")
}
