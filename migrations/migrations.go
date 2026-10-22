// Package migrations embeds the ordered PostgreSQL schema source.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS
