//go:build linux && cgo

// Package sharer owns the native Linux ShareSession lifecycle.
package sharer

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type State struct {
	State   string `json:"state"`
	Session string `json:"session"`
	URL     string `json:"url"`
	Code    string `json:"code"`
	Viewers int    `json:"viewers"`
	Error   string `json:"error"`
	Notice  string `json:"notice"`
}

type Session struct {
	capture     *capture
	conn        *websocket.Conn
	mu          sync.Mutex
	state       State
	cancel      context.CancelFunc
	done        chan struct{}
	once        sync.Once
	mediaOnce   sync.Once
	mediaCancel context.CancelFunc
	mediaDone   chan struct{}
	bitrates    map[string]uint64
}

type message struct {
	Type      string                     `json:"type"`
	Peer      string                     `json:"peer,omitempty"`
	Viewers   int                        `json:"viewers"`
	SDP       *webrtc.SessionDescription `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit   `json:"candidate,omitempty"`
}

type peer struct {
	pc     *webrtc.PeerConnection
	timer  *time.Timer
	ice    []webrtc.ICECandidateInit
	cancel context.CancelFunc
}

func (p *peer) close() { p.cancel(); p.timer.Stop(); _ = p.pc.Close() }

// Start opens a native GStreamer RTP pipeline before exposing session access.
// The pipeline must provide an appsink named video, and optionally audio.
func Start(ctx context.Context, origin, pipeline string, client *http.Client) (*Session, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("signaling must be an HTTPS origin")
	}
	capture, err := openCapture(ctx, pipeline)
	if err != nil {
		return nil, err
	}
	u.Scheme, u.Path = "wss", "/session"
	setup, done := context.WithTimeout(ctx, 15*time.Second)
	defer done()
	conn, _, err := websocket.Dial(setup, u.String(), &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		capture.close()
		return nil, err
	}
	conn.SetReadLimit(64 << 10)
	var started struct {
		Type string `json:"type"`
		State
	}
	err = wsjson.Write(setup, conn, map[string]string{"type": "start"})
	if err == nil {
		err = wsjson.Read(setup, conn, &started)
	}
	if err == nil && (started.Type != "started" || started.Code == "" || started.Session == "") {
		err = fmt.Errorf("invalid signaling start response")
	}
	if err == nil {
		link, parseErr := url.Parse(started.URL)
		if parseErr != nil || link.Scheme != "https" || link.Host != u.Host || link.User != nil || started.State.State != "sharing" {
			err = fmt.Errorf("invalid signaling access URL or state")
		}
	}
	if err != nil {
		conn.CloseNow()
		capture.close()
		return nil, err
	}
	run, cancel := context.WithCancel(context.Background())
	media, mediaCancel := context.WithCancel(run)
	s := &Session{capture: capture, conn: conn, state: started.State, cancel: cancel, done: make(chan struct{}), mediaCancel: mediaCancel, mediaDone: make(chan struct{}), bitrates: make(map[string]uint64)}
	go func() {
		err := capture.pump(media)
		if media.Err() == nil && err != nil {
			s.mu.Lock()
			s.state.Error = err.Error()
			s.mu.Unlock()
			cancel()
		}
		close(s.mediaDone)
	}()
	go s.run(run)
	go func() {
		select {
		case <-ctx.Done():
			s.Stop()
		case <-s.done:
		}
	}()
	return s, nil
}

func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) stopMedia() {
	s.mediaOnce.Do(func() { s.mediaCancel(); <-s.mediaDone; s.capture.close() })
}

func (s *Session) Stop() {
	s.once.Do(func() {
		s.stopMedia()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = wsjson.Write(ctx, s.conn, map[string]string{"type": "stop"})
		select {
		case <-s.done:
		case <-ctx.Done():
			s.cancel()
			<-s.done
		}
	})
}

func (s *Session) run(ctx context.Context) {
	var failure string
	peers := make(map[string]*peer)
	defer func() {
		s.cancel()
		for _, peer := range peers {
			peer.close()
		}
		s.stopMedia()
		s.conn.CloseNow()
		s.mu.Lock()
		if s.state.Error != "" {
			failure = s.state.Error
		}
		s.state = State{State: "stopped", Error: failure}
		s.mu.Unlock()
		close(s.done)
	}()
	for {
		read, cancel := context.WithTimeout(ctx, 35*time.Second)
		var msg message
		err := wsjson.Read(read, s.conn, &msg)
		cancel()
		if err != nil {
			failure = "Signaling connection lost. Capture stopped."
			return
		}
		if msg.Type == "stopped" {
			return
		}
		switch msg.Type {
		case "heartbeat":
		case "viewer-joined":
			if msg.Peer == "" || peers[msg.Peer] != nil {
				continue
			}
			if len(peers) >= 4 {
				s.send(ctx, message{Type: "disconnect-peer", Peer: msg.Peer})
				continue
			}
			p, err := s.addPeer(ctx, msg.Peer)
			if err != nil {
				s.send(ctx, message{Type: "disconnect-peer", Peer: msg.Peer})
				continue
			}
			peers[msg.Peer] = p
			s.mu.Lock()
			s.state.Viewers = msg.Viewers
			s.mu.Unlock()
		case "viewer-left":
			if p := peers[msg.Peer]; p != nil {
				p.close()
				delete(peers, msg.Peer)
			}
			s.setBitrate(msg.Peer, 0)
			s.mu.Lock()
			s.state.Viewers = msg.Viewers
			s.mu.Unlock()
		case "answer", "ice", "error":
			p := peers[msg.Peer]
			if msg.Type == "error" && msg.Peer == "" {
				failure = "Signaling rejected the session."
				return
			}
			if p == nil {
				continue
			}
			var err error
			if msg.Type == "answer" && msg.SDP != nil && msg.SDP.Type == webrtc.SDPTypeAnswer && p.pc.SignalingState() == webrtc.SignalingStateHaveLocalOffer {
				err = p.pc.SetRemoteDescription(*msg.SDP)
				if err == nil {
					for _, candidate := range p.ice {
						if err = p.pc.AddICECandidate(candidate); err != nil {
							break
						}
					}
					p.ice = nil
				}
			} else if msg.Type == "ice" && msg.Candidate != nil {
				if p.pc.RemoteDescription() != nil {
					err = p.pc.AddICECandidate(*msg.Candidate)
				} else if len(p.ice) < 256 {
					p.ice = append(p.ice, *msg.Candidate)
				} else {
					err = fmt.Errorf("too many ICE candidates")
				}
			}
			if err != nil || msg.Type == "error" {
				p.close()
				delete(peers, msg.Peer)
				s.send(ctx, message{Type: "disconnect-peer", Peer: msg.Peer})
			}
		default:
			failure = "Unexpected signaling response."
			return
		}
	}
}

func (s *Session) send(ctx context.Context, msg message) error {
	write, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := wsjson.Write(write, s.conn, msg)
	if err != nil {
		s.cancel()
	}
	return err
}

func (s *Session) addPeer(ctx context.Context, id string) (*peer, error) {
	pc, err := s.capture.newPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}})
	if err != nil {
		return nil, err
	}
	signalCtx := ctx
	ctx, cancel := context.WithCancel(ctx)
	fail := func() {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		s.state.Notice = "A viewer's direct connection failed. Other viewers can continue; reconnect with the same code. Restrictive NAT or firewall may block P2P; no TURN relay is available."
		s.mu.Unlock()
		_ = s.send(signalCtx, message{Type: "disconnect-peer", Peer: id})
	}
	p := &peer{pc: pc, timer: time.AfterFunc(30*time.Second, fail), cancel: cancel}
	success := false
	defer func() {
		if !success {
			p.close()
		}
	}()
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			p.timer.Stop()
		}
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected {
			fail()
		}
	})
	for _, track := range s.capture.tracks {
		sender, err := pc.AddTrack(track)
		if err != nil {
			return nil, err
		}
		go func() {
			for {
				packets, _, err := sender.ReadRTCP()
				if err != nil {
					fail()
					return
				}
				for _, packet := range packets {
					if estimate, ok := packet.(*rtcp.ReceiverEstimatedMaximumBitrate); ok && estimate.Bitrate > 0 {
						s.setBitrate(id, uint64(min(estimate.Bitrate, 4000000)))
					}
				}
			}
		}()
	}
	offerReady := make(chan struct{})
	defer close(offerReady)
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}
		select {
		case <-offerReady:
		case <-ctx.Done():
			return
		}
		value := candidate.ToJSON()
		_ = s.send(signalCtx, message{Type: "ice", Peer: id, Candidate: &value})
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return nil, err
	}
	if err = pc.SetLocalDescription(offer); err != nil {
		return nil, err
	}
	if err = s.send(signalCtx, message{Type: "offer", Peer: id, SDP: pc.LocalDescription()}); err != nil {
		return nil, err
	}
	success = true
	s.mu.Lock()
	s.bitrates[id] = 4000000
	s.mu.Unlock()
	return p, nil
}

func (s *Session) setBitrate(id string, value uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if value == 0 {
		delete(s.bitrates, id)
	} else {
		if _, active := s.bitrates[id]; !active {
			return
		}
		s.bitrates[id] = value
	}
	bitrate := uint64(4000000)
	// One encoder serves all viewers; the slowest reported link sets its bitrate.
	for _, estimate := range s.bitrates {
		bitrate = min(bitrate, estimate)
	}
	s.capture.bitrate(bitrate)
}
