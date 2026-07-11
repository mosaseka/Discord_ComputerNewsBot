package config

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/article"
)

const (
	DefaultTimezone       = "Asia/Tokyo"
	DefaultCron           = "0 8 * * *"
	DefaultDatabasePath   = "data/bot.db"
	DefaultLogLevel       = "info"
	DefaultUserAgent      = "DiscordComputerNewsBot/1.0 (+https://github.com/mosaseka/Discord_ComputerNewsBot)"
	DefaultMaxArticleSize = int64(2 * 1024 * 1024)
)

type Mode struct {
	Once    bool
	DryRun  bool
	Fixture string
}

type Config struct {
	DiscordBotToken      string
	DiscordApplicationID string
	DiscordGuildID       string
	DiscordChannelID     string
	AllowedUserIDs       map[string]struct{}
	TimezoneName         string
	Location             *time.Location
	BriefCron            string
	DatabasePath         string
	LogLevel             string
	HTTPUserAgent        string
	MaxArticleBytes      int64
	ArticleFetchEnabled  bool
	CommandStatusPublic  bool
	Mode                 Mode
}

type Feed struct {
	Name       string  `yaml:"name"`
	URL        string  `yaml:"url"`
	Enabled    bool    `yaml:"enabled"`
	Weight     float64 `yaml:"weight"`
	Language   string  `yaml:"language"`
	Category   string  `yaml:"category"`
	IsOfficial bool    `yaml:"is_official"`
}

type FeedFile struct {
	Feeds []Feed `yaml:"feeds"`
}

var (
	languagePattern = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)
	categoryPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

func Load(mode Mode) (Config, error) {
	if mode.Fixture != "" {
		if !mode.Once {
			return Config{}, errors.New("--fixture requires --once")
		}
		mode.DryRun = true
	}
	if mode.DryRun && !mode.Once {
		return Config{}, errors.New("--dry-run requires --once")
	}

	tz := envOr("APP_TIMEZONE", DefaultTimezone)
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return Config{}, fmt.Errorf("load APP_TIMEZONE %q: %w", tz, err)
	}
	cronExpr := envOr("BRIEF_CRON", DefaultCron)
	if len(strings.Fields(cronExpr)) != 5 {
		return Config{}, fmt.Errorf("BRIEF_CRON must contain exactly five fields: %q", cronExpr)
	}

	maxBytes, err := envInt64("MAX_ARTICLE_BYTES", DefaultMaxArticleSize)
	if err != nil || maxBytes <= 0 {
		return Config{}, fmt.Errorf("MAX_ARTICLE_BYTES must be a positive integer")
	}
	articleFetch, err := envBool("ARTICLE_FETCH_ENABLED", true)
	if err != nil {
		return Config{}, err
	}
	statusPublic, err := envBool("COMMAND_STATUS_PUBLIC", false)
	if err != nil {
		return Config{}, err
	}
	allowed, err := parseSnowflakes(os.Getenv("DISCORD_ALLOWED_USER_IDS"))
	if err != nil {
		return Config{}, fmt.Errorf("DISCORD_ALLOWED_USER_IDS: %w", err)
	}
	level := strings.ToLower(envOr("LOG_LEVEL", DefaultLogLevel))
	switch level {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("LOG_LEVEL must be debug, info, warn, or error: %q", level)
	}

	cfg := Config{
		DiscordBotToken:      strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN")),
		DiscordApplicationID: strings.TrimSpace(os.Getenv("DISCORD_APPLICATION_ID")),
		DiscordGuildID:       strings.TrimSpace(os.Getenv("DISCORD_GUILD_ID")),
		DiscordChannelID:     strings.TrimSpace(os.Getenv("DISCORD_CHANNEL_ID")),
		AllowedUserIDs:       allowed,
		TimezoneName:         tz,
		Location:             loc,
		BriefCron:            cronExpr,
		DatabasePath:         envOr("DATABASE_PATH", DefaultDatabasePath),
		LogLevel:             level,
		HTTPUserAgent:        envOr("HTTP_USER_AGENT", DefaultUserAgent),
		MaxArticleBytes:      maxBytes,
		ArticleFetchEnabled:  articleFetch,
		CommandStatusPublic:  statusPublic,
		Mode:                 mode,
	}
	if err := cfg.validateMode(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validateMode() error {
	if strings.TrimSpace(c.DatabasePath) == "" {
		return errors.New("DATABASE_PATH must not be empty")
	}
	if strings.TrimSpace(c.HTTPUserAgent) == "" {
		return errors.New("HTTP_USER_AGENT must not be empty")
	}
	if c.Mode.DryRun {
		return nil
	}
	if c.DiscordBotToken == "" || c.DiscordChannelID == "" {
		return errors.New("DISCORD_BOT_TOKEN and DISCORD_CHANNEL_ID are required when posting")
	}
	if !c.Mode.Once && c.DiscordApplicationID == "" {
		return errors.New("DISCORD_APPLICATION_ID is required in normal mode")
	}
	return nil
}

func LoadFeeds(path string) ([]Feed, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open feeds config: %w", err)
	}
	defer f.Close()
	return DecodeFeeds(f)
}

func DecodeFeeds(r io.Reader) ([]Feed, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var file FeedFile
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode feeds config: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("feeds config must contain exactly one YAML document")
		}
		return nil, fmt.Errorf("decode trailing feeds config: %w", err)
	}
	if len(file.Feeds) == 0 {
		return nil, errors.New("feeds config contains no feeds")
	}
	names := make(map[string]struct{}, len(file.Feeds))
	urls := make(map[string]struct{}, len(file.Feeds))
	for i := range file.Feeds {
		f := &file.Feeds[i]
		f.Name = strings.TrimSpace(f.Name)
		f.URL = strings.TrimSpace(f.URL)
		f.Language = strings.TrimSpace(f.Language)
		f.Category = strings.TrimSpace(f.Category)
		if f.Name == "" || f.URL == "" || f.Language == "" || f.Category == "" {
			return nil, fmt.Errorf("feed %d has an empty required field", i)
		}
		if f.Weight <= 0 || f.Weight > 5 {
			return nil, fmt.Errorf("feed %q weight must be greater than 0 and at most 5", f.Name)
		}
		if !languagePattern.MatchString(f.Language) {
			return nil, fmt.Errorf("feed %q has an invalid language tag %q", f.Name, f.Language)
		}
		if !categoryPattern.MatchString(f.Category) {
			return nil, fmt.Errorf("feed %q has an invalid category %q", f.Name, f.Category)
		}
		u, err := url.Parse(f.URL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return nil, fmt.Errorf("feed %q has an invalid HTTP(S) URL", f.Name)
		}
		nameKey := strings.ToLower(f.Name)
		urlKey, err := article.NormalizeURL(f.URL)
		if err != nil {
			return nil, fmt.Errorf("feed %q has an invalid URL identity", f.Name)
		}
		if _, exists := names[nameKey]; exists {
			return nil, fmt.Errorf("duplicate feed name %q", f.Name)
		}
		if _, exists := urls[urlKey]; exists {
			return nil, fmt.Errorf("duplicate feed URL %q", f.URL)
		}
		names[nameKey] = struct{}{}
		urls[urlKey] = struct{}{}
	}
	return file.Feeds, nil
}

func (c Config) IsAllowedUser(id string) bool {
	_, ok := c.AllowedUserIDs[id]
	return ok
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return v, nil
}

func envInt64(name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

func parseSnowflakes(raw string) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			return nil, errors.New("empty user ID")
		}
		if _, err := strconv.ParseUint(id, 10, 64); err != nil {
			return nil, fmt.Errorf("invalid user ID %q", id)
		}
		out[id] = struct{}{}
	}
	return out, nil
}
