const assert = require("node:assert/strict");
const { test } = require("node:test");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
const vm = require("node:vm");

const flush = () => new Promise(resolve => setImmediate(resolve));
function track(kind, surface) {
  return { kind, readyState: "live", getSettings: () => ({ displaySurface: surface }),
    applyConstraints: async () => {}, addEventListener() {}, stop() { this.readyState = "ended"; } };
}
function stream(tracks) {
  return { getTracks: () => [...tracks], getVideoTracks: () => tracks.filter(t => t.kind === "video"),
    getAudioTracks: () => tracks.filter(t => t.kind === "audio"), addTrack: t => tracks.push(t),
    removeTrack: t => tracks.splice(tracks.indexOf(t), 1) };
}

async function sharer({ surface = "window", selected = "42:123", browserAudio = true, startAudio, display } = {}) {
  const elements = new Map();
  const element = id => {
    if (!elements.has(id)) elements.set(id, { value: "", disabled: false, textContent: "", addEventListener(name, fn) { this[name] = fn; } });
    return elements.get(id);
  };
  const video = track("video", surface), audio = track("audio");
  const captured = stream(browserAudio ? [video, audio] : [video]);
  const events = new Map(), calls = [], contexts = [], sockets = [], nodes = [];
  class AudioContext {
    constructor() { this.sampleRate = 48000; this.state = "running"; this.audioWorklet = { addModule: async () => {} }; contexts.push(this); }
    async resume() {}
    async close() { this.state = "closed"; }
    createMediaStreamDestination() { return { stream: stream([track("audio")]) }; }
  }
  const sandbox = {
    document: { querySelector: element }, URL, AudioContext, Uint8Array, Int16Array, atob,
    crypto: { randomUUID: () => "capture-token" },
    AudioWorkletNode: class {
      constructor() { this.port = { postMessage: data => calls.push(["pcm", data]), close() {} }; nodes.push(this); }
      connect() {} disconnect() { this.disconnected = true; }
    },
    RTCPeerConnection: class {},
    WebSocket: class { static OPEN = 1; constructor() { this.readyState = 0; sockets.push(this); } close() { this.closed = true; } },
    setTimeout: () => 1, clearTimeout() {},
    navigator: { mediaDevices: {
      getDisplayMedia: options => { calls.push(["display", options]); return display ? display(captured) : Promise.resolve(captured); },
      getSupportedConstraints: () => ({})
    } },
    window: { addEventListener() {}, RTCPeerConnection: class {},
      go: { main: { App: {
        SignalingURL: async () => "https://share.example.com",
        StartAudio: async (...args) => { calls.push(["start", ...args]); if (startAudio) await startAudio(); },
        StopAudio: async () => { calls.push(["stop"]); }
      } } },
      runtime: { EventsOn(name, fn) { events.set(name, fn); return () => events.delete(name); } }
    }
  };
  vm.runInNewContext(readFileSync(join(__dirname, "sharer/sharer.js"), "utf8"), sandbox);
  await flush();
  element("#audio-app").value = selected;
  return { element, calls, captured, video, audio, events, contexts, sockets, nodes,
    start: () => element("#start").click(), stop: () => element("#stop").click() };
}

test("selected Windows app replaces browser audio and stops every resource", async () => {
  const s = await sharer();
  await s.start();
  assert.equal(s.calls[0][0], "display");
  assert.equal(s.calls[0][1].audio, false);
  assert.equal(s.calls[0][1].systemAudio, "exclude");
  assert.deepEqual(s.calls[1], ["start", "42:123", "capture-token"]);
  assert.equal(s.audio.readyState, "ended");
  assert.equal(s.captured.getAudioTracks().length, 1);
  assert.equal(s.sockets.length, 1);
  const receive = s.events.get("app-audio");
  receive({ token: "old-session", pcm: "AAA=" });
  assert.equal(s.calls.length, 2);
  receive({ token: "capture-token", pcm: "/38AgA==" });
  assert.deepEqual([...s.calls[2][1]], [32767, -32768]);
  s.stop();
  await flush();
  assert.equal(s.calls.at(-1)[0], "stop");
  assert.ok(s.captured.getTracks().every(t => t.readyState === "ended"));
  assert.equal(s.contexts[0].state, "closed");
  assert.equal(s.nodes[0].disconnected, true);
  assert.equal(s.events.size, 0);
  assert.equal(s.element("#start").disabled, false);
});

test("audio activation failure exposes no session and stops video", async () => {
  const s = await sharer({ startAudio: async () => { throw Error("app unavailable"); } });
  await s.start();
  await flush();
  assert.equal(s.sockets.length, 0);
  assert.equal(s.video.readyState, "ended");
  assert.match(s.element("#status").textContent, /app unavailable/);
  assert.equal(s.calls.at(-1)[0], "stop");
});

test("Stop while app audio starts waits for native cleanup", async () => {
  let resolve;
  const s = await sharer({ startAudio: () => new Promise(r => { resolve = r; }) });
  const starting = s.start();
  await flush();
  s.stop();
  assert.equal(s.element("#start").disabled, true);
  resolve();
  await starting;
  await flush();
  assert.equal(s.sockets.length, 0);
  assert.equal(s.calls.at(-1)[0], "stop");
  assert.equal(s.element("#start").disabled, false);
});

test("Stop during picker prevents late capture from starting app audio", async () => {
  let resolve;
  const s = await sharer({ display: captured => new Promise(r => { resolve = () => r(captured); }) });
  const starting = s.start();
  s.stop();
  resolve();
  await starting;
  await flush();
  assert.ok(s.captured.getTracks().every(t => t.readyState === "ended"));
  assert.ok(!s.calls.some(c => c[0] === "start"));
  assert.equal(s.sockets.length, 0);
});

test("native audio loss stops sharing; stale errors are ignored", async () => {
  const s = await sharer();
  await s.start();
  const error = s.events.get("app-audio-error");
  error({ token: "old", error: "closed" });
  assert.equal(s.video.readyState, "live");
  error({ token: "capture-token", error: "audio app closed" });
  await flush();
  assert.equal(s.video.readyState, "ended");
  assert.equal(s.sockets[0].closed, true);
  assert.match(s.element("#status").textContent, /audio app closed/);
});

test("default window remains silent; screens require audio unless app selected", async () => {
  const window = await sharer({ selected: "" });
  await window.start();
  assert.equal(window.captured.getAudioTracks().length, 0);
  assert.ok(!window.calls.some(c => c[0] === "start"));
  const screen = await sharer({ selected: "", surface: "monitor", browserAudio: false });
  await screen.start();
  assert.equal(screen.sockets.length, 0);
  assert.equal(screen.video.readyState, "ended");
  const appScreen = await sharer({ surface: "monitor", browserAudio: false });
  await appScreen.start();
  assert.equal(appScreen.sockets.length, 1);
  window.stop();
  appScreen.stop();
  await flush();
});

test("PCM worklet preserves stereo, outputs silence on starvation, bounds backlog", () => {
  let Processor;
  vm.runInNewContext(readFileSync(join(__dirname, "sharer/app-audio.js"), "utf8"), {
    Int16Array, AudioWorkletProcessor: class { constructor() { this.port = {}; } },
    registerProcessor: (_, type) => { Processor = type; }
  });
  const p = new Processor();
  const pcm = new Int16Array(1920);
  for (let i = 0; i < pcm.length; i += 2) { pcm[i] = 16384; pcm[i + 1] = -16384; }
  p.port.onmessage({ data: pcm });
  const output = [new Float32Array(1024), new Float32Array(1024)];
  p.process([], [output]);
  assert.equal(output[0][0], 0.5);
  assert.equal(output[1][959], -0.5);
  assert.equal(output[0][960], 0);
  p.port.onmessage({ data: new Int16Array(20000).fill(8192) });
  assert.ok(p.length <= 9600);
  p.process([], [output]);
  assert.equal(output[0][0], 0.25);
});
