//go:build linux && cgo

package sharer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// AudioApp identifies one playback stream, not every stream owned by a process.
type AudioApp struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type audioInput struct {
	Index      uint32            `json:"index"`
	Sink       uint32            `json:"sink"`
	Properties map[string]string `json:"properties"`
}

func (a audioInput) id() string {
	return fmt.Sprintf("%d:%s", a.Index, a.Properties["object.serial"])
}

func pulseList(ctx context.Context, kind string, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "pactl", "--format=json", "list", kind).Output()
	if err != nil {
		return fmt.Errorf("list app audio: %w (requires pactl and a running PipeWire Pulse server)", err)
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decode app audio: %w", err)
	}
	return nil
}

func ListAudioApps(ctx context.Context) ([]AudioApp, error) {
	var inputs []audioInput
	if err := pulseList(ctx, "sink-inputs", &inputs); err != nil {
		return nil, err
	}
	apps := make([]AudioApp, 0, len(inputs))
	for _, input := range inputs {
		// PipeWire serials prevent a stale selection from following a recycled node ID.
		if _, err := strconv.ParseUint(input.Properties["object.serial"], 10, 64); err != nil {
			continue
		}
		name := input.Properties["application.name"]
		if name == "" {
			name = input.Properties["application.process.binary"]
		}
		if name == "" {
			name = "Playback"
		}
		apps = append(apps, AudioApp{input.id(), fmt.Sprintf("%s — %s (#%d)", name, input.Properties["media.name"], input.Index)})
	}
	return apps, nil
}

type appAudio struct {
	reader *os.File
	cancel context.CancelFunc
	done   chan struct{}
}

func (a *appAudio) close() {
	a.cancel()
	_ = a.reader.Close()
	<-a.done
}

// captureAppAudio uses PulseAudio's single-sink-input monitor. It never records
// an unrestricted sink monitor, moves playback, or falls back to another source.
func captureAppAudio(ctx context.Context, id string) (*appAudio, error) {
	var inputs []audioInput
	if err := pulseList(ctx, "sink-inputs", &inputs); err != nil {
		return nil, err
	}
	var selected *audioInput
	for i := range inputs {
		if inputs[i].Properties["object.serial"] != "" && inputs[i].id() == id {
			selected = &inputs[i]
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("selected app audio ended; play audio, refresh the list, and select it again")
	}
	var sinks []struct {
		Index         uint32 `json:"index"`
		MonitorSource string `json:"monitor_source"`
	}
	if err := pulseList(ctx, "sinks", &sinks); err != nil {
		return nil, err
	}
	monitor := ""
	for _, sink := range sinks {
		if sink.Index == selected.Sink {
			monitor = sink.MonitorSource
		}
	}
	if monitor == "" {
		return nil, fmt.Errorf("selected app audio output disappeared; select it again")
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	run, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(run, "parec", "--raw", "--format=s16le", "--rate=48000", "--channels=2",
		"--latency-msec=20", "--client-name=Screen Share app audio", "--device="+monitor,
		fmt.Sprintf("--monitor-stream=%d", selected.Index),
		"--property=node.dont-reconnect=true", "--property=node.dont-fallback=true", "--property=node.dont-move=true")
	cmd.Stdout = writer
	if err := cmd.Start(); err != nil {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		return nil, fmt.Errorf("start app audio (requires parec): %w", err)
	}
	_ = writer.Close()
	a := &appAudio{reader: reader, cancel: cancel, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(a.done)
	}()
	return a, nil
}

const audioEncoding = "queue leaky=downstream max-size-buffers=8 max-size-bytes=0 max-size-time=0 ! " +
	"audioconvert ! audioresample ! audio/x-raw,rate=48000,channels=2 ! " +
	"opusenc bitrate=128000 frame-size=20 ! rtpopuspay pt=111 perfect-rtptime=false ! " +
	"appsink name=audio sync=false max-buffers=128 drop=true"

// UseAppAudio replaces any system-audio pipeline before Session.Start.
// Call once, and stop the Session before closing the Source.
func (s *Source) UseAppAudio(ctx context.Context, id string) error {
	if s.audio != nil {
		return fmt.Errorf("app audio already selected")
	}
	audio, err := captureAppAudio(ctx, id)
	if err != nil {
		return err
	}
	s.audio = audio
	s.Pipeline = s.videoPipeline + fmt.Sprintf(" fdsrc fd=%d do-timestamp=true ! "+
		"rawaudioparse pcm-format=s16le sample-rate=48000 num-channels=2 ! ", audio.reader.Fd()) + audioEncoding
	return nil
}
