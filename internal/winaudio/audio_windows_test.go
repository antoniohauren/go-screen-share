//go:build windows

package winaudio

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestRejectUnavailableAudioApp(t *testing.T) {
	h, created, err := process(uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	windows.CloseHandle(h)
	for _, id := range []string{"", "0:0", "bad:1", "1:bad", fmt.Sprintf("%d:%d", os.Getpid(), created+1)} {
		err := Capture(context.Background(), id, func() { t.Error("unavailable app started capture") }, func([]byte) { t.Error("unavailable app emitted audio") })
		if err == nil {
			t.Errorf("accepted invalid or stale audio app %q", id)
		}
	}
}

// This opt-in test plays two audible tones on the default Windows output.
func TestProcessAudioIsolation(t *testing.T) {
	if os.Getenv("SCREENSHARE_TEST_APP_AUDIO") != "1" {
		t.Skip("requires Windows 11 and an audio output; opt in with SCREENSHARE_TEST_APP_AUDIO=1")
	}
	startTone := func(hz int) string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestAudioPlaybackHelper$")
		cmd.Env = append(os.Environ(), "SCREENSHARE_TEST_TONE="+strconv.Itoa(hz))
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		ready := make(chan bool, 1)
		go func() { scanner := bufio.NewScanner(out); ready <- scanner.Scan() && scanner.Text() == "ready" }()
		select {
		case ok := <-ready:
			if !ok {
				t.Fatal("tone helper failed")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("tone helper timed out")
		}
		h, created, err := process(uint32(cmd.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		windows.CloseHandle(h)
		return fmt.Sprintf("%d:%d", cmd.Process.Pid, created)
	}
	first, second := startTone(440), startTone(880)
	for _, target := range []struct {
		id               string
		wanted, excluded float64
	}{{first, 440, 880}, {second, 880, 440}} {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		var samples []float64
		started := false
		err := Capture(ctx, target.id, func() { started = true }, func(pcm []byte) {
			for i := 0; i < len(pcm); i += 4 {
				samples = append(samples, float64(int16(binary.LittleEndian.Uint16(pcm[i:])))/32768)
			}
			if len(samples) >= 96000 {
				cancel()
			}
		})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !started || len(samples) < 96000 {
			t.Fatalf("capture not ready or insufficient PCM: %d frames", len(samples))
		}
		// Skip activation warmup, then measure one full second of both frequencies.
		samples = samples[len(samples)-48000:]
		amplitude := func(hz float64) float64 {
			var real, imag float64
			for i, value := range samples {
				phase := 2 * math.Pi * hz * float64(i) / 48000
				real += value * math.Cos(phase)
				imag += value * math.Sin(phase)
			}
			return 2 * math.Hypot(real, imag) / float64(len(samples))
		}
		wanted, excluded := amplitude(target.wanted), amplitude(target.excluded)
		if wanted < 0.005 || excluded > wanted*0.1 {
			t.Fatalf("process isolation failed: wanted=%f excluded=%f", wanted, excluded)
		}
	}
}

func TestAudioPlaybackHelper(t *testing.T) {
	hz, _ := strconv.Atoi(os.Getenv("SCREENSHARE_TEST_TONE"))
	if hz == 0 {
		t.Skip("subprocess helper")
	}
	var wav bytes.Buffer
	wav.WriteString("RIFF")
	binary.Write(&wav, binary.LittleEndian, uint32(36+48000*2))
	wav.WriteString("WAVEfmt ")
	binary.Write(&wav, binary.LittleEndian, uint32(16))
	for _, value := range []uint16{1, 1} {
		binary.Write(&wav, binary.LittleEndian, value)
	}
	for _, value := range []uint32{48000, 96000} {
		binary.Write(&wav, binary.LittleEndian, value)
	}
	for _, value := range []uint16{2, 16} {
		binary.Write(&wav, binary.LittleEndian, value)
	}
	wav.WriteString("data")
	binary.Write(&wav, binary.LittleEndian, uint32(48000*2))
	for i := 0; i < 48000; i++ {
		binary.Write(&wav, binary.LittleEndian, int16(4000*math.Sin(2*math.Pi*float64(hz*i)/48000)))
	}
	data := wav.Bytes()
	play := windows.NewLazySystemDLL("winmm.dll").NewProc("PlaySoundW")
	ok, _, err := play.Call(uintptr(unsafe.Pointer(&data[0])), 0, 0xf) // ASYNC | NODEFAULT | MEMORY | LOOP
	if ok == 0 {
		t.Fatal(err)
	}
	fmt.Println("ready")
	time.Sleep(30 * time.Second)
	play.Call(0, 0, 0)
	runtime.KeepAlive(data)
}
