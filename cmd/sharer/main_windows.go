//go:build windows

package main

import (
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"

	"github.com/antoniohauren/go-screen-share/web"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

type App struct{}

// SignalingURL returns only a configured HTTPS origin, never an insecure default.
func (*App) SignalingURL() (string, error) {
	u, err := url.Parse(os.Getenv("SCREENSHARE_URL"))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("set SCREENSHARE_URL to an HTTPS origin, for example https://share.example.com")
	}
	return "https://" + u.Host, nil
}

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
