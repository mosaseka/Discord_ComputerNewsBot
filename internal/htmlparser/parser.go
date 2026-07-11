// Package htmlparser extracts article text and metadata from bytes supplied by
// the caller. It never performs network requests.
package htmlparser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/security"
	"golang.org/x/net/html"
)

const DefaultMaxBytes int64 = 2 << 20

var (
	ErrTooLarge       = errors.New("HTML document is too large")
	ErrInvalidPageURL = errors.New("invalid article page URL")
	ErrInvalidHTML    = errors.New("invalid HTML document")
)

// Link is an absolute, HTTP(S)-only link found in cleaned article content.
type Link struct {
	Text string
	URL  string
	Rel  string
}

// Result contains cleaned readability output plus independently extracted
// OpenGraph, meta, canonical, and JSON-LD metadata.
type Result struct {
	Title        string
	TextContent  string
	HTMLContent  string
	Excerpt      string
	Description  string
	Byline       string
	SiteName     string
	Language     string
	CanonicalURL string
	ImageURL     string
	PublishedAt  *time.Time
	ModifiedAt   *time.Time
	Links        []Link
}

// Parse reads at most maxBytes and extracts an article. pageURL is used only
// for resolving relative metadata and links. A non-positive max selects 2 MiB.
func Parse(ctx context.Context, input io.Reader, pageURL *url.URL, maxBytes int64) (Result, error) {
	if err := validatePageURL(pageURL); err != nil {
		return Result{}, err
	}
	if input == nil {
		return Result{}, fmt.Errorf("%w: reader is nil", ErrInvalidHTML)
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	body, err := readLimited(input, maxBytes)
	if err != nil {
		return Result{}, err
	}
	return parseBytes(ctx, body, pageURL)
}

// ParseBytes extracts an article from an in-memory UTF-8 HTML document.
func ParseBytes(ctx context.Context, body []byte, pageURL *url.URL) (Result, error) {
	if int64(len(body)) > DefaultMaxBytes {
		return Result{}, fmt.Errorf("%w: document exceeds %d bytes", ErrTooLarge, DefaultMaxBytes)
	}
	if err := validatePageURL(pageURL); err != nil {
		return Result{}, err
	}
	return parseBytes(ctx, body, pageURL)
}

func parseBytes(ctx context.Context, body []byte, pageURL *url.URL) (Result, error) {
	if !utf8.Valid(body) {
		return Result{}, fmt.Errorf("%w: input is not UTF-8", ErrInvalidHTML)
	}
	document, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v", ErrInvalidHTML, err)
	}
	metadata := extractMetadata(document, pageURL)
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	// FromReader is intentionally used instead of FromURL: this package has no
	// HTTP client and cannot bypass the caller's network policy.
	article, err := readability.FromReader(bytes.NewReader(body), pageURL)
	if err != nil {
		return Result{}, fmt.Errorf("extract readable article: %w", err)
	}
	if article.Node != nil {
		sanitizeArticleNode(article.Node, pageURL)
	}

	var textOutput, htmlOutput bytes.Buffer
	if err := article.RenderText(&textOutput); err != nil {
		return Result{}, fmt.Errorf("render readable text: %w", err)
	}
	if err := article.RenderHTML(&htmlOutput); err != nil {
		return Result{}, fmt.Errorf("render readable HTML: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	published, publishedErr := article.PublishedTime()
	modified, modifiedErr := article.ModifiedTime()
	result := Result{
		Title:        firstNonEmpty(article.Title(), metadata.title),
		TextContent:  normalizeText(textOutput.String()),
		HTMLContent:  strings.TrimSpace(htmlOutput.String()),
		Excerpt:      normalizeInline(article.Excerpt()),
		Description:  firstNonEmpty(metadata.description, article.Excerpt()),
		Byline:       firstNonEmpty(article.Byline(), metadata.byline),
		SiteName:     firstNonEmpty(article.SiteName(), metadata.siteName),
		Language:     firstNonEmpty(article.Language(), metadata.language),
		CanonicalURL: metadata.canonicalURL,
		ImageURL:     firstNonEmpty(metadata.imageURL, safeResolvedURL(pageURL, article.ImageURL())),
		Links:        extractLinks(article.Node, pageURL),
	}
	result.Title = normalizeInline(result.Title)
	result.Description = normalizeInline(result.Description)
	result.Byline = normalizeInline(result.Byline)
	result.SiteName = normalizeInline(result.SiteName)
	result.Language = normalizeInline(result.Language)
	if publishedErr == nil {
		result.PublishedAt = timePointer(published)
	} else {
		result.PublishedAt = metadata.publishedAt
	}
	if modifiedErr == nil {
		result.ModifiedAt = timePointer(modified)
	} else {
		result.ModifiedAt = metadata.modifiedAt
	}
	return result, nil
}

func readLimited(r io.Reader, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, fmt.Errorf("read HTML: %w", err)
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("%w: document exceeds %d bytes", ErrTooLarge, max)
	}
	return body, nil
}

func validatePageURL(u *url.URL) error {
	if u == nil || u.Opaque != "" || u.User != nil || u.Hostname() == "" {
		return ErrInvalidPageURL
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("%w: scheme %q", ErrInvalidPageURL, u.Scheme)
	}
	if safeResolvedURL(u, u.String()) == "" {
		return fmt.Errorf("%w: URL is not a public HTTP(S) target", ErrInvalidPageURL)
	}
	return nil
}

type pageMetadata struct {
	title        string
	description  string
	byline       string
	siteName     string
	language     string
	canonicalURL string
	imageURL     string
	publishedAt  *time.Time
	modifiedAt   *time.Time
}

func extractMetadata(document *html.Node, pageURL *url.URL) pageMetadata {
	meta := make(map[string]string)
	var title, canonical, language string
	var jsonObjects []map[string]any
	walk(document, func(node *html.Node) {
		if node.Type != html.ElementNode {
			return
		}
		switch strings.ToLower(node.Data) {
		case "html":
			language = attr(node, "lang")
		case "title":
			if title == "" {
				title = textContent(node)
			}
		case "meta":
			key := strings.ToLower(firstNonEmpty(attr(node, "property"), attr(node, "name"), attr(node, "itemprop")))
			value := strings.TrimSpace(attr(node, "content"))
			if key != "" && value != "" && meta[key] == "" {
				meta[key] = value
			}
		case "link":
			if hasRel(node, "canonical") && canonical == "" {
				canonical = safeResolvedURL(pageURL, attr(node, "href"))
			}
		case "script":
			if strings.EqualFold(strings.TrimSpace(attr(node, "type")), "application/ld+json") {
				jsonObjects = append(jsonObjects, decodeJSONLD(textContent(node))...)
			}
		}
	})
	jsonMeta := metadataFromJSONLD(jsonObjects, pageURL)
	result := pageMetadata{
		title:        firstNonEmpty(meta["og:title"], meta["twitter:title"], jsonMeta.title, title),
		description:  firstNonEmpty(meta["description"], meta["og:description"], meta["twitter:description"], jsonMeta.description),
		byline:       firstNonEmpty(meta["author"], meta["article:author"], jsonMeta.byline),
		siteName:     firstNonEmpty(meta["og:site_name"], jsonMeta.siteName),
		language:     firstNonEmpty(meta["og:locale"], jsonMeta.language, language),
		canonicalURL: firstNonEmpty(canonical, safeResolvedURL(pageURL, meta["og:url"]), jsonMeta.canonicalURL),
		imageURL:     firstNonEmpty(safeResolvedURL(pageURL, meta["og:image:secure_url"]), safeResolvedURL(pageURL, meta["og:image"]), safeResolvedURL(pageURL, meta["twitter:image"]), jsonMeta.imageURL),
		publishedAt:  firstTime(parseTime(meta["article:published_time"]), parseTime(meta["datepublished"]), jsonMeta.publishedAt),
		modifiedAt:   firstTime(parseTime(meta["article:modified_time"]), parseTime(meta["datemodified"]), jsonMeta.modifiedAt),
	}
	return result
}

func decodeJSONLD(raw string) []map[string]any {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "<!--")
	raw = strings.TrimSuffix(raw, "-->")
	raw = strings.TrimPrefix(raw, "<![CDATA[")
	raw = strings.TrimSuffix(raw, "]]>")
	var value any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	var result []map[string]any
	collectArticleObjects(value, &result, 0)
	return result
}

func collectArticleObjects(value any, result *[]map[string]any, depth int) {
	if depth > 32 {
		return
	}
	switch item := value.(type) {
	case []any:
		for _, child := range item {
			collectArticleObjects(child, result, depth+1)
		}
	case map[string]any:
		if isArticleType(item["@type"]) {
			*result = append(*result, item)
		}
		if graph, ok := item["@graph"]; ok {
			collectArticleObjects(graph, result, depth+1)
		}
	}
}

func isArticleType(value any) bool {
	check := func(value string) bool {
		value = strings.ToLower(strings.TrimSpace(value))
		return strings.HasSuffix(value, "article") || value == "report" || value == "blogposting" || value == "socialmediaposting"
	}
	switch item := value.(type) {
	case string:
		return check(item)
	case []any:
		for _, child := range item {
			if text, ok := child.(string); ok && check(text) {
				return true
			}
		}
	}
	return false
}

func metadataFromJSONLD(objects []map[string]any, pageURL *url.URL) pageMetadata {
	var result pageMetadata
	for _, object := range objects {
		result.title = firstNonEmpty(result.title, stringValue(object["headline"]), stringValue(object["name"]))
		result.description = firstNonEmpty(result.description, stringValue(object["description"]))
		result.byline = firstNonEmpty(result.byline, namesValue(object["author"]))
		result.siteName = firstNonEmpty(result.siteName, namesValue(object["publisher"]))
		result.language = firstNonEmpty(result.language, stringValue(object["inLanguage"]))
		result.imageURL = firstNonEmpty(result.imageURL, safeResolvedURL(pageURL, URLValue(object["image"])))
		result.canonicalURL = firstNonEmpty(result.canonicalURL, safeResolvedURL(pageURL, URLValue(object["mainEntityOfPage"])), safeResolvedURL(pageURL, URLValue(object["url"])))
		result.publishedAt = firstTime(result.publishedAt, parseTime(stringValue(object["datePublished"])))
		result.modifiedAt = firstTime(result.modifiedAt, parseTime(stringValue(object["dateModified"])))
	}
	return result
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func namesValue(value any) string {
	var names []string
	var collect func(any)
	collect = func(value any) {
		switch item := value.(type) {
		case string:
			if item = normalizeInline(item); item != "" {
				names = append(names, item)
			}
		case map[string]any:
			collect(item["name"])
		case []any:
			for _, child := range item {
				collect(child)
			}
		}
	}
	collect(value)
	return strings.Join(names, ", ")
}

// URLValue returns a URL-like string from common schema.org scalar, object,
// and list shapes.
func URLValue(value any) string {
	switch item := value.(type) {
	case string:
		return strings.TrimSpace(item)
	case map[string]any:
		return firstNonEmpty(stringValue(item["url"]), stringValue(item["contentUrl"]), stringValue(item["@id"]))
	case []any:
		for _, child := range item {
			if value := URLValue(child); value != "" {
				return value
			}
		}
	}
	return ""
}

func sanitizeArticleNode(root *html.Node, base *url.URL) {
	var clean func(*html.Node)
	clean = func(parent *html.Node) {
		for child := parent.FirstChild; child != nil; {
			next := child.NextSibling
			if child.Type == html.ElementNode && (isNoiseElement(child.Data) || hasNoiseAttributes(child)) {
				parent.RemoveChild(child)
				child = next
				continue
			}
			if child.Type == html.ElementNode {
				attributes := child.Attr[:0]
				for _, attribute := range child.Attr {
					name := strings.ToLower(attribute.Key)
					if strings.HasPrefix(name, "on") || name == "style" || name == "srcset" {
						continue
					}
					if (name == "href" && strings.EqualFold(child.Data, "a")) || (name == "src" && strings.EqualFold(child.Data, "img")) {
						resolved := safeResolvedURL(base, attribute.Val)
						if resolved == "" {
							continue
						}
						attribute.Val = resolved
					}
					attributes = append(attributes, attribute)
				}
				child.Attr = attributes
			}
			clean(child)
			child = next
		}
	}
	clean(root)
}

func hasNoiseAttributes(node *html.Node) bool {
	if node == nil {
		return false
	}
	noiseTokens := map[string]struct{}{
		"ad": {}, "ads": {}, "advert": {}, "advertisement": {}, "sponsored": {},
		"related": {}, "recommend": {}, "recommendation": {}, "share": {}, "social": {}, "sns": {},
		"cookie": {}, "consent": {}, "comment": {}, "comments": {}, "navigation": {}, "sidebar": {},
	}
	for _, attribute := range node.Attr {
		name := strings.ToLower(attribute.Key)
		if name != "id" && name != "class" && name != "role" && name != "aria-label" {
			continue
		}
		for _, token := range strings.FieldsFunc(strings.ToLower(attribute.Val), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsNumber(r)
		}) {
			if _, noisy := noiseTokens[token]; noisy {
				return true
			}
		}
	}
	return false
}

func isNoiseElement(name string) bool {
	switch strings.ToLower(name) {
	case "script", "style", "nav", "header", "footer", "aside", "form", "button", "noscript", "iframe", "object", "embed":
		return true
	default:
		return false
	}
}

func extractLinks(root *html.Node, base *url.URL) []Link {
	if root == nil {
		return nil
	}
	links := make([]Link, 0, 8)
	seen := make(map[string]struct{})
	walk(root, func(node *html.Node) {
		if node.Type != html.ElementNode || !strings.EqualFold(node.Data, "a") {
			return
		}
		resolved := safeResolvedURL(base, attr(node, "href"))
		if resolved == "" {
			return
		}
		text := normalizeInline(textContent(node))
		key := resolved + "\x00" + text
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		links = append(links, Link{Text: text, URL: resolved, Rel: normalizeInline(attr(node, "rel"))})
	})
	return links
}

func safeResolvedURL(base *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || base == nil {
		return ""
	}
	reference, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	resolved := base.ResolveReference(reference)
	if resolved.Opaque != "" || resolved.User != nil || resolved.Hostname() == "" {
		return ""
	}
	if !strings.EqualFold(resolved.Scheme, "http") && !strings.EqualFold(resolved.Scheme, "https") {
		return ""
	}
	host := strings.ToLower(strings.TrimSuffix(resolved.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.Contains(host, "%") {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && !security.IsPublicIP(ip) {
		return ""
	}
	resolved.Fragment = ""
	return resolved.String()
}

func parseTime(value string) *time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05-0700",
		time.RFC1123Z,
		time.RFC1123,
		time.RFC822Z,
		"2006-01-02 15:04:05 -0700",
		"2006-01-02 15:04:05",
		"2006-01-02",
		"2006年01月02日 15時04分",
		"2006年01月02日",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return timePointer(parsed)
		}
	}
	return nil
}

func firstTime(values ...*time.Time) *time.Time {
	for _, value := range values {
		if value != nil && !value.IsZero() {
			return timePointer(*value)
		}
	}
	return nil
}

func timePointer(value time.Time) *time.Time {
	copy := value
	return &copy
}

func walk(node *html.Node, visit func(*html.Node)) {
	if node == nil {
		return
	}
	visit(node)
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		walk(child, visit)
	}
}

func attr(node *html.Node, name string) string {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, name) {
			return attribute.Val
		}
	}
	return ""
}

func hasRel(node *html.Node, wanted string) bool {
	for _, value := range strings.Fields(strings.ToLower(attr(node, "rel"))) {
		if value == strings.ToLower(wanted) {
			return true
		}
	}
	return false
}

func textContent(node *html.Node) string {
	var output strings.Builder
	walk(node, func(current *html.Node) {
		if current.Type == html.TextNode {
			output.WriteString(current.Data)
			output.WriteByte(' ')
		}
	})
	return normalizeInline(output.String())
}

var horizontalSpace = regexp.MustCompile(`[\t\f\v\p{Zs}]+`)

func normalizeText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	lines := strings.Split(value, "\n")
	result := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.TrimSpace(horizontalSpace.ReplaceAllString(line, " "))
		if line == "" {
			if len(result) > 0 && !blank {
				result = append(result, "")
				blank = true
			}
			continue
		}
		result = append(result, line)
		blank = false
	}
	for len(result) > 0 && result[len(result)-1] == "" {
		result = result[:len(result)-1]
	}
	return strings.Join(result, "\n")
}

func normalizeInline(value string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, collapseWhitespace(value)))
}

func collapseWhitespace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
