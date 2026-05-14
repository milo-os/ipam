package migrations

import "embed"

// FS holds all numbered SQL migration files for use with goose.
//
//go:embed *.sql
var FS embed.FS
