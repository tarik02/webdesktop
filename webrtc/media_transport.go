package webrtc

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	pion "github.com/pion/webrtc/v4"
	"github.com/tarik02/webdesktop/media"
	"go.uber.org/zap"
)

type videoPutResult struct {
	accepted bool
	replaced bool
}

type videoMailbox struct {
	mu      sync.Mutex
	pending *media.Sample
	wake    chan struct{}
}

func newVideoMailbox() videoMailbox { return videoMailbox{wake: make(chan struct{}, 1)} }
func (m *videoMailbox) put(sample media.Sample) videoPutResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending != nil && m.pending.KeyFrame && !sample.KeyFrame {
		return videoPutResult{}
	}
	replaced := m.pending != nil
	m.pending = &sample
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return videoPutResult{accepted: true, replaced: replaced}
}
func (m *videoMailbox) take(ctx context.Context) (media.Sample, bool) {
	for {
		select {
		case <-ctx.Done():
			return media.Sample{}, false
		case <-m.wake:
			m.mu.Lock()
			if m.pending != nil {
				sample := *m.pending
				m.pending = nil
				m.mu.Unlock()
				return sample, true
			}
			m.mu.Unlock()
		}
	}
}
func (m *videoMailbox) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		return 0
	}
	return 1
}

const audioMailboxCapacity = 4

type audioMailbox struct {
	mu    sync.Mutex
	queue []media.AudioSample
	wake  chan struct{}
}

func newAudioMailbox() audioMailbox { return audioMailbox{wake: make(chan struct{}, 1)} }
func (m *audioMailbox) signalLocked() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *audioMailbox) put(sample media.AudioSample) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	dropped := 0
	if len(m.queue) == audioMailboxCapacity {
		copy(m.queue, m.queue[1:])
		m.queue[len(m.queue)-1] = sample
		dropped = 1
	} else {
		m.queue = append(m.queue, sample)
	}
	m.signalLocked()
	return dropped
}
func (m *audioMailbox) take(ctx context.Context) (media.AudioSample, bool) {
	for {
		select {
		case <-ctx.Done():
			return media.AudioSample{}, false
		case <-m.wake:
			m.mu.Lock()
			if len(m.queue) > 0 {
				sample := m.queue[0]
				m.queue = m.queue[1:]
				if len(m.queue) > 0 {
					m.signalLocked()
				}
				m.mu.Unlock()
				return sample, true
			}
			m.mu.Unlock()
		}
	}
}
func (m *audioMailbox) len() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.queue) }

func positiveDiscontinuityDuration(duration time.Duration, clockRate uint32) time.Duration {
	if duration > 0 {
		return duration
	}
	return time.Second / time.Duration(clockRate)
}

func (p *peer) readRTCP(sender *pion.RTPSender, video bool) {
	for {
		packets, _, err := sender.ReadRTCP()
		if err != nil {
			if !errors.Is(err, io.EOF) &&
				!errors.Is(err, io.ErrClosedPipe) &&
				p.ctx.Err() == nil {
				p.logger.Debug("RTCP reader stopped",
					zap.Bool("video", video),
					zap.Error(err),
				)
			}
			return
		}
		if !video {
			continue
		}
		for _, packet := range packets {
			switch typed := packet.(type) {
			case *rtcp.PictureLossIndication:
				p.keyframeRequests.Add(1)
				p.service.requestKeyframe("pli")
			case *rtcp.FullIntraRequest:
				p.keyframeRequests.Add(1)
				p.service.requestKeyframe("fir")
			case *rtcp.TransportLayerNack:
				p.videoNACKReports.Add(1)
				var packets uint64
				for index := range typed.Nacks {
					packets += uint64(len(typed.Nacks[index].PacketList()))
				}
				p.videoNACKPackets.Add(packets)
			case *rtcp.ReceiverReport:
				if len(typed.Reports) > 0 {
					report := typed.Reports[0]
					p.videoFractionLost.Store(uint32(report.FractionLost))
					p.videoTotalLost.Store(report.TotalLost)
					p.videoJitter.Store(report.Jitter)
					p.videoReportSeen.Store(true)
				}
			}
		}
	}
}

func (p *peer) enqueueVideo(sample media.Sample) {
	if p.isClosing() || !p.connected.Load() || p.ctx.Err() != nil {
		return
	}
	p.videoSamplesSeen.Add(1)
	if p.videoNeedsKeyframe.Load() && !sample.KeyFrame {
		p.videoSamplesDropped.Add(1)
		return
	}
	result := p.videoSamples.put(sample)
	if result.accepted {
		p.videoSamplesEnqueued.Add(1)
		if result.replaced {
			p.videoSamplesDropped.Add(1)
		}
		if sample.KeyFrame {
			p.videoNeedsKeyframe.Store(false)
		}
	} else {
		p.videoSamplesDropped.Add(1)
	}
}

func (p *peer) enqueueAudio(sample media.AudioSample) {
	if p.isClosing() || !p.connected.Load() || p.ctx.Err() != nil {
		return
	}
	p.audioSamplesSeen.Add(1)
	dropped := p.audioSamples.put(sample)
	p.audioSamplesEnqueued.Add(1)
	p.audioSamplesDropped.Add(uint64(dropped))
}

func (p *peer) writeVideoSamples() {
	var origin time.Time
	var previousProducedAt time.Time
	var previousPTS time.Duration
	var firstPTS time.Duration
	var rtpAnchorTicks uint64
	var rtpTicksTotal uint64
	havePreviousPTS := false
	for {
		sample, ok := p.videoSamples.take(p.ctx)
		if !ok {
			return
		}
		if origin.IsZero() {
			origin = sample.ProducedAt
		}
		producedElapsed := sample.ProducedAt.Sub(origin)
		var productionGap time.Duration
		if !previousProducedAt.IsZero() {
			productionGap = sample.ProducedAt.Sub(previousProducedAt)
		}
		ptsRegressed := havePreviousPTS && sample.PTS <= previousPTS
		if ptsRegressed {
			p.videoPTSRegressions.Add(1)
			if !sample.KeyFrame {
				p.videoNeedsKeyframe.Store(true)
				p.videoSamplesDropped.Add(1)
				continue
			}
		}
		timestampAdvance := time.Duration(0)
		if havePreviousPTS {
			if ptsRegressed {
				timestampAdvance = positiveDiscontinuityDuration(sample.Duration, p.videoTrack.capability.ClockRate)
			} else {
				timestampAdvance = sample.PTS - previousPTS
			}
		}
		sampleAge := time.Since(sample.ProducedAt)
		p.videoSampleAge.Store(int64(sampleAge))
		for previousMax := p.videoMaxSampleAge.Load(); int64(sampleAge) > previousMax; previousMax = p.videoMaxSampleAge.Load() {
			if p.videoMaxSampleAge.CompareAndSwap(previousMax, int64(sampleAge)) {
				break
			}
		}
		writeStarted := time.Now()
		rtpTicks, err := p.videoTrack.WriteSample(sample.Data, timestampAdvance)
		if err != nil {
			if p.ctx.Err() == nil {
				p.logger.Debug("peer video writer stopped", zap.Error(err))
				p.closeWith(websocket.CloseGoingAway, "video transport stopped")
			}
			return
		}
		writeDuration := time.Since(writeStarted)
		p.videoWriteDuration.Store(int64(writeDuration))
		for previousMax := p.videoMaxWrite.Load(); int64(writeDuration) > previousMax; previousMax = p.videoMaxWrite.Load() {
			if p.videoMaxWrite.CompareAndSwap(previousMax, int64(writeDuration)) {
				break
			}
		}
		p.videoSamplesWritten.Add(1)
		p.videoBytesWritten.Add(uint64(len(sample.Data)))
		rtpTicksTotal += uint64(rtpTicks)
		clockRate := uint64(p.videoTrack.capability.ClockRate)
		rtpElapsed := time.Duration(rtpTicksTotal/clockRate)*time.Second +
			time.Duration((rtpTicksTotal%clockRate)*uint64(time.Second)/clockRate)
		p.videoProducedElapsed.Store(int64(producedElapsed))
		p.videoRTPElapsed.Store(int64(rtpElapsed))
		p.videoProductionGap.Store(int64(productionGap))
		p.videoSampleDuration.Store(int64(sample.Duration))
		if !havePreviousPTS || ptsRegressed {
			firstPTS = sample.PTS
			rtpAnchorTicks = rtpTicksTotal
		}
		mediaElapsed := sample.PTS - firstPTS
		rtpMediaElapsed := time.Duration((rtpTicksTotal-rtpAnchorTicks)/clockRate)*time.Second + time.Duration(((rtpTicksTotal-rtpAnchorTicks)%clockRate)*uint64(time.Second)/clockRate)
		p.videoTimingDrift.Store(int64(rtpMediaElapsed - mediaElapsed))
		p.videoRTPTicks.Store(rtpTicksTotal)
		p.videoLastRTPTicks.Store(uint64(rtpTicks))
		previousProducedAt = sample.ProducedAt
		previousPTS = sample.PTS
		havePreviousPTS = true
	}
}

func (p *peer) writeAudioSamples() {
	var previousPTS time.Duration
	havePreviousPTS := false
	for {
		sample, ok := p.audioSamples.take(p.ctx)
		if !ok {
			return
		}
		{
			ptsRegressed := havePreviousPTS && sample.PTS <= previousPTS
			timestampAdvance := time.Duration(0)
			if havePreviousPTS {
				if ptsRegressed {
					timestampAdvance = positiveDiscontinuityDuration(sample.Duration, p.audioTrack.capability.ClockRate)
				} else {
					timestampAdvance = sample.PTS - previousPTS
				}
			}
			if _, err := p.audioTrack.WriteSample(sample.Data, timestampAdvance); err != nil {
				if p.ctx.Err() == nil {
					p.logger.Debug("peer audio writer stopped", zap.Error(err))
					p.closeWith(websocket.CloseGoingAway, "audio transport stopped")
				}
				return
			}
			p.audioSamplesWritten.Add(1)
			previousPTS = sample.PTS
			havePreviousPTS = true
		}
	}
}
