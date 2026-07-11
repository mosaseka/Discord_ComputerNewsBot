package article

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

var (
	errInvalidURL = errors.New("article URL must be an absolute HTTP(S) URL without credentials")

	titleWhitespace = regexp.MustCompile(`\s+`)
	// These suffixes are presentation-only decorations commonly appended by
	// feeds. Repeated removal handles combinations such as "- Site | Blog".
	titleSuffix = regexp.MustCompile(`(?i)\s*(?:[-–—|｜:]\s*)?(?:publickey|gihyo\.jp|codezine|github\s*blog(?:\s*日本語)?|atcoderinfo|jvn|jpcert/?cc|ipa|developers?\s*blog|tech\s*blog|技術ブログ|開発者ブログ)\s*$`)
)

// NormalizeURL returns a stable identity URL. It intentionally does not make
// network-safety claims; callers must still apply the SSRF policy before I/O.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil {
		return "", errInvalidURL
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errInvalidURL
	}

	hostname := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if hostname == "" {
		return "", errInvalidURL
	}
	port := u.Port()
	if (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		u.Host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		u.Host = "[" + hostname + "]"
	} else {
		u.Host = hostname
	}

	u.Fragment = ""
	// Treat a literal trailing slash as presentation, including the root slash.
	// An escaped slash (%2F) can be significant and must not be removed.
	if u.RawPath == "" {
		u.Path = strings.TrimRight(u.Path, "/")
	} else {
		trimmed := strings.TrimRight(u.RawPath, "/")
		removed := len(u.RawPath) - len(trimmed)
		u.RawPath = trimmed
		for range removed {
			u.Path = strings.TrimSuffix(u.Path, "/")
		}
		if u.Path == "" {
			u.RawPath = ""
		}
	}

	query := u.Query()
	for key := range query {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "utm_") || lower == "ref" {
			query.Del(key)
			continue
		}
		values := query[key]
		sort.Strings(values)
		query[key] = values
	}
	u.RawQuery = query.Encode()
	u.ForceQuery = false
	return u.String(), nil
}

// NormalizeTitle produces a comparison key without altering the display title.
func NormalizeTitle(title string) string {
	title = norm.NFKC.String(strings.TrimSpace(title))
	title = strings.ToLower(title)
	for {
		trimmed := titleSuffix.ReplaceAllString(title, "")
		if trimmed == title {
			break
		}
		title = strings.TrimSpace(trimmed)
	}

	var b strings.Builder
	b.Grow(len(title))
	spacePending := false
	for _, r := range title {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			if spacePending && b.Len() > 0 {
				b.WriteByte(' ')
			}
			spacePending = false
			b.WriteRune(r)
		case unicode.IsSpace(r):
			spacePending = true
		default:
			// Punctuation is insignificant for title identity. Preserve a word
			// boundary for ASCII-like tokens instead of joining them.
			spacePending = true
		}
	}
	return titleWhitespace.ReplaceAllString(strings.TrimSpace(b.String()), " ")
}
