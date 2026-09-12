//go:build linux && cgo

package sharer_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antoniohauren/go-screen-share/internal/sharer"
	"github.com/antoniohauren/go-screen-share/internal/signaling"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

func TestShareSessionCaptureFailureExposesNoAccess(t *testing.T) {
	session, err := sharer.Start(context.Background(), "https://share.example", "missing_capture_plugin ! appsink name=video", nil)
	if err == nil || session != nil {
		t.Fatalf("capture failure exposed a session: %v, %v", session, err)
	}
}

func TestShareSessionNativeViewerReceivesVideo(t *testing.T) {
	server := newSignalingServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	session, err := sharer.Start(ctx, server.URL, videoPipeline, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop()
	connectNativeViewer(t, ctx, server, session.State(), false, nil)
	if session.State().Viewers != 1 {
		t.Fatalf("viewer count: %+v", session.State())
	}
}

type senderReport struct {
	codec  string
	report rtcp.SenderReport
}

func connectNativeViewer(t *testing.T, ctx context.Context, server *httptest.Server, state sharer.State, audio bool, reports chan<- senderReport) (*websocket.Conn, *webrtc.PeerConnection) {
	t.Helper()
	viewer, _, err := websocket.Dial(ctx, "wss"+server.URL[len("https"):]+"/session", &websocket.DialOptions{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { viewer.CloseNow() })
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	media := make(chan string, 2)
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		packet, _, err := track.ReadRTP()
		if err == nil && len(packet.Payload) > 0 {
			media <- track.Codec().MimeType
		}
		for reports != nil {
			packets, _, err := receiver.ReadRTCP()
			if err != nil {
				return
			}
			for _, packet := range packets {
				if report, ok := packet.(*rtcp.SenderReport); ok {
					select {
					case reports <- senderReport{track.Codec().MimeType, *report}:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	})
	if err := wsjson.Write(ctx, viewer, map[string]any{"type": "join", "session": state.Session, "code": state.Code}); err != nil {
		t.Fatal(err)
	}
	for {
		var msg struct {
			Type      string                    `json:"type"`
			Peer      string                    `json:"peer"`
			SDP       webrtc.SessionDescription `json:"sdp"`
			Candidate webrtc.ICECandidateInit   `json:"candidate"`
		}
		if err := wsjson.Read(ctx, viewer, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == "offer" {
			if err := pc.SetRemoteDescription(msg.SDP); err != nil {
				t.Fatal(err)
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				t.Fatal(err)
			}
			gathered := webrtc.GatheringCompletePromise(pc)
			if err := pc.SetLocalDescription(answer); err != nil {
				t.Fatal(err)
			}
			select {
			case <-gathered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := wsjson.Write(ctx, viewer, map[string]any{"type": "answer", "peer": msg.Peer, "sdp": pc.LocalDescription()}); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	go func() {
		for {
			var msg struct {
				Type      string                  `json:"type"`
				Candidate webrtc.ICECandidateInit `json:"candidate"`
			}
			if wsjson.Read(ctx, viewer, &msg) != nil {
				return
			}
			if msg.Type == "ice" {
				_ = pc.AddICECandidate(msg.Candidate)
			}
		}
	}()
	want := map[string]bool{webrtc.MimeTypeVP8: true}
	if audio {
		want[webrtc.MimeTypeOpus] = true
	}
	if strings.Contains(pc.RemoteDescription().SDP, "m=audio ") != audio {
		t.Fatal("wrong audio scope in offer")
	}
	for len(want) > 0 {
		select {
		case codec := <-media:
			if !want[codec] {
				t.Fatalf("unexpected codec: %s", codec)
			}
			delete(want, codec)
		case <-ctx.Done():
			t.Fatal("viewer never received expected media")
		}
	}
	return viewer, pc
}

func TestShareSessionNativeFourViewersReceiveAudioAndVideo(t *testing.T) {
	server := newSignalingServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pipeline := videoPipeline + " audiotestsrc is-live=true ! audioconvert ! audioresample ! audio/x-raw,rate=48000,channels=2 ! opusenc ! rtpopuspay pt=111 perfect-rtptime=false ! appsink name=audio sync=false max-buffers=8 drop=true"
	session, err := sharer.Start(ctx, server.URL, pipeline, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop()
	var first *websocket.Conn
	for range 4 {
		viewer, _ := connectNativeViewer(t, ctx, server, session.State(), true, nil)
		if first == nil {
			first = viewer
		}
	}
	if session.State().Viewers != 4 {
		t.Fatalf("viewer count: %+v", session.State())
	}
	first.CloseNow()
	for session.State().Viewers != 3 {
		select {
		case <-ctx.Done():
			t.Fatal("viewer leave did not update count")
		case <-time.After(10 * time.Millisecond):
		}
	}
	connectNativeViewer(t, ctx, server, session.State(), true, nil)
	if session.State().Viewers != 4 {
		t.Fatalf("reconnect count: %+v", session.State())
	}
}

func TestShareSessionNativeTransportFailureKeepsSessionAndOtherViewers(t *testing.T) {
	server := newSignalingServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	session, err := sharer.Start(ctx, server.URL, videoPipeline, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop()
	access := session.State()
	_, failed := connectNativeViewer(t, ctx, server, access, false, nil)
	connectNativeViewer(t, ctx, server, access, false, nil)
	failed.Close() // Keep signaling open: only this viewer's media transport failed.
	for session.State().Viewers != 1 {
		if session.State().State == "stopped" {
			t.Fatalf("one viewer stopped everyone: %+v", session.State())
		}
		select {
		case <-ctx.Done():
			t.Fatal("failed media peer was not removed")
		case <-time.After(10 * time.Millisecond):
		}
	}
	connectNativeViewer(t, ctx, server, access, false, nil)
	if got := session.State(); got.Viewers != 2 || got.Code != access.Code {
		t.Fatalf("failed peer invalidated session: %+v", got)
	}
}

func TestShareSessionPreservesAudioVideoClockDespiteDeliveryDelay(t *testing.T) {
	server := newSignalingServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Both test payloaders start at timestamp zero on the same pipeline clock.
	// Delay only audio delivery; that must not become an A/V presentation offset.
	pipeline := strings.Replace(videoPipeline, "rtpvp8pay pt=96", "rtpvp8pay pt=96 timestamp-offset=0", 1) +
		" audiotestsrc is-live=true ! audio/x-raw,rate=48000,channels=2 ! opusenc ! rtpopuspay pt=111 timestamp-offset=0 perfect-rtptime=false ! queue min-threshold-time=400000000 ! appsink name=audio sync=false max-buffers=8 drop=true"
	session, err := sharer.Start(ctx, server.URL, pipeline, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop()
	reports := make(chan senderReport, 8)
	connectNativeViewer(t, ctx, server, session.State(), true, reports)
	got := make(map[string]rtcp.SenderReport)
	for len(got) < 2 {
		select {
		case report := <-reports:
			got[report.codec] = report.report
		case <-ctx.Done():
			t.Fatal("missing sender reports")
		}
	}
	video, audio := got[webrtc.MimeTypeVP8], got[webrtc.MimeTypeOpus]
	ntpDelta := float64(int64(video.NTPTime-audio.NTPTime)) / (1 << 32)
	rtpDelta := float64(video.RTPTime)/90000 - float64(audio.RTPTime)/48000
	if offset := rtpDelta - ntpDelta; offset < -0.075 || offset > 0.075 {
		t.Fatalf("delivery delay changed A/V clock mapping by %.3fs", offset)
	}
}

const videoPipeline = "videotestsrc is-live=true ! video/x-raw,width=320,height=180,framerate=30/1 ! videoconvert ! vp8enc deadline=1 keyframe-max-dist=30 ! rtpvp8pay pt=96 perfect-rtptime=false ! appsink name=video sync=false max-buffers=8 drop=true"

func TestShareSessionNativeStartStopInvalidatesAccess(t *testing.T) {
	server := newSignalingServer(t)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := sharer.Start(ctx, server.URL, videoPipeline, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop()
	state := session.State()
	if state.State != "sharing" || state.Code == "" || state.URL == "" || state.Viewers != 0 {
		t.Fatalf("missing sharing state: %+v", state)
	}
	session.Stop()
	if stopped := session.State(); stopped.State != "stopped" || stopped.Code != "" || stopped.URL != "" {
		t.Fatalf("stale access after stop: %+v", stopped)
	}
	viewer, _, err := websocket.Dial(ctx, "wss"+server.URL[len("https"):]+"/session", &websocket.DialOptions{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.CloseNow()
	if err := wsjson.Write(ctx, viewer, map[string]any{"type": "join", "session": state.Session, "code": state.Code}); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := wsjson.Read(ctx, viewer, &result); err != nil {
		t.Fatal(err)
	}
	if result["error"] != "invalid-code" {
		t.Fatalf("stopped session still accessible: %v", result)
	}
}

func newSignalingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(nil)
	server.Config.Handler = signaling.New(server.URL)
	t.Cleanup(server.Close)
	return server
}

func TestShareSessionRejectsForeignAccessURL(t *testing.T) {
	server := httptest.NewTLSServer(signaling.New("https://wrong.example"))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := sharer.Start(ctx, server.URL, videoPipeline, server.Client())
	if session != nil {
		session.Stop()
	}
	if err == nil || session != nil {
		t.Fatalf("accepted foreign access URL: %v", err)
	}
}

func TestShareSessionRequiresLiveAudioBeforeExposingAccess(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	pipeline := videoPipeline + " audiotestsrc is-live=true ! valve drop=true ! opusenc ! rtpopuspay ! appsink name=audio sync=false async=false"
	session, err := sharer.Start(ctx, server.URL, pipeline, server.Client())
	if session != nil {
		session.Stop()
	}
	if err == nil || session != nil || requests.Load() != 0 {
		t.Fatalf("started without live audio: %v, requests=%d", err, requests.Load())
	}
}

func TestShareSessionCancelledStartExposesNoAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session, err := sharer.Start(ctx, "https://share.example", videoPipeline, nil)
	if !errors.Is(err, context.Canceled) || session != nil {
		t.Fatalf("cancelled start: %v, %v", session, err)
	}
}

func TestShareSessionCaptureLossClearsAccess(t *testing.T) {
	server := newSignalingServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := sharer.Start(ctx, server.URL, strings.Replace(videoPipeline, "is-live=true", "is-live=true num-buffers=15", 1), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Stop()
	for session.State().State != "stopped" {
		select {
		case <-ctx.Done():
			t.Fatal("capture loss did not stop sharing")
		case <-time.After(10 * time.Millisecond):
		}
	}
	state := session.State()
	if state.Code != "" || state.URL != "" || !strings.Contains(state.Error, "capture:") {
		t.Fatalf("capture loss left stale access: %+v", state)
	}
}
