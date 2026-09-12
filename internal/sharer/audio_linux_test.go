//go:build linux && cgo

package sharer

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// This opt-in check uses a private null sink; no test sound reaches speakers.
// SCREENSHARE_TEST_APP_AUDIO=1 go test ./internal/sharer -run TestAppAudioIsolation -v
func TestAppAudioIsolation(t *testing.T) {
	if os.Getenv("SCREENSHARE_TEST_APP_AUDIO") != "1" {
		t.Skip("requires a live PipeWire Pulse server, pactl, parec, and gst-launch-1.0")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	name := fmt.Sprintf("screenshare_test_%d", os.Getpid())
	module, err := exec.CommandContext(ctx, "pactl", "load-module", "module-null-sink", "sink_name="+name).Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := exec.CommandContext(cleanup, "pactl", "unload-module", strings.TrimSpace(string(module))).Run(); err != nil {
			t.Error(err)
		}
	})
	for _, frequency := range []int{440, 880} {
		cmd := exec.CommandContext(ctx, "gst-launch-1.0", "-q", "audiotestsrc", "is-live=true",
			fmt.Sprintf("freq=%d", frequency), "volume=0.5", "!", "audio/x-raw,rate=48000,channels=2", "!",
			"pulsesink", "device="+name, fmt.Sprintf("stream-properties=props,application.name=%s_%d", name, frequency))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	}
	var id string
	var otherID string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		apps, err := ListAudioApps(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, app := range apps {
			if strings.Contains(app.Name, name+"_440") {
				id = app.ID
			}
			if strings.Contains(app.Name, name+"_880") {
				otherID = app.ID
			}
		}
		if id != "" && otherID != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if id == "" || otherID == "" {
		t.Fatal("both test playback streams must appear")
	}
	if audio, err := captureAppAudio(ctx, id+"-stale"); err == nil {
		audio.close()
		t.Fatal("stale selection accepted")
	}
	// First prove the competing stream is audible, so silence cannot fake isolation.
	competitor, err := captureAppAudio(ctx, otherID)
	if err != nil {
		t.Fatal(err)
	}
	defer competitor.close()
	audio, err := captureAppAudio(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer audio.close()
	// Discard startup silence, then examine half a second of stereo PCM.
	if _, err := io.CopyN(io.Discard, audio.reader, 48000*4); err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 24000*4)
	if _, err := io.ReadFull(audio.reader, pcm); err != nil {
		t.Fatal(err)
	}
	amplitude := func(pcm []byte, frequency float64) float64 {
		var re, im float64
		for i := 0; i < len(pcm)/4; i++ {
			x := float64(int16(binary.LittleEndian.Uint16(pcm[i*4:]))) / 32768
			phase := 2 * math.Pi * frequency * float64(i) / 48000
			re += x * math.Cos(phase)
			im += x * math.Sin(phase)
		}
		return 2 * math.Hypot(re, im) / float64(len(pcm)/4)
	}
	selected, other := amplitude(pcm, 440), amplitude(pcm, 880)
	if selected < 0.1 || other > 0.01 {
		t.Fatalf("audio isolation failed: selected 440Hz=%f, other 880Hz=%f", selected, other)
	}
	t.Logf("selected 440Hz=%f; excluded 880Hz=%f", selected, other)
	if _, err := io.CopyN(io.Discard, competitor.reader, 48000*4); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(competitor.reader, pcm); err != nil {
		t.Fatal(err)
	}
	if amplitude(pcm, 880) < 0.1 {
		t.Fatal("competing stream was not producing its expected tone")
	}
	// The real fdsrc/PCM/Opus path must also become ready and stop cleanly.
	source := &Source{videoPipeline: "videotestsrc is-live=true ! videoconvert ! vp8enc deadline=1 ! rtpvp8pay pt=96 ! appsink name=video"}
	if err := source.UseAppAudio(ctx, id); err != nil {
		t.Fatal(err)
	}
	defer source.audio.close()
	capture, err := openCapture(ctx, source.Pipeline)
	if err != nil {
		t.Fatal(err)
	}
	capture.close()
}
