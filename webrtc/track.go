package webrtc

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	pion "github.com/pion/webrtc/v4"
)

const outboundMTU = 1200

type trackBinding struct {
	id     string
	writer pion.TrackLocalWriter
}

type sampleTrack struct {
	capability pion.RTPCodecCapability
	id         string
	streamID   string

	mu         sync.RWMutex
	binding    *trackBinding
	packetizer rtp.Packetizer
	remainder  float64
}

func newSampleTrack(capability pion.RTPCodecCapability, id, streamID string) *sampleTrack {
	return &sampleTrack{
		capability: capability,
		id:         id,
		streamID:   streamID,
	}
}

func (t *sampleTrack) Bind(context pion.TrackLocalContext) (pion.RTPCodecParameters, error) {
	var negotiated pion.RTPCodecParameters
	found := false
	for _, codec := range context.CodecParameters() {
		if strings.EqualFold(codec.MimeType, t.capability.MimeType) &&
			codec.ClockRate == t.capability.ClockRate {
			negotiated = codec
			found = true
			break
		}
	}
	if !found {
		return pion.RTPCodecParameters{}, pion.ErrUnsupportedCodec
	}

	var payloader rtp.Payloader = &codecs.VP8Payloader{}
	if strings.EqualFold(t.capability.MimeType, pion.MimeTypeH264) {
		payloader = &codecs.H264Payloader{}
	}

	t.mu.Lock()
	t.binding = &trackBinding{
		id:     context.ID(),
		writer: context.WriteStream(),
	}
	t.packetizer = rtp.NewPacketizer(
		outboundMTU,
		uint8(negotiated.PayloadType),
		uint32(context.SSRC()),
		payloader,
		rtp.NewRandomSequencer(),
		t.capability.ClockRate,
	)
	t.remainder = 0
	t.mu.Unlock()
	return negotiated, nil
}

func (t *sampleTrack) Unbind(context pion.TrackLocalContext) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.binding == nil || t.binding.id != context.ID() {
		return pion.ErrUnbindFailed
	}
	t.binding = nil
	return nil
}

func (t *sampleTrack) ID() string {
	return t.id
}

func (t *sampleTrack) RID() string {
	return ""
}

func (t *sampleTrack) StreamID() string {
	return t.streamID
}

func (t *sampleTrack) Kind() pion.RTPCodecType {
	return pion.RTPCodecTypeVideo
}

func (t *sampleTrack) WriteSample(data []byte, duration time.Duration) error {
	if duration <= 0 {
		return errors.New("RTP sample duration must be positive")
	}

	t.mu.Lock()
	binding := t.binding
	packetizer := t.packetizer
	if binding == nil || packetizer == nil {
		t.mu.Unlock()
		return nil
	}

	total := duration.Seconds()*float64(t.capability.ClockRate) + t.remainder
	ticks := uint32(total)
	t.remainder = total - float64(ticks)
	packets := packetizer.Packetize(data, ticks)
	t.mu.Unlock()
	for _, packet := range packets {
		if _, err := binding.writer.WriteRTP(&packet.Header, packet.Payload); err != nil {
			return err
		}
	}
	return nil
}
