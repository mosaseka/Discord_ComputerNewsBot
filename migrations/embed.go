// Package migrations exposes the immutable SQL migrations embedded in the bot binary.
package migrations

import "embed"

// FS contains every numbered migration in this directory.
//
//go:embed *.sql
var FS embed.FS
