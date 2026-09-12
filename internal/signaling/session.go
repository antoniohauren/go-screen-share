// Package signaling owns the ShareSession lifecycle. It never receives media.
package signaling

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type message struct {
	Type      string          `json:"type"`
	Session   string          `json:"session,omitempty"`
	Code      string          `json:"code,omitempty"`
	URL       string          `json:"url,omitempty"`
	State     string          `json:"state,omitempty"`
	Viewers   int             `json:"viewers"`
	Peer      string          `json:"peer,omitempty"`
	SDP       json.RawMessage `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type client struct {
	id      string
	conn    *websocket.Conn
	out     chan message
	session *shareSession
}

type shareSession struct {
	id, code       string
	sharer, viewer *client
}

type Server struct {
	// ponytail: one short lifecycle lock; shard by session if contention matters.
	mu       sync.Mutex
	origin   string
	sessions map[string]*shareSession
	clients  map[*client]bool
	closed   bool
}

func New(origin string) *Server {
	return &Server{origin: origin, sessions: make(map[string]*shareSession), clients: make(map[*client]bool)}
}

// Close disconnects all signaling clients, including clients not yet joined.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for c := range s.clients {
		c.conn.CloseNow()
	}
}

func token() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read either fills the buffer or terminates.
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/session" {
		http.NotFound(w, r)
		return
	}
	if r.TLS == nil {
		http.Error(w, "HTTPS required", http.StatusUpgradeRequired)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"wails://wails.localhost", "http://wails.localhost"},
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 10)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	c := &client{conn: conn, out: make(chan message, 32)}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.clients[c] = true
	s.mu.Unlock()
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.emit(message{Type: "heartbeat"})
				pingCtx, done := context.WithTimeout(ctx, 10*time.Second)
				err := conn.Ping(pingCtx)
				done()
				if err != nil {
					conn.CloseNow()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		for {
			select {
			case msg := <-c.out:
				data, err := json.Marshal(msg)
				if err != nil {
					conn.CloseNow()
					return
				}
				writeCtx, done := context.WithTimeout(ctx, 5*time.Second)
				err = conn.Write(writeCtx, websocket.MessageText, data)
				done()
				if err != nil {
					conn.CloseNow()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.clients, c)
		if session := c.session; session != nil {
			if session.viewer == c {
				session.viewer = nil
				session.sharer.emit(message{Type: "viewer-left", Peer: c.id, State: "sharing"})
			} else {
				s.stop(session)
			}
			c.session = nil
		}
	}()
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg message
		if kind != websocket.MessageText || json.Unmarshal(data, &msg) != nil {
			c.emit(message{Type: "error", Error: "invalid-message"})
			continue
		}
		s.mu.Lock()
		s.handle(c, msg)
		s.mu.Unlock()
	}
}

func (c *client) emit(msg message) {
	select {
	case c.out <- msg:
	default:
		c.conn.CloseNow()
	}
}

func (s *Server) handle(c *client, msg message) {
	if msg.Type == "disconnect-peer" {
		session := c.session
		if session == nil || session.sharer != c {
			c.emit(message{Type: "error", Error: "not-sharer"})
			return
		}
		if session.viewer == nil || session.viewer.id != msg.Peer {
			return
		}
		viewer := session.viewer
		viewer.session = nil
		session.viewer = nil
		viewer.emit(message{Type: "error", Error: "connection-failed"})
		c.emit(message{Type: "viewer-left", Peer: viewer.id, State: "sharing"})
		return
	}
	if msg.Type == "stop" {
		if c.session == nil || c.session.sharer != c {
			c.emit(message{Type: "error", Error: "not-sharer"})
			return
		}
		s.stop(c.session)
		return
	}
	if msg.Type == "join" {
		if c.session != nil {
			c.emit(message{Type: "error", Error: "already-joined"})
			return
		}
		session := s.sessions[msg.Session]
		if session == nil || subtle.ConstantTimeCompare([]byte(session.code), []byte(msg.Code)) != 1 {
			c.emit(message{Type: "error", Error: "invalid-code"})
			return
		}
		if session.viewer != nil {
			c.emit(message{Type: "error", Error: "session-full"})
			return
		}
		c.session = session
		c.id = token()
		session.viewer = c
		c.emit(message{Type: "joined", Peer: c.id, State: "sharing", Viewers: 1})
		session.sharer.emit(message{Type: "viewer-joined", Peer: c.id, State: "sharing", Viewers: 1})
		return
	}
	if msg.Type == "start" {
		if c.session != nil {
			c.emit(message{Type: "error", Error: "already-joined"})
			return
		}
		session := &shareSession{id: token(), code: token(), sharer: c}
		s.sessions[session.id] = session
		c.session = session
		c.emit(message{Type: "started", Session: session.id, Code: session.code, URL: s.origin + "/?session=" + session.id, State: "sharing"})
		return
	}
	if msg.Type == "offer" || msg.Type == "answer" || msg.Type == "ice" {
		session := c.session
		if session == nil {
			c.emit(message{Type: "error", Error: "not-joined"})
			return
		}
		// A departed viewer's in-flight signaling must not end a healthy share.
		if session.viewer == nil || msg.Peer != session.viewer.id {
			return
		}
		valid := true
		if msg.Type == "ice" {
			var ice struct {
				Candidate        *string `json:"candidate"`
				SDPMid           *string `json:"sdpMid"`
				SDPMLineIndex    *uint16 `json:"sdpMLineIndex"`
				UsernameFragment *string `json:"usernameFragment"`
			}
			valid = valid && json.Unmarshal(msg.Candidate, &ice) == nil && ice.Candidate != nil
			msg.Candidate, _ = json.Marshal(ice)
		} else {
			var sdp struct {
				Type string `json:"type"`
				SDP  string `json:"sdp"`
			}
			valid = valid && json.Unmarshal(msg.SDP, &sdp) == nil && sdp.Type == msg.Type && sdp.SDP != ""
			valid = valid && ((msg.Type == "offer" && c == session.sharer) || (msg.Type == "answer" && c == session.viewer))
			msg.SDP, _ = json.Marshal(sdp)
		}
		if !valid {
			c.emit(message{Type: "error", Peer: msg.Peer, Error: "invalid-message"})
			return
		}
		target := session.sharer
		if c == session.sharer {
			target = session.viewer
		}
		relay := message{Type: msg.Type, Peer: msg.Peer}
		if msg.Type == "ice" {
			relay.Candidate = msg.Candidate
		} else {
			relay.SDP = msg.SDP
		}
		target.emit(relay)
		return
	}
	c.emit(message{Type: "error", Error: "invalid-message"})
}

func (s *Server) stop(session *shareSession) {
	delete(s.sessions, session.id)
	for _, c := range []*client{session.sharer, session.viewer} {
		if c != nil {
			c.session = nil
			c.emit(message{Type: "stopped", State: "stopped"})
		}
	}
}
