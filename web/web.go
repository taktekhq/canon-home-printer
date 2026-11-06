// Package web holds the page, embedded into the binary.
package web

import "embed"

// Files is the page: index.html, app.js and style.css.
//
//go:embed index.html app.js style.css
var Files embed.FS
