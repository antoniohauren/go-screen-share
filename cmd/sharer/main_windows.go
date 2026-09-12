//go:build windows

package main

import (
	"io/fs"
	"log"

	"github.com/antoniohauren/go-screen-share/web"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

type App struct{}

func main() {
	assets, err := fs.Sub(web.Assets, "sharer")
	if err != nil {
		log.Fatal(err)
	}
	if err := wails.Run(&options.App{
		Title: "Screen Share", Width: 760, Height: 800,
		MinWidth: 480, MinHeight: 650,
		AssetServer: &assetserver.Options{Assets: assets},
		Bind:        []interface{}{&App{}},
	}); err != nil {
		log.Fatal(err)
	}
}
