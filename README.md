# Screen Share

Windows 11 and GNOME Wayland Wails sharers with an HTTPS browser receiver for
**up to four viewers**. Media travels directly from the sharer over WebRTC. The Go
service holds ephemeral session state and relays SDP/ICE only. No TURN, media
hosting, accounts, recording, or persistent session data.

## Build

Requires Go 1.24+; the Windows desktop needs a current Microsoft Edge WebView2
Evergreen Runtime. The capture picker is WebView2's built-in screen/window picker,
not a custom window list or a direct WinRT `GraphicsCapturePicker` integration.
WebView2 handles native capture, system-loopback audio, encoding, and WebRTC.

```sh
go build -o signaling ./cmd/signaling
GOOS=windows go build -tags production -ldflags="-H windowsgui" -o screen-share.exe ./cmd/sharer
```

The Windows executable embeds its UI; there is no npm build or installer.
Windows resource metadata/signing and Linux AppImage packaging are outside this slice.

### Linux Build

Requires Go 1.24+, a C compiler, `pkg-config`, GTK3/WebKitGTK 4.1 development
libraries, and GStreamer development libraries (`gstreamer-app-1.0`). Runtime
requires GNOME Wayland, PipeWire, `pipewire-pulse`, `xdg-desktop-portal-gnome`, and
GStreamer plugins providing `pipewiresrc`, `pulsesrc`, `videoconvert`, `videoscale`,
`videorate`, `vp8enc`, `rtpvp8pay`, `audioconvert`, `audioresample`, `opusenc`,
`rtpopuspay`, and `appsink`. Tests additionally use `videotestsrc` and `audiotestsrc`.
On Arch these come from GTK3/WebKit2GTK 4.1, GStreamer base/good plugins, and PipeWire
packages; other distributions split development packages separately.

```sh
go build -tags 'production,webkit2_41' -o screen-share ./cmd/sharer
SCREENSHARE_URL=https://share.example.com ./screen-share
```

Linux uses Wails only for UI. Native Go code opens the ScreenCast portal, reads its
PipeWire video stream through GStreamer, and sends VP8/Opus with Pion WebRTC.
WebKitGTK does not need browser-side WebRTC support. One encoder feeds all viewers;
receiver REMB feedback adjusts bitrate between 200 kbps and 4 Mbps for the slowest
reported link. Sender reports retain the common capture-clock mapping for audio
and video, rather than treating encoder/delivery delays as presentation offsets.
Video preserves aspect ratio in a 1920x1080 canvas at up to 30 fps.
Without bitrate feedback the encoder stays at its 4 Mbps target. No media reaches
the signaling service, and no microphone is opened.

### Share On GNOME Wayland

1. Click **Choose screen or window** and select one source in the GNOME portal picker.
2. Selecting a screen also captures the complete mix playing through the default
   speakers/headphones at capture start. This is separate from portal video consent
   and disclosed in the UI before selection. It does not combine multiple output
   devices, capture a microphone, or isolate individual applications. PipeWire
   stream properties disable movement, reconnection, and fallback to other outputs.
3. Selecting a window opens video capture only, with no audio capture pipeline.
4. URL/code appear only after every required media track produces packets and
   secure signaling starts. Missing system audio fails closed; there is no input fallback.
5. Stop cancels a pending picker or ends local media and portal access. Portal closure,
   capture errors, app exit, and signaling loss also end sharing. Restart after changing
   the default audio device to select its new mix.

This is a dynamically linked Linux executable, not yet a self-contained AppImage.

## Run Public Signaling

Provision a public DNS name, trusted TLS certificate and key, and allow inbound
TCP 443. Run under an unprivileged service account with permission to bind that
port, or map public port 443 to an unprivileged listening port. Keep the private
key outside the repository and readable only by the service account.

```sh
./signaling -addr :8443 -origin https://share.example.com \
  -cert /run/secrets/fullchain.pem -key /run/secrets/privkey.pem
```

This example assumes public 443 maps to service 8443. Without that mapping,
include `:8443` in the public origin and open that port instead. The service
terminates TLS itself; plain HTTP and TLS-offloading to an HTTP upstream are
not supported. Restart after certificate renewal. No public deployment is
created by building this repository.

The same origin serves the receiver at `/` and WSS signaling at `/session`.
Keep this connection open for the entire share. Service restart or detected
sharer disconnect invalidates access. A server ping every 20 seconds with a
10-second response deadline detects silent signaling loss in about 30 seconds.
Client-visible heartbeats also stop local media and clear stale access details
after 35 seconds without signaling messages (subject to browser timer scheduling).
Stop always ends local capture immediately; during a network partition, server
code invalidation waits for that disconnect detection. There is deliberately no rate limiting in v1;
this is friend-to-friend software, not an abuse-hardened public platform.

## Share On Windows

Launch from PowerShell with the public signaling origin configured:

```powershell
$env:SCREENSHARE_URL = "https://share.example.com"
.\screen-share.exe
```

1. Click **Choose screen or window**. Select a full screen and enable system
   audio in the picker, or choose a window for video only.
2. Full-screen selection without a live audio track fails closed. Window
   selection removes and stops all audio tracks. Tabs/unknown sources are rejected.
3. Send the displayed URL and code privately to up to four friends. They open the URL
   in desktop Chrome, Edge, or Firefox and enter the code. A fifth viewer gets
   a session-full message.
4. Click **Stop sharing**, or stop capture through the platform controls.
   Capture and all peer connections end; the active code is invalidated.

Capture targets adaptive 1920x1080 at 30 fps, not a guaranteed fixed resolution.
STUN uses `stun:stun.l.google.com:19302`; restrictive NAT/firewall combinations
can prevent connection because there is no TURN fallback. Reconnect is available
with the same code while the sharer remains connected and sharing.

## Verify

Automated tests use the agreed **ShareSession lifecycle seam**, exercised through
real TLS/WebSocket clients rather than internal mocks:

```sh
go test -race ./...
go vet ./...
node --check web/sharer/sharer.js
node --check web/viewer/viewer.js
```

On Linux with WebKitGTK 4.1, use `go test -race -tags webkit2_41 ./...` and
`go vet -tags webkit2_41 ./...`. Building only `./cmd/signaling` does not require
GTK or GStreamer. Native sharer tests require GStreamer and cgo.

Tests cover start URL/code/state, wrong codes, four-viewer capacity, leave/rejoin counts,
per-viewer SDP/ICE exchange and isolation, harmless stale-peer signaling, wrong-direction rejection,
failed-peer removal without interrupting another viewer, Stop for all viewers, sharer disconnect,
new credentials on restart, HTTPS enforcement, and service shutdown. The silent
disconnect check takes 32 seconds; `go test -short ./...` skips real-time heartbeat checks.

Linux lifecycle tests also exercise real GStreamer test sources and Pion receiver
connections: video-only SDP/media, four simultaneous audio/video receivers,
reconnect, no access on capture/audio readiness failure or cancellation,
capture-loss cleanup, foreign access-URL rejection, failed-peer isolation, Stop
invalidation, and A/V clock alignment despite deliberately delayed audio delivery.
Synthetic media tests do not establish physical screen/audio correctness.

### Manual Acceptance Gates

These require Windows 11, a public HTTPS deployment, and real desktop browsers.
They have **not** been verified by the Linux-hosted automated suite.

- Launch the built executable with current WebView2; confirm picker fits at
  normal and high DPI. Cancel selection: no session or capture remains.
- Share an entire screen while playing system audio. Verify moving video and
  sound in Chrome, Edge, and Firefox, including playback/unmute controls.
- Omit screen audio in the picker: Start must fail without exposing access details.
- Share a window while system audio plays: receiver gets only that window and
  no audio. Reject browser-tab capture if offered.
- Connect four browser viewers simultaneously; verify media reaches each and
  the sharer count shows four. Join a fifth browser: session full.
- Disconnect one viewer; count drops to three and other viewers keep receiving
  media. Reconnect with the same URL/code; media resumes and count returns to four.
- Fail one viewer's negotiation or network connection; verify other viewers
  remain connected and the failed viewer can reconnect with the active code.
- Stop while choosing, connecting, negotiating, and streaming. Confirm platform
  capture indicator clears, all four receivers stop, and old credentials cannot rejoin.
- Close the app, interrupt its network, and restart the signaling service.
  Verify media cleanup and invalidation after signaling disconnect is detected.
- Test different networks, including a restrictive NAT; failures must explain
  the P2P limitation rather than suggesting a nonexistent relay fallback.
- Check receiver layout at desktop and mobile viewport widths (mobile playback
  is not a supported v1 receiver target), keyboard navigation, and status feedback.

### Capture References

- [Microsoft WebView2 ScreenCaptureStarting documentation](https://learn.microsoft.com/en-us/microsoft-edge/webview2/reference/win32/icorewebview2_27#add_screencapturestarting)
- [Microsoft WebView2 getDisplayMedia sample](https://github.com/MicrosoftEdge/WebView2Samples/blob/main/SampleApps/WebView2APISample/assets/ScenarioScreenCapture.html)
- [WebView2 screen/system-audio report](https://github.com/MicrosoftEdge/WebView2Feedback/issues/4327)
- [Screen Capture API: systemAudio is a preference, not a guarantee](https://www.w3.org/TR/screen-capture/#dom-displaymediastreamoptions-systemaudio)
- [ScreenCast portal API](https://flatpak.github.io/xdg-desktop-portal/docs/doc-org.freedesktop.portal.ScreenCast.html)
- [GStreamer PipeWire source](https://docs.pipewire.org/page_gstreamer.html)

### GNOME Manual Acceptance Gates

These gates remain **unverified**. Development environment: GNOME 50.4 on Wayland,
PipeWire 1.6.8, WebKitGTK 4.1 API / 2.52.6, GStreamer 1.28.6. Record actual OS,
runtime versions, browser versions, and results when performing acceptance.

- Launch the production build; verify picker focus, screen/window choices, keyboard
  access, normal/high DPI, and cancellation without leaving a portal session.
- Stop while picker is open and during connection setup. Immediately restart; old
  callbacks must not expose a URL/code or leave a platform capture indicator.
- Share a screen while playing sound through default speakers/headphones. Verify
  moving video and matching sound in desktop Chrome, Edge, and Firefox. Speak into
  the microphone and play sound on another output: neither should be included.
- Share a window while system audio plays: only that window reaches viewers, and
  SDP/media contain no audio. Move/resize/occlude the window and leave screen static.
- Remove/unavailable default audio output before Start: no URL/code is exposed.
  Remove output during capture: failure must stop sharing, never select microphone.
- Open four browser viewers, leave/reconnect one, and reject a fifth. Late viewers
  must decode video promptly; failure of one peer must not interrupt the other three.
- Stop via app and GNOME capture controls, close app, interrupt signaling, and
  restart portal/PipeWire. Verify local audio/video stop, access details clear,
  and old credentials fail after signaling disconnect detection.
- Test cross-network browser interoperability, A/V sync, bitrate adaptation, and
  restrictive NAT failure feedback. There is no TURN relay fallback.
