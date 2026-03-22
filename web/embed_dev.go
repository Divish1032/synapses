//go:build dev

// Package web provides a live-reload filesystem for development.
// Build with: go build -tags dev
// The daemon reads files from disk instead of the embedded FS.
//
// daemon_serve.go does: fs.Sub(web.ConsoleFS, "console/dist")
// So ConsoleFS must contain the "console/dist" subtree. We achieve
// this by pointing at the "web/" directory on disk — which contains
// console/dist/ after `npm run build`.
package web

import (
	"io/fs"
	"os"
)

// ConsoleFS in dev mode reads from the "web/" directory on disk.
// Because daemon_serve.go calls fs.Sub(ConsoleFS, "console/dist"),
// the FS must contain that path — and os.DirFS("web") does, since
// web/console/dist/ exists on disk after building the console.
var ConsoleFS fs.FS = os.DirFS("web")
