//go:build linux && cgo

package sharer

import (
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

// Payloaders must map PTS to pipeline running time: perfect-rtptime=false,
// scale-rtptime=true, onvif-no-rate-control=false. Offsets and the pipeline
// clock/base time must remain valid for the capture's lifetime (no restarts).
type rtpClock struct {
	offset uint32
	rate   uint32
}

func (c *capture) newPeerConnection(config webrtc.Configuration) (*webrtc.PeerConnection, error) {
	media := &webrtc.MediaEngine{}
	if err := media.RegisterDefaultCodecs(); err != nil {
		return nil, err
	}
	registry := &interceptor.Registry{}
	// The default reporter retains its input writer, so correction must bind first.
	registry.Add(&clockReports{capture: c})
	if err := webrtc.RegisterDefaultInterceptors(media, registry); err != nil {
		return nil, err
	}
	return webrtc.NewAPI(webrtc.WithMediaEngine(media), webrtc.WithInterceptorRegistry(registry)).NewPeerConnection(config)
}

type clockReports struct {
	interceptor.NoOp
	capture *capture
	streams sync.Map // outbound SSRC -> rtpClock; each PeerConnection has its own bindings
}

func (r *clockReports) NewInterceptor(string) (interceptor.Interceptor, error) {
	return &clockReports{capture: r.capture}, nil
}

func (r *clockReports) BindLocalStream(info *interceptor.StreamInfo, writer interceptor.RTPWriter) interceptor.RTPWriter {
	if clock, ok := r.capture.clocks[info.MimeType]; ok && clock.rate == info.ClockRate {
		r.streams.Store(info.SSRC, clock)
	}
	return writer
}

func (r *clockReports) UnbindLocalStream(info *interceptor.StreamInfo) {
	r.streams.Delete(info.SSRC)
}

func (r *clockReports) BindRTCPWriter(writer interceptor.RTCPWriter) interceptor.RTCPWriter {
	return interceptor.RTCPWriterFunc(func(packets []rtcp.Packet, attributes interceptor.Attributes) (int, error) {
		wall, running, valid := r.capture.clockTime()
		corrected := make([]rtcp.Packet, 0, len(packets))
		for _, packet := range packets {
			if report, ok := packet.(*rtcp.SenderReport); ok {
				value, known := r.streams.Load(report.SSRC)
				if !valid || !known || report.PacketCount == 0 {
					continue
				}
				clock := value.(rtpClock)
				copy := *report
				copy.NTPTime = uint64(wall.Unix()+2208988800)<<32 | (uint64(wall.Nanosecond()) << 32 / uint64(time.Second))
				// Scale before reducing to 32 bits, without overflowing on long captures.
				copy.RTPTime = clock.offset + uint32(running/uint64(time.Second)*uint64(clock.rate)+
					running%uint64(time.Second)*uint64(clock.rate)/uint64(time.Second))
				packet = &copy
			}
			corrected = append(corrected, packet)
		}
		if len(corrected) == 0 {
			return 0, nil
		}
		return writer.Write(corrected, attributes)
	})
}
