"use strict";

const form = document.querySelector("#join-form");
const codeField = document.querySelector("#code");
const joinButton = document.querySelector("#join");
const reconnectButton = document.querySelector("#reconnect");
const playButton = document.querySelector("#play");
const video = document.querySelector("#video");
const waiting = document.querySelector("#waiting");
const statusText = document.querySelector("#status");
const stateText = document.querySelector("#state");
const session = new URLSearchParams(location.search).get("session");
const natMessage = "Direct connection failed. A restrictive NAT or firewall may block P2P; this app has no TURN relay. Try another network, then reconnect with the same code.";
let active;
let lastCode = "";

function cleanup(message, retry = true) {
  const run = active;
  active = null;
  if (run) {
    clearTimeout(run.timer);
    clearTimeout(run.watchdog);
    run.pc?.close();
    run.socket.close();
  }
  video.srcObject?.getTracks().forEach(track => track.stop());
  video.srcObject = null;
  playButton.hidden = true;
  waiting.hidden = false;
  joinButton.disabled = !session;
  codeField.disabled = false;
  reconnectButton.hidden = !retry || !lastCode;
  stateText.textContent = "DISCONNECTED";
  statusText.textContent = message;
}

function send(run, message) {
  if (active === run && run.socket.readyState === WebSocket.OPEN) run.socket.send(JSON.stringify(message));
}

async function receive(run, message) {
  if (active !== run) return;
  if (message.type === "stopped") {
    cleanup("Sharing stopped. This session code is no longer valid.", false);
    return;
  }
  if (message.type === "error") {
    const errors = {
      "invalid-code": "Code is incorrect or the session has ended. Check the URL and code with your friend.",
      "session-full": "Session full. Four viewers are already connected. Reconnect when someone leaves.",
      "connection-failed": "Your direct connection failed. Sharing is still active; reconnect with the same code. If it fails again, try another network; no TURN relay is available.",
      "invalid-message": "Signaling rejected the request. Check that app and server versions match.",
      "not-joined": "You are no longer joined. Reconnect to try again.",
      "already-joined": "This connection is already joined. Reconnect to start fresh."
    };
    cleanup(errors[message.error] || "Signaling failed. Reconnect to try again.");
    return;
  }
  if (message.type === "joined") {
    if (run.pc || typeof message.peer !== "string" || !message.peer) throw new Error("Unexpected join response.");
    run.peer = message.peer;
    const pc = run.pc = new RTCPeerConnection({ iceServers: [{ urls: "stun:stun.l.google.com:19302" }] });
    const current = () => active === run && run.pc === pc && run.peer === message.peer;
    stateText.textContent = "CONNECTING";
    statusText.textContent = "Code accepted. Connecting directly to the shared screen...";
    clearTimeout(run.timer);
    run.timer = setTimeout(() => { if (current()) cleanup(natMessage); }, 30000);
    pc.onicecandidate = event => {
      if (current() && event.candidate) send(run, { type: "ice", peer: run.peer, candidate: event.candidate.toJSON() });
    };
    pc.ontrack = event => {
      if (!current()) { event.track.stop(); return; }
      if (!video.srcObject) video.srcObject = new MediaStream();
      video.srcObject.addTrack(event.track);
      event.track.onended = () => { if (current()) cleanup("Shared media ended. Reconnect if sharing is still active."); };
      video.play().catch(() => { if (current()) playButton.hidden = false; });
    };
    pc.onconnectionstatechange = () => {
      if (!current()) return;
      if (pc.connectionState === "connected") {
        clearTimeout(run.timer);
        stateText.textContent = "LIVE";
        statusText.textContent = "Connected directly. Use the player controls for volume or full screen.";
      } else if (["failed", "disconnected"].includes(pc.connectionState)) cleanup(natMessage);
    };
    return;
  }
  if (!run.pc || message.peer !== run.peer) return;
  const pc = run.pc;
  const peer = run.peer;
  const current = () => active === run && run.pc === pc && run.peer === peer;
  if (message.type === "offer") {
    if (pc.remoteDescription || message.sdp?.type !== "offer") return;
    await pc.setRemoteDescription(message.sdp);
    if (!current()) return;
    for (const candidate of run.ice.splice(0)) {
      await pc.addIceCandidate(candidate);
      if (!current()) return;
    }
    const answer = await pc.createAnswer();
    if (!current()) return;
    await pc.setLocalDescription(answer);
    if (current()) send(run, { type: "answer", peer, sdp: pc.localDescription.toJSON() });
  } else if (message.type === "ice" && message.candidate) {
    if (pc.remoteDescription) await pc.addIceCandidate(message.candidate);
    else run.ice.push(message.candidate);
  }
}

function join(code) {
  cleanup("Opening a secure signaling connection...", false);
  lastCode = code;
  joinButton.disabled = true;
  codeField.disabled = true;
  stateText.textContent = "JOINING";
  try {
    const endpoint = new URL("/session", location.origin);
    endpoint.protocol = "wss:";
    const socket = new WebSocket(endpoint);
    const run = active = { socket, pc: null, peer: null, ice: [] };
    run.timer = setTimeout(() => { if (active === run) cleanup("Signaling timed out. Check your connection and reconnect."); }, 15000);
    const watchSignaling = () => {
      clearTimeout(run.watchdog);
      run.watchdog = setTimeout(() => {
        if (active === run) cleanup("Signaling stopped responding. Media stopped; reconnect if sharing is still active.");
      }, 35000);
    };
    socket.onopen = () => {
      if (active !== run) return;
      watchSignaling();
      send(run, { type: "join", session, code });
    };
    let queue = Promise.resolve();
    socket.onmessage = event => {
      if (active !== run) return;
      watchSignaling();
      let message;
      try {
        message = JSON.parse(event.data);
        if (!message || typeof message !== "object") throw new Error("Invalid signaling response.");
      } catch (error) {
        cleanup(error.message);
        return;
      }
      if (message.type === "heartbeat") return;
      // Session termination must not wait for pending WebRTC promises.
      const task = ["stopped", "error"].includes(message.type)
        ? receive(run, message)
        : (queue = queue.then(() => receive(run, message)));
      task.catch(() => { if (active === run) cleanup("Could not negotiate a direct connection. Reconnect; if it still fails, check your browser and network."); });
    };
    socket.onclose = socket.onerror = () => {
      if (active === run) cleanup("Signaling connection lost. Media stopped. Reconnect with the same code if sharing is still active.");
    };
  } catch (error) {
    cleanup(`Cannot connect: ${error.message}`);
  }
}

form.addEventListener("submit", event => {
  event.preventDefault();
  if (session && form.reportValidity()) join(codeField.value.trim());
});
reconnectButton.addEventListener("click", () => { if (lastCode) join(lastCode); });
playButton.addEventListener("click", () => {
  const run = active;
  video.muted = false;
  video.volume = 1;
  video.play().then(() => { if (active === run) playButton.hidden = true; }).catch(() => {
    if (active === run) statusText.textContent = "Playback was blocked. Use the video player's play and volume controls.";
  });
});
video.addEventListener("playing", () => { if (active) { waiting.hidden = true; playButton.hidden = true; } });
window.addEventListener("pagehide", () => cleanup("Disconnected.", false));

if (!session || location.protocol !== "https:" || !window.RTCPeerConnection) {
  joinButton.disabled = true;
  codeField.disabled = true;
  statusText.textContent = !session ? "Open the full viewer URL your friend sent; its session ID is missing." : "Use the HTTPS viewer URL in desktop Chrome, Edge or Firefox with WebRTC enabled.";
}
