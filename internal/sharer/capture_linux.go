//go:build linux && cgo

package sharer

/*
#cgo pkg-config: gstreamer-app-1.0
#include <stdlib.h>
#include <gst/gst.h>
#include <gst/app/gstappsink.h>

static GstElement *capture_new(const char *description, char **error) {
    gst_init(NULL, NULL);
    GError *err = NULL;
    GstElement *pipeline = gst_parse_launch(description, &err);
    if (err) {
        *error = g_strdup(err->message);
        g_error_free(err);
        if (pipeline) gst_object_unref(pipeline);
        return NULL;
    }
    if (!pipeline || gst_element_set_state(pipeline, GST_STATE_PLAYING) == GST_STATE_CHANGE_FAILURE) {
        *error = g_strdup("Capture could not start. Check PipeWire and GStreamer plugins.");
        if (pipeline) {
            gst_element_set_state(pipeline, GST_STATE_NULL);
            gst_object_unref(pipeline);
        }
        return NULL;
    }
    return pipeline;
}

static void capture_close(GstElement *pipeline) {
    gst_element_set_state(pipeline, GST_STATE_NULL);
    gst_object_unref(pipeline);
}

static void capture_bitrate(GstElement *pipeline, int bitrate) {
    GstElement *encoder = gst_bin_get_by_name(GST_BIN(pipeline), "video_encoder");
    if (encoder) {
        g_object_set(encoder, "target-bitrate", bitrate, NULL);
        gst_object_unref(encoder);
    }
}

static GstElement *capture_sink(GstElement *pipeline, const char *name, int *invalid) {
    GstElement *sink = GST_IS_BIN(pipeline) ? gst_bin_get_by_name(GST_BIN(pipeline), name) : NULL;
    if (sink && !GST_IS_APP_SINK(sink)) {
        *invalid = 1;
        gst_object_unref(sink);
        return NULL;
    }
    return sink;
}

static char *capture_error(GstElement *pipeline) {
    GstBus *bus = gst_element_get_bus(pipeline);
    if (!bus) return NULL;
    GstMessage *msg = gst_bus_pop_filtered(bus, GST_MESSAGE_ERROR | GST_MESSAGE_EOS);
    gst_object_unref(bus);
    if (!msg) return NULL;
    char *text;
    if (GST_MESSAGE_TYPE(msg) == GST_MESSAGE_ERROR) {
        GError *err = NULL;
        gst_message_parse_error(msg, &err, NULL);
        text = g_strdup(err ? err->message : "Capture failed");
        if (err) g_error_free(err);
    } else {
        text = g_strdup("Capture ended");
    }
    gst_message_unref(msg);
    return text;
}

static guint64 capture_running_time(GstElement *pipeline) {
    GstClock *clock = gst_element_get_clock(pipeline);
    if (!clock) return GST_CLOCK_TIME_NONE;
    GstClockTime now = gst_clock_get_time(clock);
    GstClockTime base = gst_element_get_base_time(pipeline);
    gst_object_unref(clock);
    return GST_CLOCK_TIME_IS_VALID(base) && now >= base ? now - base : GST_CLOCK_TIME_NONE;
}

static int capture_pull(GstElement *sink, void **data, gsize *size, guint *offset, gint *rate) {
    GstSample *sample = gst_app_sink_try_pull_sample(GST_APP_SINK(sink), 100 * GST_MSECOND);
    if (!sample) return gst_app_sink_is_eos(GST_APP_SINK(sink)) ? -2 : 0;
    if (offset) {
        GstCaps *caps = gst_sample_get_caps(sample);
        const GstStructure *s = caps && gst_caps_get_size(caps) ? gst_caps_get_structure(caps, 0) : NULL;
        if (!s || !gst_structure_get_uint(s, "timestamp-offset", offset) ||
            !gst_structure_get_int(s, "clock-rate", rate) || *rate <= 0) {
            gst_sample_unref(sample);
            return -3;
        }
    }
    GstBuffer *buffer = gst_sample_get_buffer(sample);
    if (buffer && gst_buffer_get_size(buffer) <= G_MAXINT)
        gst_buffer_extract_dup(buffer, 0, gst_buffer_get_size(buffer), data, size);
    gst_sample_unref(sample);
    return *data && *size ? 1 : -1;
}
*/
import "C"

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

type capture struct {
	pipeline  *C.GstElement
	sinks     []*C.GstElement
	tracks    []*webrtc.TrackLocalStaticRTP
	first     []*rtp.Packet
	clocks    map[string]rtpClock
	busMu     sync.Mutex
	busErr    error
	controlMu sync.Mutex
	closed    bool
}

func openCapture(ctx context.Context, description string) (*capture, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	text := C.CString(description)
	defer C.free(unsafe.Pointer(text))
	var failure *C.char
	pipeline := C.capture_new(text, &failure)
	if pipeline == nil {
		defer C.g_free(C.gpointer(failure))
		return nil, fmt.Errorf("capture: %s", C.GoString(failure))
	}
	c := &capture{pipeline: pipeline, clocks: make(map[string]rtpClock)}
	ready := false
	defer func() {
		if !ready {
			c.close()
		}
	}()
	for _, name := range []string{"video", "audio"} {
		text := C.CString(name)
		var invalid C.int
		sink := C.capture_sink(pipeline, text, &invalid)
		C.free(unsafe.Pointer(text))
		if invalid != 0 || (sink == nil && name == "video") {
			return nil, fmt.Errorf("capture: %s must be an appsink", name)
		}
		if sink == nil {
			continue
		}
		c.sinks = append(c.sinks, sink)
		codec := webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}
		if name == "audio" {
			codec = webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2}
		}
		track, err := webrtc.NewTrackLocalStaticRTP(codec, name, "screen")
		if err != nil {
			return nil, fmt.Errorf("capture: %w", err)
		}
		c.tracks = append(c.tracks, track)
	}
	for i, sink := range c.sinks {
		var clock rtpClock
		packet, err := c.read(ctx, sink, &clock)
		if err != nil {
			return nil, fmt.Errorf("capture: %s readiness: %w", c.tracks[i].ID(), err)
		}
		codec := c.tracks[i].Codec()
		if clock.rate != codec.ClockRate {
			return nil, fmt.Errorf("capture: %s clock rate must be %d", c.tracks[i].ID(), codec.ClockRate)
		}
		c.clocks[codec.MimeType] = clock
		c.first = append(c.first, packet)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ready = true
	return c, nil
}

func (c *capture) read(ctx context.Context, sink *C.GstElement, clock *rtpClock) (*rtp.Packet, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Bus messages are consumed once; preserve failure for every reader.
		c.busMu.Lock()
		if c.busErr == nil {
			if text := C.capture_error(c.pipeline); text != nil {
				c.busErr = fmt.Errorf("capture: %s", C.GoString(text))
				C.g_free(C.gpointer(text))
			}
		}
		err := c.busErr
		c.busMu.Unlock()
		if err != nil {
			return nil, err
		}
		var data unsafe.Pointer
		var size C.gsize
		var offset C.guint
		var rate C.gint
		var offsetPtr *C.guint
		if clock != nil {
			offsetPtr = &offset
		}
		status := C.capture_pull(sink, &data, &size, offsetPtr, &rate)
		var bytes []byte
		if status == 1 {
			bytes = C.GoBytes(data, C.int(size))
		}
		C.g_free(C.gpointer(data))
		switch status {
		case 0:
			continue
		case -2:
			return nil, fmt.Errorf("capture: appsink ended")
		case -1:
			return nil, fmt.Errorf("capture: unreadable RTP buffer")
		case -3:
			return nil, fmt.Errorf("capture: RTP caps require timestamp-offset and clock-rate")
		}
		packet := &rtp.Packet{}
		if err := packet.Unmarshal(bytes); err != nil {
			return nil, fmt.Errorf("capture: invalid RTP: %w", err)
		}
		if packet.Version != 2 || len(packet.Payload) == 0 {
			return nil, fmt.Errorf("capture: invalid RTP media packet")
		}
		if clock != nil {
			*clock = rtpClock{offset: uint32(offset), rate: uint32(rate)}
		}
		return packet, nil
	}
}

// pump blocks until cancellation or capture/write failure, joining all readers.
// Call once, after attaching tracks; close must wait for pump to return.
func (c *capture) pump(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	failures := make(chan error, len(c.sinks))
	for i, sink := range c.sinks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			packet := c.first[i]
			var err error
			for ctx.Err() == nil {
				// Pion still delivers to healthy bindings when another peer's write
				// fails. ICE state/negotiation timers remove failed peers separately.
				_ = c.tracks[i].WriteRTP(packet)
				packet, err = c.read(ctx, sink, nil)
				if err != nil {
					break
				}
			}
			if err != nil {
				failures <- fmt.Errorf("capture: %s: %w", c.tracks[i].ID(), err)
				cancel()
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-failures:
		return err
	default:
		return ctx.Err()
	}
}

// close releases native resources; never call concurrently with pump.
func (c *capture) close() {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	c.closed = true
	C.capture_close(c.pipeline)
	for _, sink := range c.sinks {
		C.gst_object_unref(C.gpointer(sink))
	}
}

func (c *capture) bitrate(value uint64) {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	if !c.closed {
		C.capture_bitrate(c.pipeline, C.int(max(200000, min(4000000, value))))
	}
}

func (c *capture) clockTime() (time.Time, uint64, bool) {
	c.controlMu.Lock()
	defer c.controlMu.Unlock()
	if c.closed {
		return time.Time{}, 0, false
	}
	before := time.Now()
	running := uint64(C.capture_running_time(c.pipeline))
	// Pair the native clock read with the midpoint of its wall-clock interval.
	wall := before.Add(time.Since(before) / 2)
	return wall, running, running != ^uint64(0)
}
