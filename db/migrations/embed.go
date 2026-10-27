// Package migrations embeds the ordered SQL migrations shipped with the command.
package migrations

import "embed"

// Files contains only released SQL migrations, not test fixtures.
//
//go:embed *.sql
var Files embed.FS
