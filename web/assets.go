// Package web contains separate desktop and public receiver assets.
package web

import "embed"

// Assets contains sharer and viewer roots; serve each with fs.Sub.
//
//go:embed sharer viewer
var Assets embed.FS
