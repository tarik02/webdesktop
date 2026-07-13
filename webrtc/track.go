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
	id         string
	writer     pion.TrackLocalWriter
	packetizer rtp.Packetizer
	remainder  uint64
	writeMu    sync.Mutex
}

type sampleTrack struct {
	capability pion.RTPCodecCapability
	kind       pion.RTPCodecType
	id         string
	streamID   string

	mu       sync.RWMutex
	bindings map[string]*trackBinding
}

func newSampleTrack(capability pion.RTPCodecCapability, kind pion.RTPCodecType, id string, streamID string) *sampleTrack {
	return &sampleTrack{capability: capability, kind: kind, id: id, streamID: streamID, bindings: make(map[string]*trackBinding)}
}

func (t *sampleTrack) Bind(context pion.TrackLocalContext) (pion.RTPCodecParameters, error) {
	var negotiated pion.RTPCodecParameters
	found := false
	for _, codec := range context.CodecParameters() {
		if strings.EqualFold(codec.MimeType, t.capability.MimeType) && codec.ClockRate == t.capability.ClockRate && codec.Channels == t.capability.Channels {
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
	} else if strings.EqualFold(t.capability.MimeType, pion.MimeTypeOpus) {
		payloader = &codecs.OpusPayloader{}
	}

	binding := &trackBinding{
		id: context.ID(), writer: context.WriteStream(),
		packetizer: rtp.NewPacketizer(outboundMTU, uint8(negotiated.PayloadType), uint32(context.SSRC()), payloader, rtp.NewRandomSequencer(), t.capability.ClockRate),
	}
	t.mu.Lock()
	t.bindings[binding.id] = binding
	t.mu.Unlock()
	return negotiated, nil
}

func (t *sampleTrack) Unbind(context pion.TrackLocalContext) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.bindings[context.ID()]; !ok {
		return pion.ErrUnbindFailed
	}
	delete(t.bindings, context.ID())
	return nil
}

func (t *sampleTrack) ID() string              { return t.id }
func (t *sampleTrack) RID() string             { return "" }
func (t *sampleTrack) StreamID() string        { return t.streamID }
func (t *sampleTrack) Kind() pion.RTPCodecType { return t.kind }

func (t *sampleTrack) WriteSample(data []byte, timestampAdvance time.Duration) (uint32, error) {
	if timestampAdvance < 0 {
		return 0, errors.New("RTP timestamp advance must not be negative")
	}
	t.mu.RLock()
	bindings := make([]*trackBinding, 0, len(t.bindings))
	for _, binding := range t.bindings {
		bindings = append(bindings, binding)
	}
	t.mu.RUnlock()
	var firstErr error
	var firstTicks uint32
	for index, binding := range bindings {
		binding.writeMu.Lock()
		ticks := t.durationToTicks(binding, timestampAdvance)
		if ticks > 0 {
			binding.packetizer.SkipSamples(ticks)
		}
		packets := binding.packetizer.Packetize(data, 0)
		for _, packet := range packets {
			if _, err := binding.writer.WriteRTP(&packet.Header, packet.Payload); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		binding.writeMu.Unlock()
		if index == 0 {
			firstTicks = ticks
		}
	}
	return firstTicks, firstErr
}

func (t *sampleTrack) durationToTicks(binding *trackBinding, duration time.Duration) uint32 {
	const nanosPerSecond = uint64(time.Second)
	maxTicks := uint64(^uint32(0))
	seconds := uint64(duration / time.Second)
	nanoseconds := uint64(duration % time.Second)
	wholeTicks := seconds * uint64(t.capability.ClockRate)
	if wholeTicks >= maxTicks {
		binding.remainder = 0
		return uint32(maxTicks)
	}
	fractional := nanoseconds*uint64(t.capability.ClockRate) + binding.remainder
	ticks := wholeTicks + fractional/nanosPerSecond
	binding.remainder = fractional % nanosPerSecond
	if ticks > maxTicks {
		binding.remainder = 0
		return uint32(maxTicks)
	}
	return uint32(ticks)
}
