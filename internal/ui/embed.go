package ui

import "embed"

// Files contains the complete browser application served by the Go binary.
//
//go:embed dist/*
var Files embed.FS
