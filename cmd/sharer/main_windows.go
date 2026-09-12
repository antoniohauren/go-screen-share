//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/fs"
	"log"
	"sync"

	"github.com/antoniohauren/go-screen-share/internal/winaudio"
	"github.com/antoniohauren/go-screen-share/web"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

type App struct {
	ctx    context.Context
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func (a *App) AudioApps() ([]winaudio.App, error) { return winaudio.ListApps() }

func (a *App) StartAudio(id, token string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		return fmt.Errorf("audio capture already running")
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.cancel, a.done = cancel, make(chan struct{})
	ready := make(chan error, 1)
	go func() {
		defer close(a.done)
		started := false
		err := winaudio.Capture(ctx, id, func() { started = true; ready <- nil }, func(pcm []byte) {
			runtime.EventsEmit(a.ctx, "app-audio", map[string]string{"token": token, "pcm": base64.StdEncoding.EncodeToString(pcm)})
		})
		if !started {
			ready <- err
		} else if err != nil && ctx.Err() == nil {
			runtime.EventsEmit(a.ctx, "app-audio-error", map[string]string{"token": token, "error": err.Error()})
		}
	}()
	if err := <-ready; err != nil {
		cancel()
		<-a.done
		a.cancel = nil
		return err
	}
	return nil
}

func (a *App) StopAudio() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		a.cancel()
		<-a.done
		a.cancel = nil
	}
}

func main() {
	assets, err := fs.Sub(web.Assets, "sharer")
	if err != nil {
		log.Fatal(err)
	}
	app := &App{}
	if err := wails.Run(&options.App{
		Title: "Screen Share", Width: 760, Height: 800,
		MinWidth: 480, MinHeight: 650,
		AssetServer: &assetserver.Options{Assets: assets},
		Bind:        []interface{}{app},
		OnStartup:   func(ctx context.Context) { app.ctx = ctx },
		OnShutdown:  func(context.Context) { app.StopAudio() },
	}); err != nil {
		log.Fatal(err)
	}
}
