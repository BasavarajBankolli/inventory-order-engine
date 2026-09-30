// Package migrations embeds the SQL migration files into the Go binary.
//
// Thanks to //go:embed the compiled program carries its own migrations, so
// the Docker image does not need a separate copy of this folder and the SQL
// can never get out of sync with the code that was built with it.
package migrations

import "embed"

// FS contains every *.sql file in this directory.
//
//go:embed *.sql
var FS embed.FS
