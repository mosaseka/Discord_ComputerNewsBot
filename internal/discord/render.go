package discord

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/clipperhouse/uax29/v2/graphemes"
)

const (
	MaxMessageContent   = 2000
	MaxEmbedsPerMessage = 10
	MaxEmbedTotal       = 6000
	MaxEmbedTitle       = 256
	MaxEmbedDescription = 4096
	MaxEmbedFields      = 25
	MaxFieldName        = 256
	MaxFieldValue       = 1024
	MaxFooterText       = 2048
	MaxAuthorName       = 256
)

type Item struct {
	Title      string
	URL        string
	Source     string
	Published  string
	Summary    string
	Background string
	Impact     string
	Next       string
	ImageURL   string
	Generated  bool
}

func (item Item) SummaryLabel() string {
	if item.Generated {
		return "Gemini生成要約"
	}
	return "抽出型要約"
}

type Briefing struct {
	Items          []Item
	PriorityTitle  string
	PriorityURL    string
	PriorityReason string
}

func BuildEmbeds(brief Briefing) []*discordgo.MessageEmbed {
	embeds := make([]*discordgo.MessageEmbed, 0, len(brief.Items)+1)
	for _, item := range brief.Items {
		embed := &discordgo.MessageEmbed{
			Title:       Truncate(item.Title, MaxEmbedTitle),
			URL:         safeHTTPURL(item.URL, false),
			Description: Truncate(formatSource(item.Source, item.Published), 500),
			Footer:      &discordgo.MessageEmbedFooter{Text: item.SummaryLabel()},
			Fields: []*discordgo.MessageEmbedField{
				{Name: "要点", Value: fallback(Truncate(item.Summary, 700), "記事から要点を抽出できませんでした")},
			},
		}
		if !item.Generated {
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Name: "技術的背景", Value: fallback(Truncate(item.Background, 600), "記事本文から詳細な技術的背景を抽出できませんでした"),
			})
		}
		embed.Fields = append(embed.Fields,
			&discordgo.MessageEmbedField{Name: "開発者・学生への影響", Value: fallback(Truncate(item.Impact, 600), "開発者への具体的な影響は元記事を確認してください")},
			&discordgo.MessageEmbedField{Name: "次に確認すべき点", Value: fallback(Truncate(item.Next, 600), "リリースノートまたは公式ドキュメントの確認を推奨します")},
		)
		if image := safeHTTPURL(item.ImageURL, true); image != "" {
			embed.Image = &discordgo.MessageEmbedImage{URL: image}
		}
		embeds = append(embeds, embed)
	}
	if brief.PriorityTitle != "" {
		title := Truncate("今日の最優先トピック: "+brief.PriorityTitle, MaxEmbedTitle)
		embeds = append(embeds, &discordgo.MessageEmbed{
			Title:       title,
			URL:         safeHTTPURL(brief.PriorityURL, false),
			Description: Truncate(brief.PriorityReason, 800),
			Color:       0xE67E22,
		})
	}
	return embeds
}

// BuildPreviewEmbeds fits the briefing into one ephemeral response. When the
// full embeds span messages, fit the field values within the remaining budget
// while keeping titles, source metadata, links and summary labels.
func BuildPreviewEmbeds(brief Briefing) ([]*discordgo.MessageEmbed, error) {
	embeds := BuildEmbeds(brief)
	batches, err := PackEmbeds(embeds)
	if err != nil || len(batches) <= 1 {
		return embeds, err
	}
	baseUnits, fieldCount := 0, 0
	for _, embed := range embeds {
		baseUnits += EmbedCharacters(embed)
		for _, field := range embed.Fields {
			baseUnits -= Units(field.Value)
			fieldCount++
		}
	}
	if len(embeds) > MaxEmbedsPerMessage || baseUnits >= MaxEmbedTotal || fieldCount == 0 {
		return nil, errors.New("briefing metadata exceeds the Discord preview limit")
	}
	fieldBudget := (MaxEmbedTotal - baseUnits) / fieldCount
	if fieldBudget < 1 {
		return nil, errors.New("briefing summary fields exceed the Discord preview limit")
	}
	for _, embed := range embeds {
		for _, field := range embed.Fields {
			// Keep the article link in the title. Omit a follow-up link
			// that would otherwise be cut in the middle of the URL.
			if Units(field.Value) > fieldBudget && field.Name == "次に確認すべき点" {
				if start := strings.LastIndex(field.Value, "\n"); start >= 0 {
					field.Value = field.Value[:start]
				}
			}
			field.Value = Truncate(field.Value, fieldBudget)
		}
	}
	return embeds, nil
}

func PackEmbeds(embeds []*discordgo.MessageEmbed) ([][]*discordgo.MessageEmbed, error) {
	var batches [][]*discordgo.MessageEmbed
	var current []*discordgo.MessageEmbed
	currentChars := 0
	for _, embed := range embeds {
		if err := ValidateEmbed(embed); err != nil {
			return nil, err
		}
		chars := EmbedCharacters(embed)
		if len(current) > 0 && (len(current)+1 > MaxEmbedsPerMessage || currentChars+chars > MaxEmbedTotal) {
			batches = append(batches, current)
			current = nil
			currentChars = 0
		}
		current = append(current, embed)
		currentChars += chars
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches, nil
}

func ValidateEmbed(embed *discordgo.MessageEmbed) error {
	if embed == nil {
		return errors.New("nil embed")
	}
	if Units(embed.Title) > MaxEmbedTitle {
		return fmt.Errorf("embed title exceeds %d characters", MaxEmbedTitle)
	}
	if Units(embed.Description) > MaxEmbedDescription {
		return fmt.Errorf("embed description exceeds %d characters", MaxEmbedDescription)
	}
	if len(embed.Fields) > MaxEmbedFields {
		return fmt.Errorf("embed has more than %d fields", MaxEmbedFields)
	}
	for _, field := range embed.Fields {
		if field == nil || Units(field.Name) > MaxFieldName || Units(field.Value) > MaxFieldValue {
			return errors.New("embed field exceeds Discord limits")
		}
	}
	if embed.Footer != nil && Units(embed.Footer.Text) > MaxFooterText {
		return errors.New("embed footer exceeds Discord limits")
	}
	if embed.Author != nil && Units(embed.Author.Name) > MaxAuthorName {
		return errors.New("embed author exceeds Discord limits")
	}
	if EmbedCharacters(embed) > MaxEmbedTotal {
		return errors.New("single embed exceeds total Discord limit")
	}
	return nil
}

func EmbedCharacters(embed *discordgo.MessageEmbed) int {
	if embed == nil {
		return 0
	}
	total := Units(embed.Title) + Units(embed.Description)
	for _, field := range embed.Fields {
		if field != nil {
			total += Units(field.Name) + Units(field.Value)
		}
	}
	if embed.Footer != nil {
		total += Units(embed.Footer.Text)
	}
	if embed.Author != nil {
		total += Units(embed.Author.Name)
	}
	return total
}

// Units counts UTF-16 code units. Discord documents character limits without a
// precise Unicode unit; UTF-16 is conservative for non-BMP characters.
func Units(value string) int {
	value = strings.ToValidUTF8(value, "�")
	return len(utf16.Encode([]rune(value)))
}

// Truncate keeps extended grapheme clusters intact while enforcing a UTF-16
// budget. An ellipsis is included when content is removed.
func Truncate(value string, maxUnits int) string {
	if maxUnits <= 0 {
		return ""
	}
	value = strings.ToValidUTF8(value, "�")
	if Units(value) <= maxUnits {
		return value
	}
	ellipsis := "…"
	budget := maxUnits - Units(ellipsis)
	if budget <= 0 {
		return ellipsis
	}
	iter := graphemes.FromString(value)
	end := 0
	used := 0
	for iter.Next() {
		cluster := iter.Value()
		cost := Units(cluster)
		if used+cost > budget {
			break
		}
		used += cost
		end = iter.End()
	}
	return strings.TrimSpace(value[:end]) + ellipsis
}

func formatSource(source, published string) string {
	source = strings.TrimSpace(source)
	published = strings.TrimSpace(published)
	switch {
	case source != "" && published != "":
		return "情報源: " + source + "\n公開日時: " + published
	case source != "":
		return "情報源: " + source
	case published != "":
		return "公開日時: " + published
	default:
		return ""
	}
}

func fallback(value, fallbackValue string) string {
	if strings.TrimSpace(value) == "" {
		return fallbackValue
	}
	return value
}

func safeHTTPURL(raw string, httpsOnly bool) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User != nil || u.Hostname() == "" || u.Fragment != "" && raw == "" {
		return ""
	}
	if httpsOnly && u.Scheme != "https" {
		return ""
	}
	if !httpsOnly && u.Scheme != "https" && u.Scheme != "http" {
		return ""
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !isPublicIP(ip) {
		return ""
	}
	return u.String()
}

func isPublicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsUnspecified()
}

func validUTF8(value string) bool { return utf8.ValidString(value) }
