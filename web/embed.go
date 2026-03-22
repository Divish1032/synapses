//go:build !dev

// Package web embeds the production build of the web console (Preact + Vite).
//
// Build the console first:
//
//	cd web/console && npm run build
//
// Then the Go binary embeds console/dist/ via //go:embed.
// The daemon serves these files at the root path ("/").
package web

import "embed"

//go:embed all:console/dist
var ConsoleFS embed.FS
