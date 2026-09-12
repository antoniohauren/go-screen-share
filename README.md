# Screen Share

Windows 11 Wails sharer and HTTPS browser receiver for **up to four viewers**.
Media travels directly between WebView2 and the viewer over WebRTC. The Go
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
Windows resource metadata/signing and Linux desktop capture are outside this slice.

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

Tests cover start URL/code/state, wrong codes, four-viewer capacity, leave/rejoin counts,
per-viewer SDP/ICE exchange and isolation, harmless stale-peer signaling, wrong-direction rejection,
failed-peer removal without interrupting another viewer, Stop for all viewers, sharer disconnect,
new credentials on restart, HTTPS enforcement, and service shutdown. The silent
disconnect check takes 32 seconds; `go test -short ./...` skips real-time heartbeat checks.

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
