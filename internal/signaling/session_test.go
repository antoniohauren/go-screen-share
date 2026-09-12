package signaling_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/antoniohauren/go-screen-share/internal/signaling"
	"github.com/coder/websocket"
)

type message map[string]any

func connect(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(server.URL, "https")+"/session", &websocket.DialOptions{HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func send(t *testing.T, conn *websocket.Conn, msg message) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
}

func receive(t *testing.T, conn *websocket.Conn, kind string) message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var msg message
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatal(err)
	}
	if msg["type"] != kind {
		t.Fatalf("want %s, got %s", kind, data)
	}
	return msg
}

func TestShareSessionStartExposesAccessAndSharingState(t *testing.T) {
	server := httptest.NewTLSServer(signaling.New("https://share.example"))
	defer server.Close()
	sharer := connect(t, server)
	send(t, sharer, message{"type": "start"})
	started := receive(t, sharer, "started")
	if started["state"] != "sharing" || started["viewers"] != float64(0) {
		t.Fatalf("unexpected state: %#v", started)
	}
	session, _ := started["session"].(string)
	code, _ := started["code"].(string)
	if len(session) < 22 || len(code) < 22 || started["url"] != "https://share.example/?session="+session {
		t.Fatalf("missing opaque access details: %#v", started)
	}
}

func TestShareSessionJoinRequiresCodeAndEnforcesOneViewer(t *testing.T) {
	server := httptest.NewTLSServer(signaling.New("https://share.example"))
	defer server.Close()
	sharer := connect(t, server)
	send(t, sharer, message{"type": "start"})
	started := receive(t, sharer, "started")
	viewer := connect(t, server)
	send(t, viewer, message{"type": "join", "session": started["session"], "code": "wrong"})
	if got := receive(t, viewer, "error"); got["error"] != "invalid-code" {
		t.Fatalf("%#v", got)
	}
	join := message{"type": "join", "session": started["session"], "code": started["code"]}
	send(t, viewer, join)
	joined := receive(t, viewer, "joined")
	notice := receive(t, sharer, "viewer-joined")
	if joined["peer"] == "" || joined["peer"] != notice["peer"] || notice["viewers"] != float64(1) {
		t.Fatalf("%#v %#v", joined, notice)
	}
	second := connect(t, server)
	send(t, second, join)
	if got := receive(t, second, "error"); got["error"] != "session-full" {
		t.Fatalf("%#v", got)
	}
	viewer.CloseNow()
	if got := receive(t, sharer, "viewer-left"); got["viewers"] != float64(0) {
		t.Fatalf("%#v", got)
	}
	send(t, second, join)
	rejoined := receive(t, second, "joined")
	if rejoined["peer"] == joined["peer"] {
		t.Fatal("reconnect reused old peer identity")
	}
	receive(t, sharer, "viewer-joined")
}

func TestShareSessionRelaysOnlyCurrentPeerSetup(t *testing.T) {
	server := httptest.NewTLSServer(signaling.New("https://share.example"))
	defer server.Close()
	sharer, viewer := connect(t, server), connect(t, server)
	send(t, sharer, message{"type": "start"})
	started := receive(t, sharer, "started")
	send(t, viewer, message{"type": "join", "session": started["session"], "code": started["code"]})
	peer := receive(t, viewer, "joined")["peer"]
	receive(t, sharer, "viewer-joined")
	offer := message{"type": "offer", "peer": peer, "sdp": message{"type": "offer", "sdp": "v=0\r\n"}}
	send(t, sharer, offer)
	if got := receive(t, viewer, "offer"); got["peer"] != peer || got["sdp"].(map[string]any)["sdp"] != "v=0\r\n" {
		t.Fatalf("%#v", got)
	}
	send(t, viewer, message{"type": "answer", "peer": peer, "sdp": message{"type": "answer", "sdp": "v=0\r\n"}})
	receive(t, sharer, "answer")
	for _, pair := range [][2]*websocket.Conn{{sharer, viewer}, {viewer, sharer}} {
		send(t, pair[0], message{"type": "ice", "peer": peer, "candidate": message{"candidate": "candidate:1 1 UDP 1 192.0.2.1 5000 typ host", "sdpMid": "0"}})
		receive(t, pair[1], "ice")
	}
	for _, invalid := range []message{
		{"type": "answer", "peer": peer, "sdp": message{"type": "answer", "sdp": "v=0\r\n"}},
		{"type": "offer", "peer": peer, "sdp": message{"type": "answer", "sdp": "v=0\r\n"}},
		{"type": "ice", "peer": peer, "candidate": "not an ICE object"},
		{"type": "media", "peer": peer},
	} {
		send(t, sharer, invalid)
		if got := receive(t, sharer, "error"); got["error"] != "invalid-message" {
			t.Fatalf("%#v", got)
		}
	}
}

func TestShareSessionStaleSignalingDoesNotInterruptReconnect(t *testing.T) {
	server := httptest.NewTLSServer(signaling.New("https://share.example"))
	defer server.Close()
	sharer, viewer := connect(t, server), connect(t, server)
	send(t, sharer, message{"type": "start"})
	started := receive(t, sharer, "started")
	join := message{"type": "join", "session": started["session"], "code": started["code"]}
	send(t, viewer, join)
	peer := receive(t, viewer, "joined")["peer"]
	receive(t, sharer, "viewer-joined")
	viewer.CloseNow()
	receive(t, sharer, "viewer-left")
	send(t, sharer, message{"type": "ice", "peer": peer, "candidate": message{"candidate": ""}})
	replacement := connect(t, server)
	send(t, replacement, join)
	receive(t, replacement, "joined")
	receive(t, sharer, "viewer-joined")
	send(t, sharer, message{"type": "stop"})
	receive(t, sharer, "stopped")
}

func TestShareSessionStopInvalidatesAccessAndDisconnectsViewer(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "sharer-disconnect"}[disconnect], func(t *testing.T) {
			server := httptest.NewTLSServer(signaling.New("https://share.example"))
			defer server.Close()
			sharer, viewer := connect(t, server), connect(t, server)
			send(t, sharer, message{"type": "start"})
			started := receive(t, sharer, "started")
			join := message{"type": "join", "session": started["session"], "code": started["code"]}
			send(t, viewer, join)
			receive(t, viewer, "joined")
			receive(t, sharer, "viewer-joined")
			send(t, viewer, message{"type": "stop"})
			if got := receive(t, viewer, "error"); got["error"] != "not-sharer" {
				t.Fatalf("%#v", got)
			}
			if disconnect {
				sharer.CloseNow()
			} else {
				send(t, sharer, message{"type": "stop"})
				if got := receive(t, sharer, "stopped"); got["state"] != "stopped" || got["viewers"] != float64(0) {
					t.Fatalf("%#v", got)
				}
			}
			receive(t, viewer, "stopped")
			send(t, viewer, join)
			if got := receive(t, viewer, "error"); got["error"] != "invalid-code" {
				t.Fatalf("%#v", got)
			}
			if !disconnect {
				send(t, sharer, message{"type": "start"})
				restarted := receive(t, sharer, "started")
				if restarted["code"] == started["code"] || restarted["session"] == started["session"] {
					t.Fatal("restart reused invalidated access")
				}
			}
		})
	}
}

func TestShareSessionRejectsInsecureTransport(t *testing.T) {
	server := httptest.NewServer(signaling.New("https://share.example"))
	defer server.Close()
	response, err := http.Get(server.URL + "/session")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("HTTP status %d", response.StatusCode)
	}
}

func TestShareSessionServiceShutdownEndsConnections(t *testing.T) {
	service := signaling.New("https://share.example")
	server := httptest.NewTLSServer(service)
	defer server.Close()
	sharer := connect(t, server)
	send(t, sharer, message{"type": "start"})
	receive(t, sharer, "started")
	service.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := sharer.Read(ctx)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("connection should close immediately: %v", err)
	}
}

func TestShareSessionCanDisconnectFailedPeerWithoutStoppingCaptureSession(t *testing.T) {
	server := httptest.NewTLSServer(signaling.New("https://share.example"))
	defer server.Close()
	sharer, viewer := connect(t, server), connect(t, server)
	send(t, sharer, message{"type": "start"})
	started := receive(t, sharer, "started")
	join := message{"type": "join", "session": started["session"], "code": started["code"]}
	send(t, viewer, join)
	peer := receive(t, viewer, "joined")["peer"]
	receive(t, sharer, "viewer-joined")
	send(t, viewer, message{"type": "disconnect-peer", "peer": peer})
	if got := receive(t, viewer, "error"); got["error"] != "not-sharer" {
		t.Fatalf("%#v", got)
	}
	send(t, sharer, message{"type": "disconnect-peer", "peer": peer})
	if got := receive(t, viewer, "error"); got["error"] != "connection-failed" {
		t.Fatalf("%#v", got)
	}
	if got := receive(t, sharer, "viewer-left"); got["state"] != "sharing" || got["viewers"] != float64(0) {
		t.Fatalf("%#v", got)
	}
	send(t, viewer, join)
	receive(t, viewer, "joined")
	receive(t, sharer, "viewer-joined")
}

func TestShareSessionUnresponsiveSharerLosesAccess(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("exercises the real 30-second network failure bound")
	}
	server := httptest.NewTLSServer(signaling.New("https://share.example"))
	defer server.Close()
	sharer := connect(t, server)
	send(t, sharer, message{"type": "start"})
	started := receive(t, sharer, "started")
	// Stop reading, so this real WebSocket client no longer answers server pings.
	time.Sleep(32 * time.Second)
	viewer := connect(t, server)
	send(t, viewer, message{"type": "join", "session": started["session"], "code": started["code"]})
	if got := receive(t, viewer, "error"); got["error"] != "invalid-code" {
		t.Fatalf("%#v", got)
	}
}

func TestShareSessionSendsClientVisibleHeartbeat(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("exercises the real 20-second heartbeat interval")
	}
	server := httptest.NewTLSServer(signaling.New("https://share.example"))
	defer server.Close()
	sharer := connect(t, server)
	send(t, sharer, message{"type": "start"})
	receive(t, sharer, "started")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	_, data, err := sharer.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var msg message
	if err := json.Unmarshal(data, &msg); err != nil || msg["type"] != "heartbeat" {
		t.Fatalf("expected client-visible heartbeat, got %s (%v)", data, err)
	}
}
