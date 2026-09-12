"use strict";

// WASAPI produces 48 kHz stereo PCM. Keep latency bounded if the UI stalls.
class AppAudioProcessor extends AudioWorkletProcessor {
  constructor() {
    super();
    this.samples = new Int16Array(4800 * 2);
    this.read = 0;
    this.length = 0;
    this.playing = false;
    this.port.onmessage = ({ data }) => {
      if (!(data instanceof Int16Array) || data.length % 2) return;
      if (data.length > this.samples.length) data = data.subarray(data.length - this.samples.length);
      if (this.length + data.length > this.samples.length) {
        this.read = 0;
        this.length = 0;
        this.playing = false;
      }
      for (let i = 0; i < data.length; i++) {
        this.samples[(this.read + this.length + i) % this.samples.length] = data[i];
      }
      this.length += data.length;
    };
  }

  process(inputs, outputs) {
    const channels = outputs[0];
    if (channels.length !== 2) return true;
    if (!this.playing && this.length >= 960 * 2) this.playing = true;
    for (let i = 0; i < channels[0].length; i++) {
      for (let channel = 0; channel < 2; channel++) {
        channels[channel][i] = this.playing && this.length
          ? this.samples[this.read] / 32768 : 0;
        if (this.playing && this.length) {
          this.read = (this.read + 1) % this.samples.length;
          this.length--;
        }
      }
      if (!this.length) this.playing = false;
    }
    return true;
  }
}

registerProcessor("app-audio", AppAudioProcessor);
