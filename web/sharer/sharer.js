"use strict";

const startButton = document.querySelector("#start");
const stopButton = document.querySelector("#stop");
const statusText = document.querySelector("#status");
const stateText = document.querySelector("#state");
const viewerCount = document.querySelector("#viewers");
const urlField = document.querySelector("#url");
const codeField = document.querySelector("#code");
const audioApp = document.querySelector("#audio-app");
const refreshAudio = document.querySelector("#refresh-audio");
let origin;
let active;
let native = false;
let nativeStart = Promise.resolve();
let nativeStopping = false;

refreshAudio.addEventListener("click", async () => {
  refreshAudio.disabled = true;
  try {
    const apps = await window.go.main.App.AudioApps();
    if (startButton.disabled) return;
    const selected = audioApp.value;
    audioApp.replaceChildren(new Option("Screen: system audio / Window: no audio", ""));
    for (const app of apps) audioApp.add(new Option(app.name, app.id));
    if (selected && !apps.some(app => app.id === selected)) {
      audioApp.add(new Option("Previous stream ended — refresh and select again", selected));
    }
    audioApp.value = selected;
    statusText.textContent = apps.length ? "Choose the app audio stream, then choose your screen or window." : "No playback streams found. Play sound in your app, then refresh.";
  } catch (error) {
    statusText.textContent = `Cannot list app audio: ${error.message || error}`;
  } finally {
    refreshAudio.disabled = startButton.disabled;
  }
});

function stop(message = "Capture stopped.", confirmed = false) {
  if (native) {
    nativeStopping = true;
    startButton.disabled = true;
    stopButton.disabled = true;
    urlField.value = "";
    codeField.value = "";
    viewerCount.textContent = "0";
    stateText.textContent = "STOPPING";
    statusText.textContent = "Stopping native capture...";
    nativeStart.catch(() => {}).then(() => window.go.main.App.Stop()).then(() => {
      nativeStopping = false;
      stateText.textContent = "STOPPED";
      startButton.disabled = !origin;
      audioApp.disabled = false;
      refreshAudio.disabled = false;
      stopButton.disabled = true;
      statusText.textContent = "Capture stopped. Offline session access may take about 30 seconds to expire.";
    }).catch(error => {
      statusText.textContent = `Stop failed: ${error.message || error}. Close the app to end capture.`;
      stopButton.disabled = false;
    });
    return;
  }
  const run = active;
  active = null;
  if (run) {
    clearTimeout(run.timer);
    clearTimeout(run.watchdog);
    run.stream?.getTracks().forEach(track => track.stop());
    for (const peer of run.peers.values()) removePeer(run, peer);
    if (run.socket?.readyState === WebSocket.OPEN) {
      run.socket.send(JSON.stringify({ type: "stop" }));
    }
    run.socket?.close();
  }
  urlField.value = "";
  codeField.value = "";
  viewerCount.textContent = "0";
  stateText.textContent = "STOPPED";
  statusText.textContent = message + (run?.socket && !confirmed
    ? " Offline session access may take about 30 seconds to expire." : "");
  startButton.disabled = !origin;
  stopButton.disabled = true;
}

function send(run, message) {
  if (active === run && run.socket.readyState === WebSocket.OPEN) {
    run.socket.send(JSON.stringify(message));
  }
}

function removePeer(run, peer) {
  if (!peer) return;
  run.peers.delete(peer.id);
  clearTimeout(peer.timer);
  peer.pc?.close();
}

function failPeer(run, peer, message) {
  if (active !== run || !peer || run.peers.get(peer.id) !== peer) return;
  removePeer(run, peer);
  send(run, { type: "disconnect-peer", peer: peer.id });
  statusText.textContent = message;
}

async function receive(run, message) {
  if (active !== run) return;
  if (message.type === "stopped") return stop("Sharing stopped. The session code is no longer valid.", true);
  if (message.type === "error") {
    if (message.peer !== undefined) {
      failPeer(run, run.peers.get(message.peer), "Viewer connection rejected. Capture continues; ask that viewer to reconnect with the same code.");
      return;
    }
    return stop(`Signaling rejected the request: ${message.error}. Capture stopped. Start a new session.`);
  }
  if (message.type === "started") {
    const link = new URL(message.url);
    if (link.origin !== origin || link.protocol !== "https:" || !message.session || !message.code) {
      throw new Error("Signaling returned invalid session details.");
    }
    clearTimeout(run.timer);
    run.started = true;
    urlField.value = link.href;
    codeField.value = message.code;
    stateText.textContent = "SHARING";
    viewerCount.textContent = String(message.viewers);
    statusText.textContent = "Sharing is live. Send the URL and code to up to four viewers.";
    return;
  }
  if (!run.started) throw new Error("Unexpected signaling response.");
  if (message.type === "viewer-left") {
    removePeer(run, run.peers.get(message.peer));
    viewerCount.textContent = String(message.viewers);
    return;
  }
  if (message.type === "viewer-joined") {
    if (typeof message.peer !== "string" || !message.peer || run.peers.has(message.peer)) return;
    viewerCount.textContent = String(message.viewers);
    const peer = { id: message.peer, pc: null, ice: [], queue: Promise.resolve() };
    run.peers.set(peer.id, peer);
    const pc = peer.pc = new RTCPeerConnection({ iceServers: [{ urls: "stun:stun.l.google.com:19302" }] });
    const current = () => active === run && run.peers.get(peer.id) === peer;
    const failed = () => failPeer(run, peer, "A viewer's direct connection failed. Restrictive NAT or firewall may block P2P; no TURN relay is available. Capture continues for other viewers; ask them to reconnect with the same code.");
    peer.timer = setTimeout(failed, 30000);
    pc.onconnectionstatechange = () => {
      if (!current()) return;
      if (pc.connectionState === "connected") clearTimeout(peer.timer);
      if (["failed", "disconnected"].includes(pc.connectionState)) failed();
    };
    pc.onicecandidate = event => {
      if (current() && event.candidate) send(run, { type: "ice", peer: message.peer, candidate: event.candidate.toJSON() });
    };
    run.stream.getTracks().forEach(track => pc.addTrack(track, run.stream));
    const offer = await pc.createOffer();
    if (!current()) return;
    await pc.setLocalDescription(offer);
    if (current()) send(run, { type: "offer", peer: message.peer, sdp: pc.localDescription.toJSON() });
    return;
  }
  const peer = run.peers.get(message.peer);
  if (!peer) return;
  const current = () => active === run && run.peers.get(peer.id) === peer;
  // Only negotiation is queued; stop and viewer-left take effect immediately.
  peer.queue = peer.queue.then(async () => {
    if (!current()) return;
    if (message.type === "answer") {
      if (peer.pc.signalingState !== "have-local-offer" || message.sdp?.type !== "answer") return;
      await peer.pc.setRemoteDescription(message.sdp);
      if (!current()) return;
      for (const candidate of peer.ice.splice(0)) {
        await peer.pc.addIceCandidate(candidate);
        if (!current()) return;
      }
    } else if (message.type === "ice" && message.candidate) {
      if (peer.pc.remoteDescription) await peer.pc.addIceCandidate(message.candidate);
      else peer.ice.push(message.candidate);
    }
  });
  return peer.queue;
}

startButton.addEventListener("click", async () => {
  if (native) {
    if (startButton.disabled) return;
    nativeStopping = false;
    startButton.disabled = true;
    stopButton.disabled = false;
    audioApp.disabled = true;
    refreshAudio.disabled = true;
    try { nativeStart = window.go.main.App.Start(audioApp.value); await nativeStart; }
    catch (error) {
      if (nativeStopping) return;
      startButton.disabled = false;
      stopButton.disabled = true;
      audioApp.disabled = false;
      refreshAudio.disabled = false;
      statusText.textContent = `Cannot start: ${error.message || error}`;
    }
    return;
  }
  if (active || !origin) return;
  const run = { peers: new Map(), stream: null, socket: null, started: false };
  active = run;
  startButton.disabled = true;
  stopButton.disabled = false;
  stateText.textContent = "CHOOSING";
  statusText.textContent = "Choose a screen with audio, or a window without audio.";
  try {
    // Keep this call in the click activation: backend config was loaded beforehand.
    const stream = await navigator.mediaDevices.getDisplayMedia({
      video: { width: { ideal: 1920 }, height: { ideal: 1080 }, frameRate: { ideal: 30 } },
      audio: true, systemAudio: "include", selfBrowserSurface: "exclude",
      surfaceSwitching: "exclude"
    });
    if (active !== run) {
      stream.getTracks().forEach(track => track.stop());
      return;
    }
    run.stream = stream;
    const video = stream.getVideoTracks()[0];
    const surface = video?.getSettings().displaySurface;
    if (!["monitor", "window"].includes(surface)) throw new Error("Choose a full screen or window, not a browser tab. Unknown capture sources are not supported.");
    if (surface === "window") {
      stream.getAudioTracks().forEach(track => { stream.removeTrack(track); track.stop(); });
    } else if (!stream.getAudioTracks().some(track => track.readyState === "live")) {
      throw new Error("Full-screen sharing requires system audio. Choose a screen and enable audio in the picker, or share a window instead.");
    }
    stream.getTracks().forEach(track => track.addEventListener("ended", () => {
      if (active === run) stop("Capture ended. Sharing stopped locally.");
    }));
    const supported = navigator.mediaDevices.getSupportedConstraints();
    const constraints = {};
    if (supported.width) constraints.width = { ideal: 1920, max: 1920 };
    if (supported.height) constraints.height = { ideal: 1080, max: 1080 };
    if (supported.frameRate) constraints.frameRate = { ideal: 30, max: 30 };
    await video.applyConstraints(constraints);
    if (active !== run) return;
    if (stream.getTracks().some(track => track.readyState !== "live")) throw new Error("Capture ended before sharing started.");
    stateText.textContent = "CONNECTING";
    statusText.textContent = "Opening a secure signaling connection...";
    const endpoint = new URL("/session", origin);
    endpoint.protocol = "wss:";
    const socket = run.socket = new WebSocket(endpoint);
    run.timer = setTimeout(() => { if (active === run) stop("Signaling timed out. Check your connection and try again."); }, 15000);
    const watchSignaling = () => {
      clearTimeout(run.watchdog);
      run.watchdog = setTimeout(() => {
        if (active === run) stop("Signaling stopped responding. Capture stopped; start again for a new session.");
      }, 35000);
    };
    socket.onopen = () => {
      if (active !== run) return;
      watchSignaling();
      send(run, { type: "start" });
    };
    socket.onmessage = event => {
      if (active !== run) return;
      watchSignaling();
      let message;
      try {
        message = JSON.parse(event.data);
        if (!message || typeof message !== "object") throw new Error("Invalid signaling response.");
      } catch (error) {
        stop(error.message);
        return;
      }
      if (message.type === "heartbeat") return;
      const pending = receive(run, message);
      const peer = run.peers.get(message.peer);
      pending.catch(error => {
        if (active !== run) return;
        if (message.peer !== undefined) {
          failPeer(run, peer, "Viewer negotiation failed. Capture continues for other viewers; ask that viewer to reconnect with the same code.");
          return;
        }
        stop(`Sharing failed: ${error.message}. Capture stopped.`);
      });
    };
    socket.onclose = socket.onerror = () => {
      if (active === run) stop("Signaling connection lost. All capture stopped; start again to create a new session.");
    };
  } catch (error) {
    if (active === run) stop(error.name === "NotAllowedError" ? "Capture was cancelled or denied. Nothing is being shared." : error.message);
  }
});

stopButton.addEventListener("click", () => stop());
window.addEventListener("pagehide", () => stop());

(async () => {
  try {
    const configured = await window.go.main.App.SignalingURL();
    const parsed = new URL(configured);
    if (parsed.protocol !== "https:" || parsed.username || parsed.password || parsed.pathname !== "/" || parsed.search || parsed.hash) throw new Error("SCREENSHARE_URL must be an HTTPS origin.");
    native = typeof window.go.main.App.Start === "function";
    if (native) {
      document.querySelector("#capture-help").textContent = "Choose an audio app below to share its sound with a screen or window. Without an app selection, screens share all system audio and windows share video only. Microphone audio is never captured.";
      document.querySelector("#app-audio").hidden = false;
      window.runtime.EventsOn("share-state", state => {
        if (nativeStopping) return;
        urlField.value = state.url;
        codeField.value = state.code;
        viewerCount.textContent = String(state.viewers);
        stateText.textContent = state.state.toUpperCase();
        startButton.disabled = state.state !== "stopped";
        stopButton.disabled = state.state === "stopped";
        audioApp.disabled = state.state !== "stopped";
        refreshAudio.disabled = state.state !== "stopped";
        statusText.textContent = state.error || state.notice || ({
          choosing: "Choose a screen or window. Cancel shares nothing.",
          connecting: "Starting native capture and secure signaling...",
          sharing: "Sharing is live. Send the URL and code to up to four viewers.",
          stopped: "Capture stopped. Offline session access may take about 30 seconds to expire."
        })[state.state];
      });
    } else if (!navigator.mediaDevices?.getDisplayMedia || !window.RTCPeerConnection) throw new Error("This WebView2 runtime does not support screen capture and WebRTC. Update WebView2 and try again.");
    origin = parsed.origin;
    startButton.disabled = false;
    statusText.textContent = "Ready. Nothing is shared until you choose a source.";
  } catch (error) {
    statusText.textContent = `Cannot start: ${error.message || error}`;
  }
})();
