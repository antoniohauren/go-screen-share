//go:build linux && cgo

package sharer

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	portalName    = "org.freedesktop.portal.Desktop"
	portalPath    = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	screenCast    = "org.freedesktop.portal.ScreenCast"
	portalSession = "org.freedesktop.portal.Session"
	portalRequest = "org.freedesktop.portal.Request"
)

// Source owns one portal selection and its PipeWire remote. Do not copy it.
// Pipeline is for Start in this process; it contains a process-local descriptor.
type Source struct {
	Pipeline string

	conn      *dbus.Conn
	owner     string
	session   dbus.ObjectPath
	remote    *os.File
	done      chan struct{}
	cancel    context.CancelFunc
	endOnce   sync.Once
	closeOnce sync.Once
}

// Done closes on portal closure, bus/portal loss, parent cancellation, or Close.
// The caller must stop its Session, then Close the Source, even after Done closes.
func (s *Source) Done() <-chan struct{} { return s.done }

// Close releases the portal, private bus connection, and PipeWire descriptor.
// It is idempotent. Call only after Session.Stop (or after Start fails).
func (s *Source) Close() {
	s.closeOnce.Do(func() {
		s.end()
		// Repeat after any in-flight selection call: cancellation may have raced
		// CreateSession before the predicted session object existed.
		s.closeObject(s.session, portalSession)
		_ = s.conn.Close()
		if s.remote != nil {
			_ = s.remote.Close()
		}
	})
}

func (s *Source) end() {
	s.endOnce.Do(func() {
		close(s.done)
		s.cancel()
		// Revocation must not wait for the caller to finish stopping GStreamer.
		// In particular, never close the PipeWire FD here.
		s.closeObject(s.session, portalSession)
	})
}

func (s *Source) closeObject(path dbus.ObjectPath, iface string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.conn.Object(s.owner, path).CallWithContext(ctx, iface+".Close", 0).Err
}

// SelectSource opens the native GNOME Wayland picker, without a consent probe.
// Monitor selections include the default output's entire system audio mix;
// windows are video-only. The UI must disclose this before calling SelectSource.
// Keep ctx alive throughout sharing. Cancellation revokes the portal promptly,
// but the caller retains responsibility for stopping capture and calling Close.
func SelectSource(ctx context.Context) (*Source, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gnome := false
	for _, desktop := range strings.FieldsFunc(os.Getenv("XDG_CURRENT_DESKTOP")+":"+os.Getenv("XDG_SESSION_DESKTOP"), func(r rune) bool { return r == ':' || r == ';' }) {
		gnome = gnome || strings.EqualFold(desktop, "gnome")
	}
	if !strings.EqualFold(os.Getenv("XDG_SESSION_TYPE"), "wayland") || !gnome {
		return nil, fmt.Errorf("source selection requires a GNOME Wayland session")
	}
	conn, err := dbus.SessionBusPrivate()
	if err != nil {
		return nil, fmt.Errorf("portal bus: %w", err)
	}
	// Allow cancellation during authentication, but detach before selection:
	// later cancellation must leave the bus alive to send portal Close calls.
	stopConnect := context.AfterFunc(ctx, func() { _ = conn.Close() })
	if err = conn.Auth(nil); err == nil {
		err = conn.Hello()
	}
	stopConnect()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("portal bus: %w", err)
	}
	selection, cancel := context.WithCancel(ctx)
	sender := strings.ReplaceAll(strings.TrimPrefix(conn.Names()[0], ":"), ".", "_")
	token := "share_" + rand.Text()
	s := &Source{
		conn: conn, owner: portalName, cancel: cancel, done: make(chan struct{}),
		session: dbus.ObjectPath(string(portalPath) + "/session/" + sender + "/" + token),
	}
	ready := false
	defer func() {
		if !ready {
			s.Close()
		}
	}()
	if !conn.SupportsUnixFDs() {
		return nil, fmt.Errorf("portal bus does not support Unix file descriptors")
	}
	var properties map[string]dbus.Variant
	if err := conn.Object(portalName, portalPath).CallWithContext(selection,
		"org.freedesktop.DBus.Properties.GetAll", 0, screenCast).Store(&properties); err != nil {
		return nil, fmt.Errorf("screen cast capabilities: %w", err)
	}
	types, _ := properties["AvailableSourceTypes"].Value().(uint32)
	cursors, _ := properties["AvailableCursorModes"].Value().(uint32)
	if types&3 != 3 || cursors&2 == 0 {
		return nil, fmt.Errorf("portal must support monitors, windows, and an embedded cursor")
	}
	if err := conn.BusObject().CallWithContext(selection, "org.freedesktop.DBus.GetNameOwner", 0, portalName).Store(&s.owner); err != nil {
		return nil, fmt.Errorf("portal owner: %w", err)
	}
	// Pin calls/signals to this backend instance; do not silently follow a restart.
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	for _, match := range [][]dbus.MatchOption{
		{dbus.WithMatchSender(s.owner), dbus.WithMatchObjectPath(s.session), dbus.WithMatchInterface(portalSession), dbus.WithMatchMember("Closed")},
		{dbus.WithMatchSender("org.freedesktop.DBus"), dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"), dbus.WithMatchArg(0, s.owner)},
	} {
		if err := conn.AddMatchSignalContext(selection, match...); err != nil {
			return nil, fmt.Errorf("portal subscription: %w", err)
		}
	}
	go func() {
		defer conn.RemoveSignal(signals)
		defer s.end()
		for {
			select {
			case <-selection.Done():
				return
			case <-conn.Context().Done():
				return
			case signal, ok := <-signals:
				if !ok {
					return
				}
				if signal.Sender == s.owner && signal.Path == s.session && signal.Name == portalSession+".Closed" {
					return
				}
				if signal.Sender == "org.freedesktop.DBus" && signal.Name == "org.freedesktop.DBus.NameOwnerChanged" {
					var name, oldOwner, newOwner string
					if dbus.Store(signal.Body, &name, &oldOwner, &newOwner) == nil && name == s.owner && newOwner == "" {
						return
					}
				}
			}
		}
	}()
	results, err := s.request(selection, "CreateSession", map[string]dbus.Variant{
		"session_handle_token": dbus.MakeVariant(token),
	})
	if err != nil {
		return nil, err
	}
	// The portal specification deliberately returns session_handle as a string.
	handle, ok := results["session_handle"].Value().(string)
	if !ok || handle != string(s.session) {
		if path := dbus.ObjectPath(handle); ok && path.IsValid() && path != s.session {
			s.closeObject(path, portalSession)
		}
		return nil, fmt.Errorf("portal returned an unexpected session handle")
	}
	if _, err := s.request(selection, "SelectSources", map[string]dbus.Variant{
		"types": dbus.MakeVariant(uint32(3)), "multiple": dbus.MakeVariant(false),
		"cursor_mode": dbus.MakeVariant(uint32(2)),
	}, s.session); err != nil {
		return nil, err
	}
	results, err = s.request(selection, "Start", map[string]dbus.Variant{}, s.session, "")
	if err != nil {
		return nil, err
	}
	var streams []struct {
		Node       uint32
		Properties map[string]dbus.Variant
	}
	streamList := results["streams"]
	if streamList.Signature().String() != "a(ua{sv})" {
		return nil, fmt.Errorf("portal returned missing or invalid streams")
	}
	if err := dbus.Store([]interface{}{streamList.Value()}, &streams); err != nil || len(streams) != 1 {
		return nil, fmt.Errorf("portal must return exactly one PipeWire stream")
	}
	if streams[0].Node == 0 || streams[0].Node == ^uint32(0) {
		return nil, fmt.Errorf("portal returned an invalid PipeWire node")
	}
	sourceType, _ := streams[0].Properties["source_type"].Value().(uint32)
	if sourceType != 1 && sourceType != 2 {
		return nil, fmt.Errorf("portal returned unsupported source_type %d", sourceType)
	}
	// Do not abandon an FD-bearing reply on cancellation. Close the connection
	// to unblock the call, then consume its result and own any received FD.
	call := conn.Object(s.owner, portalPath).Go(screenCast+".OpenPipeWireRemote", 0,
		make(chan *dbus.Call, 1), s.session, map[string]dbus.Variant{})
	select {
	case <-call.Done:
	case <-selection.Done():
		s.end()
		_ = conn.Close()
		<-call.Done
	}
	var fd dbus.UnixFD
	if err := call.Store(&fd); err != nil {
		if selection.Err() != nil {
			return nil, selection.Err()
		}
		return nil, fmt.Errorf("portal PipeWire remote: %w", err)
	}
	if fd < 0 {
		return nil, fmt.Errorf("portal returned an invalid PipeWire descriptor")
	}
	syscall.CloseOnExec(int(fd))
	s.remote = os.NewFile(uintptr(fd), "portal-pipewire")
	if err := selection.Err(); err != nil {
		return nil, err
	}
	// A fixed canvas preserves aspect ratio (letterboxing) even on window resize.
	// Prefer 30 fps for variable-rate sources, but allow lower rates: drop-only
	// cannot negotiate a higher fixed rate than its input. It adds no frame wait.
	// Exclude 0/1 output (unbounded cadence); 1/G_MAXINT is the smallest fraction.
	// keepalive-time (milliseconds) covers static screens after the first frame.
	s.Pipeline = fmt.Sprintf("pipewiresrc fd=%d path=%d do-timestamp=true keepalive-time=100 provide-clock=false ! "+
		"queue leaky=downstream max-size-buffers=2 max-size-bytes=0 max-size-time=0 ! "+
		"videorate drop-only=true max-rate=30 ! video/x-raw,framerate=30/1;video/x-raw,framerate=[1/2147483647,30/1] ! "+
		"videoconvert ! videoscale add-borders=true ! video/x-raw,format=I420,width=1920,height=1080,pixel-aspect-ratio=1/1 ! "+
		"vp8enc name=video_encoder deadline=1 cpu-used=8 lag-in-frames=0 target-bitrate=4000000 keyframe-max-dist=30 ! "+
		"rtpvp8pay pt=96 perfect-rtptime=false ! appsink name=video sync=false max-buffers=128 drop=true", fd, streams[0].Node)
	if sourceType == 1 {
		// Pulse resolves this alias to the default sink's monitor, not its default
		// input. Never retry with an unset device or a microphone on audio failure.
		s.Pipeline += " pulsesrc device=\"@DEFAULT_MONITOR@\" " +
			"stream-properties=\"props,node.dont-reconnect=(string)true,node.dont-fallback=(string)true,node.dont-move=(string)true\" " +
			"provide-clock=false buffer-time=20000 latency-time=10000 ! " +
			"queue leaky=downstream max-size-buffers=8 max-size-bytes=0 max-size-time=0 ! " +
			"audioconvert ! audioresample ! audio/x-raw,rate=48000,channels=2 ! " +
			"opusenc bitrate=128000 frame-size=20 ! rtpopuspay pt=111 perfect-rtptime=false ! " +
			"appsink name=audio sync=false max-buffers=128 drop=true"
	}
	if err := selection.Err(); err != nil {
		return nil, err
	}
	ready = true
	return s, nil
}

func (s *Source) request(ctx context.Context, method string, options map[string]dbus.Variant, args ...interface{}) (map[string]dbus.Variant, error) {
	token := "share_" + rand.Text()
	sender := strings.ReplaceAll(strings.TrimPrefix(s.conn.Names()[0], ":"), ".", "_")
	path := dbus.ObjectPath(string(portalPath) + "/request/" + sender + "/" + token)
	options["handle_token"] = dbus.MakeVariant(token)
	signals := make(chan *dbus.Signal, 16)
	s.conn.Signal(signals)
	defer s.conn.RemoveSignal(signals)
	match := []dbus.MatchOption{dbus.WithMatchSender(s.owner), dbus.WithMatchObjectPath(path), dbus.WithMatchInterface(portalRequest), dbus.WithMatchMember("Response")}
	if err := s.conn.AddMatchSignalContext(ctx, match...); err != nil {
		return nil, fmt.Errorf("portal %s subscription: %w", method, err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.conn.RemoveMatchSignalContext(cleanup, match...)
	}()
	complete := false
	defer func() {
		if !complete {
			s.closeObject(path, portalRequest)
		}
	}()
	var returned dbus.ObjectPath
	if err := s.conn.Object(s.owner, portalPath).CallWithContext(ctx, screenCast+"."+method, 0, append(args, options)...).Store(&returned); err != nil {
		return nil, fmt.Errorf("portal %s: %w", method, err)
	}
	if returned != path {
		if returned.IsValid() {
			s.closeObject(returned, portalRequest)
		}
		return nil, fmt.Errorf("portal %s returned an unexpected request handle", method)
	}
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case signal, ok := <-signals:
			if !ok {
				return nil, fmt.Errorf("portal connection closed")
			}
			if signal.Sender != s.owner || signal.Path != path || signal.Name != portalRequest+".Response" {
				continue
			}
			if len(signal.Body) != 2 {
				return nil, fmt.Errorf("portal %s returned an invalid response body", method)
			}
			response, responseOK := signal.Body[0].(uint32)
			// Preserve decoded variant signatures: dbus.Store rebuilds map values
			// and changes arrays of decoded structs from a(ua{sv}) to aav.
			results, resultsOK := signal.Body[1].(map[string]dbus.Variant)
			if !responseOK || !resultsOK {
				return nil, fmt.Errorf("portal %s returned invalid response types", method)
			}
			complete = true
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			switch response {
			case 0:
				return results, nil
			case 1:
				return nil, fmt.Errorf("portal %s: %w", method, context.Canceled)
			default:
				return nil, fmt.Errorf("portal %s failed (response %d)", method, response)
			}
		}
	}
}
