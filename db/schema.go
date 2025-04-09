package db

import "embed"

//go:embed schema/*.sql
var FS embed.FS
