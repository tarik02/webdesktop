package webrtc

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	pion "github.com/pion/webrtc/v4"
	"github.com/tarik02/webdesktop/media"
)

const (
	outboundMTU   = 1200
	payloaderOpus = "opus"
)

type trackBinding struct {
	id     string
	writer pion.TrackLocalWriter
}

type sampleTrack struct {
	capability pion.RTPCodecCapability
	payloader  string
	kind       pion.RTPCodecType
	id         string
	streamID   string

	mu         sync.RWMutex
	binding    *trackBinding
	packetizer rtp.Packetizer
	remainder  float64
}

func newSampleTrack(
	capability pion.RTPCodecCapability,
	payloader string,
	kind pion.RTPCodecType,
	id string,
	streamID string,
) *sampleTrack {
	return &sampleTrack{
		capability: capability,
		payloader:  payloader,
		kind:       kind,
		id:         id,
		streamID:   streamID,
	}
}

func (t *sampleTrack) Bind(context pion.TrackLocalContext) (pion.RTPCodecParameters, error) {
	var negotiated pion.RTPCodecParameters
	found := false
	for _, codec := range context.CodecParameters() {
		if strings.EqualFold(codec.MimeType, t.capability.MimeType) &&
			codec.ClockRate == t.capability.ClockRate &&
			codec.Channels == t.capability.Channels {
			negotiated = codec
			found = true
			break
		}
	}
	if !found {
		return pion.RTPCodecParameters{}, pion.ErrUnsupportedCodec
	}

	var payloader rtp.Payloader
	switch t.payloader {
	case media.PayloaderVP8:
		payloader = &codecs.VP8Payloader{}
	case media.PayloaderH264:
		payloader = &codecs.H264Payloader{}
	case payloaderOpus:
		payloader = &codecs.OpusPayloader{}
	default:
		return pion.RTPCodecParameters{}, fmt.Errorf("unsupported RTP payloader %q", t.payloader)
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
	return t.kind
}

func (t *sampleTrack) WriteSample(data []byte, duration time.Duration) (uint32, error) {
	if duration <= 0 {
		return 0, errors.New("RTP sample duration must be positive")
	}

	t.mu.Lock()
	binding := t.binding
	packetizer := t.packetizer
	if binding == nil || packetizer == nil {
		t.mu.Unlock()
		return 0, nil
	}
	total := duration.Seconds()*float64(t.capability.ClockRate) + t.remainder
	ticks := uint32(total)
	t.remainder = total - float64(ticks)
	packets := packetizer.Packetize(data, ticks)
	t.mu.Unlock()
	for _, packet := range packets {
		if _, err := binding.writer.WriteRTP(&packet.Header, packet.Payload); err != nil {
			return 0, err
		}
	}
	return ticks, nil
}

func (t *sampleTrack) WriteSampleAt(data []byte, timestampAdvance time.Duration) (uint64, error) {
	if timestampAdvance < 0 {
		return 0, errors.New("RTP timestamp advance must not be negative")
	}

	t.mu.Lock()
	binding := t.binding
	packetizer := t.packetizer
	if binding == nil || packetizer == nil {
		t.mu.Unlock()
		return 0, nil
	}

	clockRate := uint64(t.capability.ClockRate)
	wholeTicks := uint64(timestampAdvance/time.Second) * clockRate
	fraction := float64(timestampAdvance%time.Second)*float64(clockRate)/float64(time.Second) + t.remainder
	fractionTicks := uint64(fraction)
	ticks := wholeTicks + fractionTicks
	t.remainder = fraction - float64(fractionTicks)
	if advance := uint32(ticks); advance > 0 {
		packetizer.SkipSamples(advance)
	}
	packets := packetizer.Packetize(data, 0)
	t.mu.Unlock()
	for _, packet := range packets {
		if _, err := binding.writer.WriteRTP(&packet.Header, packet.Payload); err != nil {
			return 0, err
		}
	}
	return ticks, nil
}
