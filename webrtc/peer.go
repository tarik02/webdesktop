package webrtc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	pion "github.com/pion/webrtc/v4"
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
	videoSamples videoMailbox
	audioSender  *pion.RTPSender
	audioTrack   *sampleTrack
	audioSamples audioMailbox

	ctx                context.Context
	cancel             context.CancelFunc
	closeOnce          sync.Once
	finishOnce         sync.Once
	done               chan struct{}
	ownedMu            sync.Mutex
	ownedOpen          bool
	owned              sync.WaitGroup
	closing            atomic.Bool
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
	clipboardState           *clipboardChannelState
	clipboardSequence        uint64
	inputSequence            uint64
	inputSequenceSet         bool
	inputChannelGeneration   uint64
	inputLeaseGeneration     uint64

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

func (p *peer) isClosing() bool { return p.closing.Load() || p.ctx.Err() != nil }

func (p *peer) goOwned(fn func()) bool {
	p.ownedMu.Lock()
	if !p.ownedOpen {
		p.ownedMu.Unlock()
		return false
	}
	p.owned.Add(1)
	p.ownedMu.Unlock()
	go func() {
		defer p.owned.Done()
		fn()
	}()
	return true
}

func (s *Service) newPeer(connection *websocket.Conn) (*peer, error) {
	id, err := s.reservePeer()
	if err != nil {
		return nil, err
	}

	s.qualityChangeMu.Lock()
	s.qualityMu.Lock()
	quality := s.source.Quality()
	videoCodec := videoCodecCapability(quality.Codec)
	peerConnection, err := s.newPeerConnection(quality.Codec, videoCodec)
	if err != nil {
		s.qualityMu.Unlock()
		s.qualityChangeMu.Unlock()
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
		videoSamples: newVideoMailbox(),
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		ownedOpen:    true,
	}
	if err := s.registerPeer(peer); err != nil {
		s.qualityMu.Unlock()
		s.qualityChangeMu.Unlock()
		cancel()
		_ = peerConnection.Close()
		s.releaseReservation()
		return nil, err
	}
	s.qualityMu.Unlock()
	s.qualityChangeMu.Unlock()
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
		peer.audioSamples = newAudioMailbox()
		audioSender, err := peerConnection.AddTrack(peer.audioTrack)
		if err != nil {
			peer.Close()
			return nil, fmt.Errorf("add audio track: %w", err)
		}
		peer.audioSender = audioSender
	}

	peerConnection.OnICECandidate(peer.onLocalICECandidate)
	peerConnection.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		if peer.isClosing() {
			return
		}
		peer.logger.Info("peer connection state changed", zap.String("state", state.String()))
		switch state {
		case pion.PeerConnectionStateConnected:
			if peer.isClosing() {
				return
			}
			peer.videoNeedsKeyframe.Store(true)
			peer.connected.Store(true)
			if peer.isClosing() {
				peer.connected.Store(false)
				return
			}
			peer.connectedKeyframeRequest.Do(func() {
				if peer.isClosing() {
					peer.connected.Store(false)
					return
				}
				peer.keyframeRequests.Add(1)
				s.requestKeyframe("peer-connected")
			})
		case pion.PeerConnectionStateFailed, pion.PeerConnectionStateClosed:
			peer.closeWith(websocket.CloseGoingAway, "peer connection closed")
		}
	})
	peerConnection.OnICEConnectionStateChange(func(state pion.ICEConnectionState) {
		if peer.isClosing() {
			return
		}
		if s.cfg.TracingEnabled {
			peer.logger.Debug("ICE connection state changed", zap.String("state", state.String()))
		}
		if state == pion.ICEConnectionStateFailed || state == pion.ICEConnectionStateClosed {
			peer.closeWith(websocket.CloseGoingAway, "ICE connection closed")
		}
	})
	if s.cfg.TracingEnabled {
		peerConnection.OnICEGatheringStateChange(func(state pion.ICEGatheringState) {
			if peer.isClosing() {
				return
			}
			peer.logger.Debug("ICE gathering state changed", zap.String("state", state.String()))
		})
		peerConnection.OnSignalingStateChange(func(state pion.SignalingState) {
			if peer.isClosing() {
				return
			}
			peer.logger.Debug("signaling state changed", zap.String("state", state.String()))
		})
	}
	peerConnection.OnDataChannel(peer.onDataChannel)

	peer.goOwned(func() { peer.readRTCP(peer.videoSender, true) })
	peer.goOwned(peer.writeVideoSamples)
	if peer.audioSender != nil {
		peer.goOwned(func() { peer.readRTCP(peer.audioSender, false) })
		peer.goOwned(peer.writeAudioSamples)
	}
	if s.cfg.TracingEnabled {
		peer.goOwned(peer.traceLoop)
	}
	peer.logger.Info("WebRTC peer created", zap.Int("active_peers", s.PeerCount()))
	return peer, nil
}

func (p *peer) run(requestContext context.Context) {
	p.goOwned(func() {
		select {
		case <-requestContext.Done():
			p.closeWith(websocket.CloseGoingAway, "request canceled")
		case <-p.ctx.Done():
		}
	})
	p.goOwned(p.pingLoop)

	p.conn.SetReadLimit(maxSignalingMessageBytes)
	if err := p.conn.SetReadDeadline(time.Now().Add(initialOfferTimeout)); err != nil {
		p.closeWith(websocket.CloseInternalServerErr, "set offer deadline")
		return
	}
	p.conn.SetPongHandler(func(string) error {
		if p.isClosing() {
			return errors.New("peer is closing")
		}
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
	if candidate == nil || p.isClosing() {
		return
	}
	init := candidate.ToJSON()

	p.localCandidateMu.Lock()
	if !p.answerSent {
		if len(p.pendingLocalCandidates) >= maxQueuedCandidates {
			p.localCandidateMu.Unlock()
			p.Close()
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
		p.Close()
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
			p.Close()
			return
		}
	}
}

func (p *peer) writeSignal(message signalResponse) bool {
	p.signalWriteMu.Lock()
	defer p.signalWriteMu.Unlock()

	if p.isClosing() {
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
		logger.Debug("frontend trace", fields...)
	case "info":
		logger.Info("frontend trace", fields...)
	case "warn":
		logger.Warn("frontend trace", fields...)
	case "error":
		logger.Error("frontend trace", fields...)
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
		zap.Int("video_queue_length", p.videoSamples.len()),
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
	if p.audioTrack != nil {
		fields = append(fields,
			zap.Int("audio_queue_length", p.audioSamples.len()),
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

// Close releases the socket, peer connection, and peer accounting exactly once.
func (p *peer) Close() {
	p.closeWith(websocket.CloseNormalClosure, "peer closed")
}

func (p *peer) closeWith(code int, reason string) {
	p.closeOnce.Do(func() {
		p.ownedMu.Lock()
		p.ownedOpen = false
		p.ownedMu.Unlock()
		p.closing.Store(true)
		p.connected.Store(false)
		p.inputMu.Lock()
		p.inputLeaseGeneration++
		p.inputMu.Unlock()
		p.cancel()
		_ = p.service.input.Release(p.id)
		go p.finishClose(code, reason)
	})
}

func (p *peer) finishClose(code int, reason string) {
	p.finishOnce.Do(func() {
		if p.service.cfg.TracingEnabled {
			p.logger.Debug("peer closing", zap.Int("websocket_close_code", code), zap.String("reason", reason))
		}
		_ = p.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason), time.Now().Add(websocketCloseTimeout))
		_ = p.conn.Close()
		if sctp := p.pc.SCTP(); sctp != nil {
			if transport := sctp.Transport(); transport != nil {
				if iceTransport := transport.ICETransport(); iceTransport != nil {
					_ = iceTransport.Stop()
				}
			}
		}
		_ = p.pc.Close()
		p.owned.Wait()
		p.service.removePeer(p)
		p.service.releaseReservation()
		close(p.done)
		p.logger.Info("WebRTC peer closed", zap.Int("active_peers", p.service.PeerCount()))
	})
}
