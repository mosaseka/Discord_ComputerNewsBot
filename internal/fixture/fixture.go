// Package fixture loads deterministic, completely offline feed fixtures.
package fixture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/mosaseka/Discord_ComputerNewsBot/internal/article"
	"github.com/mosaseka/Discord_ComputerNewsBot/internal/feed"
)

type manifest struct {
	Now   time.Time      `yaml:"now"`
	Feeds []manifestFeed `yaml:"feeds"`
}

type manifestFeed struct {
	File       string  `yaml:"file"`
	Name       string  `yaml:"name"`
	URL        string  `yaml:"url"`
	Weight     float64 `yaml:"weight"`
	Language   string  `yaml:"language"`
	Category   string  `yaml:"category"`
	IsOfficial bool    `yaml:"is_official"`
}

type Result struct {
	Now      time.Time
	Articles []article.Article
}

func Load(ctx context.Context, directory string) (Result, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return Result{}, fmt.Errorf("resolve fixture directory: %w", err)
	}
	manifestPath := filepath.Join(root, "manifest.yaml")
	file, err := os.Open(manifestPath)
	if err != nil {
		return Result{}, fmt.Errorf("open fixture manifest: %w", err)
	}
	dec := yaml.NewDecoder(file)
	dec.KnownFields(true)
	var spec manifest
	err = dec.Decode(&spec)
	closeErr := file.Close()
	if err != nil {
		return Result{}, fmt.Errorf("decode fixture manifest: %w", err)
	}
	if closeErr != nil {
		return Result{}, fmt.Errorf("close fixture manifest: %w", closeErr)
	}
	if spec.Now.IsZero() || len(spec.Feeds) == 0 {
		return Result{}, errors.New("fixture manifest requires now and at least one feed")
	}

	parser := feed.NewParser(50)
	result := Result{Now: spec.Now}
	for _, item := range spec.Feeds {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if strings.TrimSpace(item.File) == "" || filepath.IsAbs(item.File) {
			return Result{}, fmt.Errorf("fixture feed %q has an invalid file", item.Name)
		}
		path := filepath.Clean(filepath.Join(root, item.File))
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return Result{}, fmt.Errorf("fixture file escapes directory: %q", item.File)
		}
		f, err := os.Open(path)
		if err != nil {
			return Result{}, fmt.Errorf("open fixture feed %q: %w", item.Name, err)
		}
		source := article.Source{
			Name: item.Name, URL: item.URL, Weight: item.Weight, Language: item.Language,
			Category: item.Category, IsOfficial: item.IsOfficial,
		}
		articles, parseErr := parser.Parse(ctx, f, source)
		closeErr := f.Close()
		if parseErr != nil {
			return Result{}, fmt.Errorf("parse fixture feed %q: %w", item.Name, parseErr)
		}
		if closeErr != nil {
			return Result{}, fmt.Errorf("close fixture feed %q: %w", item.Name, closeErr)
		}
		result.Articles = append(result.Articles, articles...)
	}
	return result, nil
}
