package migrations

import "embed"

// FS contains all production database migrations.
//
//go:embed *.sql
var FS embed.FS
