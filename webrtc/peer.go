package webrtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	pion "github.com/pion/webrtc/v4"
	remoteinput "github.com/tarik02/webdesktop/input"
	"github.com/tarik02/webdesktop/media"
	"go.uber.org/zap"
)

const (
	maxSignalingMessageBytes = 128 * 1024
	maxControlMessageBytes   = 16 * 1024
	maxInputMessageBytes     = 4 * 1024
	maxQueuedCandidates      = 256

	initialOfferTimeout        = 10 * time.Second
	signalingPingInterval      = 5 * time.Second
	signalingPongWait          = 15 * time.Second
	traceSnapshotInterval      = 5 * time.Second
	websocketCloseTimeout      = time.Second
	peerConnectionCloseTimeout = 2 * time.Second
)

type peer struct {
	id      uint64
	service *Service
	logger  *zap.Logger
	conn    *websocket.Conn
	pc      *pion.PeerConnection

	videoSender  *pion.RTPSender
	videoTrack   *sampleTrack
	videoCodec   string
	videoSamples chan media.Sample
	audioSender  *pion.RTPSender
	audioTrack   *sampleTrack
	audioSamples chan media.AudioSample

	ctx                context.Context
	cancel             context.CancelFunc
	closeOnce          sync.Once
	connected          atomic.Bool
	videoNeedsKeyframe atomic.Bool

	signalWriteMu    sync.Mutex
	controlWriteMu   sync.Mutex
	inputWriteMu     sync.Mutex
	clipboardWriteMu sync.Mutex

	offerHandled             bool
	remoteDescriptionSet     bool
	pendingRemoteCandidates  []pion.ICECandidateInit
	localCandidateMu         sync.Mutex
	answerSent               bool
	pendingLocalCandidates   []pion.ICECandidateInit
	connectedKeyframeRequest sync.Once
	controlMu                sync.Mutex
	control                  *pion.DataChannel
	inputMu                  sync.Mutex
	input                    *pion.DataChannel
	clipboardMu              sync.Mutex
	clipboard                *pion.DataChannel
	clipboardReceive         *clipboardReceive
	clipboardSequence        uint64
	clipboardBufferedLow     chan struct{}
	clipboardClosed          chan struct{}
	clipboardCloseOnce       sync.Once
	inputSequence            uint64
	inputSequenceSet         bool

	videoSamplesSeen     atomic.Uint64
	videoSamplesEnqueued atomic.Uint64
	videoSamplesDropped  atomic.Uint64
	videoSamplesWritten  atomic.Uint64
	videoBytesWritten    atomic.Uint64
	videoNACKReports     atomic.Uint64
	videoNACKPackets     atomic.Uint64
	audioSamplesSeen     atomic.Uint64
	audioSamplesEnqueued atomic.Uint64
	audioSamplesDropped  atomic.Uint64
	audioSamplesWritten  atomic.Uint64
	keyframeRequests     atomic.Uint64
	inputMessagesSeen    atomic.Uint64
	inputMessagesSent    atomic.Uint64
	inputOverloads       atomic.Uint64
	videoReportSeen      atomic.Bool
	videoFractionLost    atomic.Uint32
	videoTotalLost       atomic.Uint32
	videoJitter          atomic.Uint32
	videoPTSRegressions  atomic.Uint64
	videoProducedElapsed atomic.Int64
	videoRTPElapsed      atomic.Int64
	videoProductionGap   atomic.Int64
	videoSampleDuration  atomic.Int64
	videoTimingDrift     atomic.Int64
	videoSampleAge       atomic.Int64
	videoMaxSampleAge    atomic.Int64
	videoWriteDuration   atomic.Int64
	videoMaxWrite        atomic.Int64
	videoRTPTicks        atomic.Uint64
	videoLastRTPTicks    atomic.Uint64
}

func (s *Service) newPeer(connection *websocket.Conn) (*peer, error) {
	id, err := s.reservePeer()
	if err != nil {
		return nil, err
	}

	s.qualityMu.Lock()
	quality := s.source.Quality()
	videoCodec := videoCodecCapability(quality.Codec)
	peerConnection, err := s.newPeerConnection(
		quality.Codec,
		videoCodec,
	)
	if err != nil {
		s.qualityMu.Unlock()
		s.releaseReservation()
		return nil, err
	}

	ctx, cancel := context.WithCancel(s.ctx)
	videoTrack := newSampleTrack(
		videoCodec,
		pion.RTPCodecTypeVideo,
		fmt.Sprintf("video-%d", id),
		"desktop",
	)
	peerLogger := s.logger.With(zap.Uint64("peer_id", id))
	peer := &peer{
		id:           id,
		service:      s,
		logger:       peerLogger,
		conn:         connection,
		pc:           peerConnection,
		videoTrack:   videoTrack,
		videoCodec:   quality.Codec,
		videoSamples: make(chan media.Sample),
		ctx:          ctx,
		cancel:       cancel,
	}
	if err := s.registerPeer(peer); err != nil {
		s.qualityMu.Unlock()
		cancel()
		_ = peerConnection.Close()
		return nil, err
	}
	s.qualityMu.Unlock()
	videoSender, err := peerConnection.AddTrack(videoTrack)
	if err != nil {
		peer.Close()
		return nil, fmt.Errorf("add video track: %w", err)
	}
	peer.videoSender = videoSender
	if s.cfg.AudioEnabled {
		peer.audioTrack = newSampleTrack(
			s.audioCodec,
			pion.RTPCodecTypeAudio,
			fmt.Sprintf("audio-%d", id),
			"desktop",
		)
		peer.audioSamples = make(chan media.AudioSample)
		audioSender, err := peerConnection.AddTrack(peer.audioTrack)
		if err != nil {
			peer.Close()
			return nil, fmt.Errorf("add audio track: %w", err)
		}
		peer.audioSender = audioSender
	}

	peerConnection.OnICECandidate(peer.onLocalICECandidate)
	peerConnection.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		peer.logger.Info("peer connection state changed", zap.String("state", state.String()))
		switch state {
		case pion.PeerConnectionStateConnected:
			peer.videoNeedsKeyframe.Store(true)
			peer.connected.Store(true)
			peer.connectedKeyframeRequest.Do(func() {
				peer.keyframeRequests.Add(1)
				s.requestKeyframe("peer-connected")
			})
		case pion.PeerConnectionStateFailed, pion.PeerConnectionStateClosed:
			go peer.closeWith(websocket.CloseGoingAway, "peer connection closed")
		}
	})
	peerConnection.OnICEConnectionStateChange(func(state pion.ICEConnectionState) {
		if s.cfg.TracingEnabled {
			peer.logger.Debug("ICE connection state changed", zap.String("state", state.String()))
		}
		if state == pion.ICEConnectionStateFailed || state == pion.ICEConnectionStateClosed {
			go peer.closeWith(websocket.CloseGoingAway, "ICE connection closed")
		}
	})
	if s.cfg.TracingEnabled {
		peerConnection.OnICEGatheringStateChange(func(state pion.ICEGatheringState) {
			peer.logger.Debug("ICE gathering state changed", zap.String("state", state.String()))
		})
		peerConnection.OnSignalingStateChange(func(state pion.SignalingState) {
			peer.logger.Debug("signaling state changed", zap.String("state", state.String()))
		})
	}
	peerConnection.OnDataChannel(peer.onDataChannel)

	go peer.readRTCP(peer.videoSender, true)
	go peer.writeVideoSamples()
	if peer.audioSender != nil {
		go peer.readRTCP(peer.audioSender, false)
		go peer.writeAudioSamples()
	}
	if s.cfg.TracingEnabled {
		go peer.traceLoop()
	}
	peer.logger.Info("WebRTC peer created", zap.Int("active_peers", s.PeerCount()))
	return peer, nil
}

func (p *peer) run(requestContext context.Context) {
	go func() {
		select {
		case <-requestContext.Done():
			p.closeWith(websocket.CloseGoingAway, "request canceled")
		case <-p.ctx.Done():
		}
	}()
	go p.pingLoop()

	p.conn.SetReadLimit(maxSignalingMessageBytes)
	if err := p.conn.SetReadDeadline(time.Now().Add(initialOfferTimeout)); err != nil {
		p.closeWith(websocket.CloseInternalServerErr, "set offer deadline")
		return
	}
	p.conn.SetPongHandler(func(string) error {
		if !p.offerHandled {
			return nil
		}
		return p.conn.SetReadDeadline(time.Now().Add(signalingPongWait))
	})

	closeCode := websocket.CloseNormalClosure
	closeReason := "signaling closed"
	for {
		messageType, data, err := p.conn.ReadMessage()
		if err != nil {
			var networkError net.Error
			if errors.As(err, &networkError) && networkError.Timeout() {
				if !p.offerHandled {
					closeCode = websocket.ClosePolicyViolation
					closeReason = "offer timeout"
				} else {
					closeCode = websocket.CloseGoingAway
					closeReason = "signaling pong timeout"
				}
			}
			if !websocket.IsCloseError(err,
				websocket.CloseNormalClosure,
				websocket.CloseGoingAway,
				websocket.CloseNoStatusReceived,
			) && p.ctx.Err() == nil {
				p.logger.Debug("WebSocket read stopped", zap.Error(err))
			}
			break
		}
		if messageType != websocket.TextMessage {
			if !p.writeSignalError("invalid_message", "signaling messages must be WebSocket text messages") {
				break
			}
			continue
		}
		if !utf8.Valid(data) {
			if !p.writeSignalError("invalid_message", "signaling message is not valid UTF-8") {
				break
			}
			continue
		}
		request, err := decodeSignalRequest(data)
		if err != nil {
			if !p.writeSignalError("invalid_message", fmt.Sprintf("decode signaling message: %v", err)) {
				break
			}
			continue
		}
		if !request.Version.Set {
			if !p.writeSignalError("missing_field", "version is required") {
				break
			}
			continue
		}
		if !request.Type.Set {
			if !p.writeSignalError("missing_field", "type is required") {
				break
			}
			continue
		}
		if request.Version.Value != signalingVersion {
			if !p.writeSignalError(
				"unsupported_version",
				fmt.Sprintf("signaling protocol version %d is not supported", request.Version.Value),
			) {
				break
			}
			continue
		}

		switch request.Type.Value {
		case signalTypeOffer:
			if !request.SDP.Set ||
				request.SDP.Value == "" ||
				request.Candidate.Set ||
				hasClientLogFields(request) {
				if !p.writeSignalError("invalid_offer", "offer requires sdp and no candidate") {
					p.Close()
					return
				}
				continue
			}
			if err := p.handleOffer(request.SDP.Value); err != nil {
				_ = p.writeSignalError("invalid_offer", err.Error())
				p.Close()
				return
			}
		case signalTypeICECandidate:
			if request.SDP.Set || !request.Candidate.Set || hasClientLogFields(request) {
				if !p.writeSignalError("invalid_candidate", "ice-candidate requires candidate and no sdp") {
					p.Close()
					return
				}
				continue
			}
			if err := p.handleRemoteCandidate(request.Candidate.Value); err != nil {
				_ = p.writeSignalError("invalid_candidate", err.Error())
				p.Close()
				return
			}
		case signalTypeClientLog:
			if !p.service.cfg.TracingEnabled {
				if !p.writeSignalError("tracing_disabled", "client tracing is disabled") {
					p.Close()
					return
				}
				continue
			}
			if protocolErr := validateClientLogRequest(request); protocolErr != nil {
				if !p.writeSignalError(protocolErr.Code, protocolErr.Message) {
					p.Close()
					return
				}
				continue
			}
			p.logClientTrace(request)
		default:
			if !p.writeSignalError(
				"unexpected_message",
				fmt.Sprintf("signaling message type %q is not supported", request.Type.Value),
			) {
				p.Close()
				return
			}
		}
	}
	p.closeWith(closeCode, closeReason)
}

func (p *peer) handleOffer(sdp string) error {
	if p.offerHandled {
		return errors.New("only one offer is allowed per WebSocket connection")
	}
	if p.service.cfg.AudioEnabled {
		if err := validateAudioOffer(sdp); err != nil {
			return err
		}
	}
	if p.videoCodec == media.CodecH264 {
		if err := validateH264Offer(sdp); err != nil {
			return err
		}
	}
	p.offerHandled = true
	if err := p.conn.SetReadDeadline(time.Now().Add(signalingPongWait)); err != nil {
		return fmt.Errorf("set signaling read deadline: %w", err)
	}

	if err := p.pc.SetRemoteDescription(pion.SessionDescription{
		Type: pion.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		return fmt.Errorf("set remote offer: %w", err)
	}
	p.remoteDescriptionSet = true

	for _, candidate := range p.pendingRemoteCandidates {
		if err := p.pc.AddICECandidate(candidate); err != nil {
			return fmt.Errorf("add queued ICE candidate: %w", err)
		}
	}
	p.pendingRemoteCandidates = nil

	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("create answer: %w", err)
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return fmt.Errorf("set local answer: %w", err)
	}
	localDescription := p.pc.LocalDescription()
	if localDescription == nil {
		return errors.New("local answer is unavailable")
	}
	answerSDP := localDescription.SDP
	if p.videoCodec == media.CodecH264 {
		answerSDP, err = rewriteH264Answer(answerSDP)
		if err != nil {
			return fmt.Errorf("set H.264 answer parameters: %w", err)
		}
	}
	if !p.writeSignal(signalResponse{
		Version: signalingVersion,
		Type:    signalTypeAnswer,
		SDP:     answerSDP,
	}) {
		return errors.New("write answer")
	}
	p.flushLocalCandidates()
	return nil
}

func (p *peer) handleRemoteCandidate(candidate pion.ICECandidateInit) error {
	if !p.remoteDescriptionSet {
		if len(p.pendingRemoteCandidates) >= maxQueuedCandidates {
			return errors.New("too many ICE candidates arrived before the offer")
		}
		p.pendingRemoteCandidates = append(p.pendingRemoteCandidates, candidate)
		return nil
	}
	if err := p.pc.AddICECandidate(candidate); err != nil {
		return fmt.Errorf("add ICE candidate: %w", err)
	}
	return nil
}

func (p *peer) onLocalICECandidate(candidate *pion.ICECandidate) {
	if candidate == nil || p.ctx.Err() != nil {
		return
	}
	init := candidate.ToJSON()

	p.localCandidateMu.Lock()
	if !p.answerSent {
		if len(p.pendingLocalCandidates) >= maxQueuedCandidates {
			p.localCandidateMu.Unlock()
			go p.Close()
			return
		}
		p.pendingLocalCandidates = append(p.pendingLocalCandidates, init)
		p.localCandidateMu.Unlock()
		return
	}
	p.localCandidateMu.Unlock()

	if !p.writeSignal(signalResponse{
		Version:   signalingVersion,
		Type:      signalTypeICECandidate,
		Candidate: &init,
	}) {
		go p.Close()
	}
}

func (p *peer) flushLocalCandidates() {
	p.localCandidateMu.Lock()
	p.answerSent = true
	candidates := append([]pion.ICECandidateInit(nil), p.pendingLocalCandidates...)
	p.pendingLocalCandidates = nil
	p.localCandidateMu.Unlock()

	for i := range candidates {
		if !p.writeSignal(signalResponse{
			Version:   signalingVersion,
			Type:      signalTypeICECandidate,
			Candidate: &candidates[i],
		}) {
			go p.Close()
			return
		}
	}
}

func (p *peer) writeSignal(message signalResponse) bool {
	p.signalWriteMu.Lock()
	defer p.signalWriteMu.Unlock()

	if p.ctx.Err() != nil {
		return false
	}
	if err := p.conn.SetWriteDeadline(time.Now().Add(defaultSignalingWriteTimeout)); err != nil {
		return false
	}
	if err := p.conn.WriteJSON(message); err != nil {
		p.logger.Debug("WebSocket write stopped", zap.Error(err))
		return false
	}
	return true
}

func (p *peer) writeSignalError(code, message string) bool {
	return p.writeSignal(signalResponse{
		Version: signalingVersion,
		Type:    signalTypeError,
		Error: &protocolError{
			Code:    code,
			Message: message,
		},
	})
}

func (p *peer) pingLoop() {
	ticker := time.NewTicker(signalingPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			if err := p.conn.WriteControl(
				websocket.PingMessage,
				nil,
				time.Now().Add(defaultSignalingWriteTimeout),
			); err != nil {
				p.closeWith(websocket.CloseGoingAway, "signaling ping failed")
				return
			}
		}
	}
}

func (p *peer) logClientTrace(request signalRequest) {
	keys := make([]string, 0, len(request.Details.Value))
	for key := range request.Details.Value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	details := make([]zap.Field, 0, len(keys))
	for _, key := range keys {
		details = append(details, zap.String(key, request.Details.Value[key]))
	}
	fields := []zap.Field{
		zap.String("client_event", request.Event.Value),
		zap.String("client_level", request.Level.Value),
		zap.Dict("client_details", details...),
	}
	logger := p.logger.Named("client")
	switch request.Level.Value {
	case "debug":
		logger.Debug("client trace", fields...)
	case "info":
		logger.Info("client trace", fields...)
	case "warn":
		logger.Warn("client trace", fields...)
	case "error":
		logger.Error("client trace", fields...)
	}
}

func (p *peer) traceLoop() {
	p.logTraceSnapshot()
	ticker := time.NewTicker(traceSnapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.logTraceSnapshot()
		}
	}
}

func (p *peer) logTraceSnapshot() {
	p.controlMu.Lock()
	controlState := "absent"
	if p.control != nil {
		controlState = p.control.ReadyState().String()
	}
	p.controlMu.Unlock()
	p.inputMu.Lock()
	inputState := "absent"
	if p.input != nil {
		inputState = p.input.ReadyState().String()
	}
	p.inputMu.Unlock()

	fields := []zap.Field{
		zap.String("codec", p.videoCodec),
		zap.Bool("connected", p.connected.Load()),
		zap.Bool("video_needs_keyframe", p.videoNeedsKeyframe.Load()),
		zap.String("peer_connection_state", p.pc.ConnectionState().String()),
		zap.String("ice_connection_state", p.pc.ICEConnectionState().String()),
		zap.String("ice_gathering_state", p.pc.ICEGatheringState().String()),
		zap.String("signaling_state", p.pc.SignalingState().String()),
		zap.String("control_channel_state", controlState),
		zap.String("input_channel_state", inputState),
		zap.Int("video_queue_length", len(p.videoSamples)),
		zap.Uint64("video_samples_seen", p.videoSamplesSeen.Load()),
		zap.Uint64("video_samples_enqueued", p.videoSamplesEnqueued.Load()),
		zap.Uint64("video_samples_dropped", p.videoSamplesDropped.Load()),
		zap.Uint64("video_samples_written", p.videoSamplesWritten.Load()),
		zap.Uint64("video_bytes_written", p.videoBytesWritten.Load()),
		zap.Uint64("video_nack_reports", p.videoNACKReports.Load()),
		zap.Uint64("video_nack_packets", p.videoNACKPackets.Load()),
		zap.Int("video_bitrate_kbps", p.service.source.Quality().BitrateKbps),
		zap.Uint64("video_source_pts_regressions", p.videoPTSRegressions.Load()),
		zap.Duration("video_produced_elapsed", time.Duration(p.videoProducedElapsed.Load())),
		zap.Duration("video_rtp_elapsed", time.Duration(p.videoRTPElapsed.Load())),
		zap.Duration("video_production_gap", time.Duration(p.videoProductionGap.Load())),
		zap.Duration("video_sample_duration", time.Duration(p.videoSampleDuration.Load())),
		zap.Duration("video_timing_drift", time.Duration(p.videoTimingDrift.Load())),
		zap.Duration("video_sample_age", time.Duration(p.videoSampleAge.Load())),
		zap.Duration("video_max_sample_age", time.Duration(p.videoMaxSampleAge.Load())),
		zap.Duration("video_write_duration", time.Duration(p.videoWriteDuration.Load())),
		zap.Duration("video_max_write_duration", time.Duration(p.videoMaxWrite.Load())),
		zap.Uint64("video_rtp_ticks", p.videoRTPTicks.Load()),
		zap.Uint64("video_last_rtp_ticks", p.videoLastRTPTicks.Load()),
		zap.Uint64("keyframe_requests", p.keyframeRequests.Load()),
		zap.Uint64("input_messages_seen", p.inputMessagesSeen.Load()),
		zap.Uint64("input_messages_sent", p.inputMessagesSent.Load()),
		zap.Uint64("input_overloads", p.inputOverloads.Load()),
	}
	if p.audioSamples != nil {
		fields = append(fields,
			zap.Int("audio_queue_length", len(p.audioSamples)),
			zap.Uint64("audio_samples_seen", p.audioSamplesSeen.Load()),
			zap.Uint64("audio_samples_enqueued", p.audioSamplesEnqueued.Load()),
			zap.Uint64("audio_samples_dropped", p.audioSamplesDropped.Load()),
			zap.Uint64("audio_samples_written", p.audioSamplesWritten.Load()),
		)
	}
	if p.videoReportSeen.Load() {
		fields = append(fields,
			zap.Uint32("video_rtcp_fraction_lost", p.videoFractionLost.Load()),
			zap.Uint32("video_rtcp_total_lost", p.videoTotalLost.Load()),
			zap.Uint32("video_rtcp_jitter_clock_units", p.videoJitter.Load()),
		)
	}
	p.logger.Debug("peer trace snapshot", fields...)
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

func (p *peer) onDataChannel(channel *pion.DataChannel) {
	switch channel.Label() {
	case "control":
		p.setControlChannel(channel)
	case "input":
		p.setInputChannel(channel)
	case "clipboard":
		if p.service.clipboard.Enabled() {
			p.setClipboardChannel(channel)
		} else {
			_ = channel.Close()
		}
	default:
		p.logger.Info("rejecting unsupported data channel", zap.String("label", channel.Label()))
		_ = channel.Close()
	}
}

func (p *peer) setControlChannel(channel *pion.DataChannel) {
	if !channel.Ordered() || channel.MaxPacketLifeTime() != nil || channel.MaxRetransmits() != nil {
		p.logger.Info("rejecting control data channel without reliable ordered delivery")
		_ = channel.Close()
		return
	}

	p.controlMu.Lock()
	if p.control != nil {
		p.controlMu.Unlock()
		p.logger.Info("rejecting duplicate control data channel")
		_ = channel.Close()
		return
	}
	p.control = channel
	p.controlMu.Unlock()

	channel.OnOpen(func() {
		p.logger.Info("control data channel opened")
	})
	channel.OnClose(func() {
		p.logger.Info("control data channel closed")
		_ = p.service.input.Release(p.id)
	})
	channel.OnError(func(err error) {
		p.logger.Debug("control data channel error", zap.Error(err))
		_ = p.service.input.Release(p.id)
	})
	channel.OnMessage(func(message pion.DataChannelMessage) {
		p.handleControlMessage(channel, message)
	})
}

func (p *peer) setInputChannel(channel *pion.DataChannel) {
	if !channel.Ordered() || channel.MaxPacketLifeTime() != nil || channel.MaxRetransmits() != nil {
		p.logger.Info("rejecting input data channel without reliable ordered delivery")
		_ = channel.Close()
		return
	}

	p.inputMu.Lock()
	if p.input != nil {
		p.inputMu.Unlock()
		p.logger.Info("rejecting duplicate input data channel")
		_ = channel.Close()
		return
	}
	p.input = channel
	p.inputMu.Unlock()

	channel.OnOpen(func() {
		p.logger.Info("input data channel opened")
	})
	channel.OnClose(func() {
		p.logger.Info("input data channel closed")
		_ = p.service.input.Release(p.id)
	})
	channel.OnError(func(err error) {
		p.logger.Debug("input data channel error", zap.Error(err))
		_ = p.service.input.Release(p.id)
	})
	channel.OnMessage(func(message pion.DataChannelMessage) {
		p.handleInputMessage(channel, message)
	})
}

func (p *peer) handleControlMessage(channel *pion.DataChannel, message pion.DataChannelMessage) {
	if !message.IsString {
		p.writeControlError(channel, "", "invalid_message", "control messages must be text")
		return
	}
	if len(message.Data) > maxControlMessageBytes {
		p.writeControlError(channel, "", "message_too_large", "control message exceeds 16384 bytes")
		return
	}
	if !utf8.Valid(message.Data) {
		p.writeControlError(channel, "", "invalid_message", "control message is not valid UTF-8")
		return
	}

	request, err := decodeControlRequest(message.Data)
	if err != nil {
		p.writeControlError(channel, request.ID.Value, "invalid_message", fmt.Sprintf("decode control message: %v", err))
		return
	}
	if protocolErr := validateControlRequest(request); protocolErr != nil {
		p.writeControlError(channel, request.ID.Value, protocolErr.Code, protocolErr.Message)
		return
	}

	switch request.Type.Value {
	case controlTypeInputAcquire:
		if !p.connected.Load() {
			p.writeControlError(channel, request.ID.Value, "peer_not_connected", "WebRTC peer is not connected")
			return
		}
		p.inputMu.Lock()
		inputChannel := p.input
		p.inputMu.Unlock()
		if inputChannel == nil || inputChannel.ReadyState() != pion.DataChannelStateOpen {
			p.writeControlError(channel, request.ID.Value, "input_channel_required", "an open reliable ordered input data channel is required")
			return
		}
		capabilities, err := p.service.input.Acquire(p.id, p.onInputRevoked)
		if err != nil {
			code := inputErrorCode(err)
			p.writeControlError(channel, request.ID.Value, code, err.Error())
			return
		}
		if !p.writeControl(channel, controlResponse{
			Version: controlVersion,
			ID:      request.ID.Value,
			Type:    controlTypeInputAcquireResult,
			OK:      true,
			Input: &controlInput{
				Pointer:  capabilities.Pointer,
				Keyboard: capabilities.Keyboard,
			},
		}) {
			go p.Close()
			return
		}
		if p.service.clipboard.Enabled() {
			go p.sendLatestClipboard()
		}
		return
	case controlTypeInputRelease:
		if err := p.service.input.Release(p.id); err != nil {
			p.writeControlError(channel, request.ID.Value, inputErrorCode(err), err.Error())
			return
		}
		if !p.writeControl(channel, controlResponse{
			Version: controlVersion,
			ID:      request.ID.Value,
			Type:    controlTypeInputReleaseResult,
			OK:      true,
		}) {
			go p.Close()
		}
		return
	}

	p.service.qualityMu.Lock()
	current := p.service.source.Quality()
	quality := media.Quality{
		Codec:       current.Codec,
		Width:       current.Width,
		Height:      current.Height,
		Framerate:   current.Framerate,
		BitrateKbps: current.BitrateKbps,
	}
	if request.Quality.Value.Codec.Set {
		quality.Codec = request.Quality.Value.Codec.Value
	}
	if request.Quality.Value.Width.Set {
		quality.Width = request.Quality.Value.Width.Value
	}
	if request.Quality.Value.Height.Set {
		quality.Height = request.Quality.Value.Height.Value
	}
	if request.Quality.Value.Framerate.Set {
		quality.Framerate = request.Quality.Value.Framerate.Value
	}
	if request.Quality.Value.BitrateKbps.Set {
		quality.BitrateKbps = request.Quality.Value.BitrateKbps.Value
	}
	if quality.Codec == media.CodecH264 {
		if err := media.ValidateH264Level42(quality); err != nil {
			p.service.qualityMu.Unlock()
			p.writeControlError(channel, request.ID.Value, "h264_level_incompatible", err.Error())
			return
		}
	}
	err = p.service.source.UpdateQuality(quality)
	effective := p.service.source.Quality()
	codecChanged := err == nil && effective.Codec != current.Codec
	var qualityGeneration uint64
	var incompatiblePeers []*peer
	if codecChanged {
		p.service.qualityGeneration++
		qualityGeneration = p.service.qualityGeneration
		for _, peer := range p.service.peerSnapshot() {
			if peer != p && peer.videoCodec != effective.Codec {
				incompatiblePeers = append(incompatiblePeers, peer)
			}
		}
	}
	p.service.qualityMu.Unlock()
	if err != nil {
		p.writeControlError(channel, request.ID.Value, "quality_update_failed", err.Error())
		return
	}
	responseWritten := p.writeControl(channel, controlResponse{
		Version: controlVersion,
		ID:      request.ID.Value,
		Type:    controlTypeQualitySetResult,
		OK:      true,
		Quality: qualityResponse(effective),
	})
	for _, peer := range incompatiblePeers {
		go p.service.closePeerForCodecChange(peer, qualityGeneration)
	}
	if !responseWritten {
		go p.Close()
	}
}

func (p *peer) handleInputMessage(channel *pion.DataChannel, message pion.DataChannelMessage) {
	p.inputMessagesSeen.Add(1)
	if !message.IsString {
		p.writeInputError(channel, nil, "invalid_message", "input messages must be text")
		return
	}
	if len(message.Data) > maxInputMessageBytes {
		p.writeInputError(channel, nil, "message_too_large", "input message exceeds 4096 bytes")
		return
	}
	if !utf8.Valid(message.Data) {
		p.writeInputError(channel, nil, "invalid_message", "input message is not valid UTF-8")
		return
	}

	request, err := decodeInputRequest(message.Data)
	sequence := inputSequencePointer(request)
	if err != nil {
		p.writeInputError(channel, sequence, "invalid_message", fmt.Sprintf("decode input message: %v", err))
		return
	}
	event, protocolErr := validateInputRequest(request)
	if protocolErr != nil {
		p.writeInputError(channel, sequence, protocolErr.Code, protocolErr.Message)
		return
	}

	p.inputMu.Lock()
	if p.inputSequenceSet && request.Sequence.Value <= p.inputSequence {
		p.inputMu.Unlock()
		p.writeInputError(channel, sequence, "invalid_sequence", "sequence must increase monotonically")
		return
	}
	p.inputSequence = request.Sequence.Value
	p.inputSequenceSet = true
	p.inputMu.Unlock()

	if !p.connected.Load() {
		p.writeInputError(channel, sequence, "peer_not_connected", "WebRTC peer is not connected")
		return
	}
	if !p.service.input.Owns(p.id) {
		p.writeInputError(channel, sequence, "input_not_owned", "peer does not own input")
		return
	}
	if err := p.service.input.Submit(p.id, event); err != nil {
		if errors.Is(err, remoteinput.ErrOverloaded) {
			p.inputOverloads.Add(1)
			return
		}
		p.writeInputError(channel, sequence, inputErrorCode(err), err.Error())
		return
	}
	p.inputMessagesSent.Add(1)
}

func (p *peer) enqueueVideo(sample media.Sample) {
	if !p.connected.Load() || p.ctx.Err() != nil {
		return
	}
	p.videoSamplesSeen.Add(1)
	if p.videoNeedsKeyframe.Load() && !sample.KeyFrame {
		p.videoSamplesDropped.Add(1)
		return
	}
	select {
	case p.videoSamples <- sample:
		p.videoSamplesEnqueued.Add(1)
		if sample.KeyFrame {
			p.videoNeedsKeyframe.Store(false)
		}
	case <-p.ctx.Done():
	}
}

func (p *peer) enqueueAudio(sample media.AudioSample) {
	if !p.connected.Load() || p.ctx.Err() != nil {
		return
	}
	p.audioSamplesSeen.Add(1)
	select {
	case p.audioSamples <- sample:
		p.audioSamplesEnqueued.Add(1)
	case <-p.ctx.Done():
	}
}

func (p *peer) writeVideoSamples() {
	var origin time.Time
	var previousProducedAt time.Time
	var previousPTS time.Duration
	var rtpTicksTotal uint64
	havePreviousPTS := false
	for {
		select {
		case <-p.ctx.Done():
			return
		case sample := <-p.videoSamples:
			if origin.IsZero() {
				origin = sample.ProducedAt
			}
			producedElapsed := sample.ProducedAt.Sub(origin)
			var productionGap time.Duration
			if !previousProducedAt.IsZero() {
				productionGap = sample.ProducedAt.Sub(previousProducedAt)
			}
			if havePreviousPTS && sample.PTS <= previousPTS {
				p.videoPTSRegressions.Add(1)
			}
			sampleAge := time.Since(sample.ProducedAt)
			p.videoSampleAge.Store(int64(sampleAge))
			for previousMax := p.videoMaxSampleAge.Load(); int64(sampleAge) > previousMax; previousMax = p.videoMaxSampleAge.Load() {
				if p.videoMaxSampleAge.CompareAndSwap(previousMax, int64(sampleAge)) {
					break
				}
			}
			writeStarted := time.Now()
			rtpTicks, err := p.videoTrack.WriteSampleAt(sample.Data, productionGap)
			if err != nil {
				if p.ctx.Err() == nil {
					p.logger.Debug("peer video writer stopped", zap.Error(err))
					go p.closeWith(websocket.CloseGoingAway, "video transport stopped")
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
			rtpTicksTotal += rtpTicks
			clockRate := uint64(p.videoTrack.capability.ClockRate)
			rtpElapsed := time.Duration(rtpTicksTotal/clockRate)*time.Second +
				time.Duration((rtpTicksTotal%clockRate)*uint64(time.Second)/clockRate)
			p.videoProducedElapsed.Store(int64(producedElapsed))
			p.videoRTPElapsed.Store(int64(rtpElapsed))
			p.videoProductionGap.Store(int64(productionGap))
			p.videoSampleDuration.Store(int64(sample.Duration))
			p.videoTimingDrift.Store(int64(rtpElapsed - producedElapsed))
			p.videoRTPTicks.Store(rtpTicksTotal)
			p.videoLastRTPTicks.Store(rtpTicks)
			previousProducedAt = sample.ProducedAt
			previousPTS = sample.PTS
			havePreviousPTS = true
		}
	}
}

func (p *peer) writeAudioSamples() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case sample := <-p.audioSamples:
			if _, err := p.audioTrack.WriteSample(sample.Data, sample.Duration); err != nil {
				if p.ctx.Err() == nil {
					p.logger.Debug("peer audio writer stopped", zap.Error(err))
					go p.closeWith(websocket.CloseGoingAway, "audio transport stopped")
				}
				return
			}
			p.audioSamplesWritten.Add(1)
		}
	}
}

func (p *peer) writeControlError(channel *pion.DataChannel, id, code, message string) {
	if !p.writeControl(channel, controlResponse{
		Version: controlVersion,
		ID:      id,
		Type:    controlTypeError,
		OK:      false,
		Error: &protocolError{
			Code:    code,
			Message: message,
		},
	}) {
		go p.Close()
	}
}

func (p *peer) writeControl(channel *pion.DataChannel, response controlResponse) bool {
	data, err := json.Marshal(response)
	if err != nil {
		p.logger.Error("encode control response", zap.Error(err))
		return false
	}

	p.controlWriteMu.Lock()
	defer p.controlWriteMu.Unlock()
	if err := channel.SendText(string(data)); err != nil {
		p.logger.Debug("control data channel write stopped", zap.Error(err))
		return false
	}
	return true
}

func (p *peer) onInputRevoked(sequence uint64, cause error) {
	p.inputMu.Lock()
	channel := p.input
	p.inputMu.Unlock()
	if channel == nil {
		return
	}
	var correlation *uint64
	if sequence != 0 {
		correlation = &sequence
	}
	p.writeInputError(channel, correlation, inputErrorCode(cause), cause.Error())
	_ = channel.Close()
}

func (p *peer) writeInputError(channel *pion.DataChannel, sequence *uint64, code, message string) {
	data, err := json.Marshal(inputResponse{
		Version:  inputVersion,
		Sequence: sequence,
		Type:     inputTypeError,
		OK:       false,
		Error: &protocolError{
			Code:    code,
			Message: message,
		},
	})
	if err != nil {
		p.logger.Error("encode input response", zap.Error(err))
		return
	}

	p.inputWriteMu.Lock()
	defer p.inputWriteMu.Unlock()
	if err := channel.SendText(string(data)); err != nil {
		p.logger.Debug("input data channel write stopped", zap.Error(err))
	}
}

func inputSequencePointer(request inputRequest) *uint64 {
	if !request.Sequence.Set {
		return nil
	}
	sequence := request.Sequence.Value
	return &sequence
}

func inputErrorCode(err error) string {
	switch {
	case errors.Is(err, remoteinput.ErrBusy):
		return "input_busy"
	case errors.Is(err, remoteinput.ErrDisabled):
		return "input_disabled"
	case errors.Is(err, remoteinput.ErrPointerUnauthorized):
		return "input_pointer_unauthorized"
	case errors.Is(err, remoteinput.ErrKeyboardUnauthorized):
		return "input_keyboard_unauthorized"
	case errors.Is(err, remoteinput.ErrNotReady):
		return "input_not_ready"
	case errors.Is(err, remoteinput.ErrNotOwner):
		return "input_not_owned"
	case errors.Is(err, remoteinput.ErrOverloaded):
		return "input_overloaded"
	case errors.Is(err, remoteinput.ErrClosed):
		return "input_unavailable"
	default:
		return "input_failed"
	}
}

// Close releases the socket, peer connection, and peer accounting exactly once.
func (p *peer) Close() {
	p.closeWith(websocket.CloseNormalClosure, "peer closed")
}

func (p *peer) closeWith(code int, reason string) {
	p.closeOnce.Do(func() {
		if p.service.cfg.TracingEnabled {
			p.logger.Debug("peer closing",
				zap.Int("websocket_close_code", code),
				zap.String("reason", reason),
			)
		}
		p.connected.Store(false)
		_ = p.service.input.Release(p.id)
		p.cancel()
		p.service.removePeer(p)
		_ = p.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(code, reason),
			time.Now().Add(websocketCloseTimeout),
		)
		_ = p.conn.Close()

		if sctp := p.pc.SCTP(); sctp != nil {
			if transport := sctp.Transport(); transport != nil {
				if iceTransport := transport.ICETransport(); iceTransport != nil {
					_ = iceTransport.Stop()
				}
			}
		}
		closed := make(chan struct{})
		go func() {
			_ = p.pc.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(peerConnectionCloseTimeout):
			p.logger.Warn("peer connection close timed out")
		}
		p.logger.Info("WebRTC peer closed", zap.Int("active_peers", p.service.PeerCount()))
	})
}
