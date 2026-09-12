//go:build linux && cgo

package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"sync"
	"time"

	"github.com/antoniohauren/go-screen-share/internal/sharer"
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

func (a *App) AudioApps() ([]sharer.AudioApp, error) {
	return sharer.ListAudioApps(a.ctx)
}

// Start returns immediately so Stop can cancel the native picker as well as media.
func (a *App) Start(audioID string) error {
	origin, err := a.SignalingURL()
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil {
		return fmt.Errorf("sharing is already active")
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.cancel, a.done = cancel, make(chan struct{})
	go a.share(ctx, origin, audioID)
	return nil
}

func (a *App) Stop() {
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (a *App) share(ctx context.Context, origin, audioID string) {
	var failure error
	emit := func(state sharer.State) { runtime.EventsEmit(a.ctx, "share-state", state) }
	defer func() {
		state := sharer.State{State: "stopped"}
		if failure != nil && !errors.Is(failure, context.Canceled) {
			state.Error = failure.Error()
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.cancel()
		emit(state)
		a.cancel = nil
		close(a.done)
	}()
	emit(sharer.State{State: "choosing"})
	source, err := sharer.SelectSource(ctx)
	if err != nil {
		failure = err
		return
	}
	defer source.Close()
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-source.Done():
			cancel()
		case <-run.Done():
		}
	}()
	if audioID != "" {
		if err := source.UseAppAudio(run, audioID); err != nil {
			failure = err
			return
		}
	}
	emit(sharer.State{State: "connecting"})
	session, err := sharer.Start(run, origin, source.Pipeline, nil)
	if err != nil {
		failure = err
		return
	}
	defer session.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var previous sharer.State
	for {
		if run.Err() != nil {
			return
		}
		state := session.State()
		if state.State == "stopped" {
			if state.Error != "" {
				failure = errors.New(state.Error)
			}
			return
		}
		if state != previous {
			emit(state)
			previous = state
		}
		select {
		case <-run.Done():
			return
		case <-ticker.C:
		}
	}
}

func main() {
	assets, err := fs.Sub(web.Assets, "sharer")
	if err != nil {
		log.Fatal(err)
	}
	app := &App{}
	if err := wails.Run(&options.App{
		Title: "Screen Share", Width: 760, Height: 800, MinWidth: 480, MinHeight: 650,
		AssetServer: &assetserver.Options{Assets: assets}, Bind: []interface{}{app},
		OnStartup:     func(ctx context.Context) { app.ctx = ctx },
		OnBeforeClose: func(context.Context) bool { app.Stop(); return false },
		OnShutdown:    func(context.Context) { app.Stop() },
	}); err != nil {
		log.Fatal(err)
	}
}
