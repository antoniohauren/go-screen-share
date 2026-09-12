> Subsequent requirement: Both platforms now offer explicit app-audio selection
> with a window or screen, superseding the window-audio exclusions below. Linux
> isolates a selected playback stream; Windows isolates the selected window's
> process tree through WASAPI loopback. Audio selection is independent of video
> selection, and other windows/tabs sharing that stream or process tree may be
> audible. Without app selection, screens share system audio and windows remain
> video-only. See README's platform sharing instructions.

## Problem Statement

Friends need a small desktop application that can share either a selected window or full screen directly to browser viewers. They need no account, chat, recording, remote control, or media-hosting service.

## Solution

A Go/Wails sharer application creates a WebRTC sharing session and displays a browser URL with a one-time code. The app uses native capture selection on Windows 11 and GNOME Wayland. Viewers open the URL in a supported desktop browser, enter the code, and receive P2P media. A public signaling service relays setup messages only.

## User Stories

1. As a sharer, I want to run a portable Windows executable, so that I can share without an installer.
2. As a sharer, I want to run an AppImage on supported GNOME Wayland systems, so that I can share without distribution-specific packaging.
3. As a sharer, I want a native picker to choose a full screen, so that I control what viewers see.
4. As a sharer, I want a native picker to choose a window, so that I do not expose unrelated desktop content.
5. As a sharer, I want full-screen sharing to include system audio, so that viewers hear content played on my system.
6. As a sharer, I want window sharing to send only selected-window video, so that v1 works within portal and native-picker limits.
7. As a sharer, I want to start a sharing session with one simple flow, so that I can begin quickly.
8. As a sharer, I want a URL and one-time code, so that I can give viewers the minimum connection details.
9. As a viewer, I want to open the shared URL in Chrome, Edge, or Firefox, so that I need no native application.
10. As a viewer, I want to enter the session code, so that I can join a sharing session.
11. As a sharer, I want media to travel P2P, so that the service does not host my screen or audio.
12. As a sharer, I want up to four viewers, so that I can share with a small group of friends.
13. As a fifth viewer, I want a clear session-full result, so that I know why I cannot join.
14. As a viewer, I want to reconnect while sharing remains active, so that brief network failures do not require a new session.
15. As a sharer, I want the code to remain usable until I stop sharing, so that friends can join later in the session.
16. As a sharer, I want the session to stop immediately when I choose Stop, so that viewers lose access when I am done.
17. As a sharer, I want viewer count displayed, so that I know whether anyone is watching.
18. As a sharer, I want adaptive video targeting 1080p at 30 fps, so that screen content is readable while WebRTC can adapt to network conditions.
19. As a user, I want no chat, remote control, recording, accounts, or analytics, so that application remains focused on sharing.

## Implementation Decisions

- Build a Go desktop sharer with Wails and a browser receiver.
- Support Windows 11 and current GNOME Wayland installations with PipeWire and `xdg-desktop-portal-gnome`.
- Use native capture pickers. Do not build a custom window list.
- Full-screen capture includes system audio. GNOME uses PipeWire system-mix capture alongside portal video; Windows uses system-loopback capture.
- Window capture is video-only. GNOME ScreenCast portal cannot isolate app/window audio. Native Windows picker cannot map selected capture item to process audio without replacing picker with custom selection.
- Use WebRTC for media. Target adaptive 1080p/30fps with no v1 quality selector.
- Use a public HTTPS/WSS signaling service for SDP and ICE exchange only. Do not relay media.
- Use STUN only. Do not deploy TURN in v1.
- Give each session an opaque one-time code and receiver URL. Code is valid until sharer stops. No separate approval prompt is required.
- Allow a maximum of four simultaneous viewers. Sharer sends one WebRTC stream per viewer. Reject additional viewers as session full.
- Let a disconnected viewer reconnect using the active session code.
- Sharer UI contains only capture selection, URL/code, viewer count, and Stop.
- Do not persist user records, session history, analytics, or logs beyond operational process output. Do not add public-service rate limiting in v1.
- Ship a portable `.exe` on Windows and an AppImage on Linux. Do not add installers, package repositories, auto-update, or code signing in v1.
- Future v2 may add server-streamed media for greater viewer counts; it is not part of this design.

## Testing Decisions

- Test externally observable behavior, not internal implementation details.
- Use one high-level `ShareSession` lifecycle seam. Drive start, viewer join/leave, reconnect, capacity rejection, and stop; assert observable session state and signaling outcomes.
- Verify native picker, full-screen audio capture, window video capture, and browser WebRTC interoperability manually on Windows 11 and supported GNOME Wayland environments.
- Verify four simultaneous viewers can connect and a fifth receives session-full behavior.
- Verify stopping sharing disconnects viewers and invalidates the session code.
- No existing test prior art exists because repository starts empty.

## Out of Scope

- Window/application audio capture.
- Chat, annotations, remote control, file transfer, recording, or screen snapshots.
- User accounts, authentication flows, viewer approval, history, analytics, or persistent data.
- Mobile or Safari receivers.
- Windows versions before Windows 11.
- Linux desktops outside supported GNOME Wayland/PipeWire/portal environment.
- TURN, media relays, SFU/server-streaming, or more than four viewers.
- Resolution controls, installer, package repository, auto-update, and code signing.
- Rate limiting and abuse controls for public signaling service.

## Further Notes

- WebRTC media is encrypted in transit; signaling service must use HTTPS/WSS.
- Direct P2P may fail on restrictive NATs because v1 intentionally has no TURN fallback.
- Session codes intentionally do not expire while sharer remains active. This is appropriate only for friend-to-friend use.
