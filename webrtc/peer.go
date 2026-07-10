package webrtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/pion/rtcp"
	pion "github.com/pion/webrtc/v4"
	"github.com/tarik02/webdesktop/media"
	"go.uber.org/zap"
)

const (
	maxSignalingMessageBytes = 128 * 1024
	maxControlMessageBytes   = 16 * 1024
	maxQueuedCandidates      = 256
	peerSampleQueueSize      = 8

	initialOfferTimeout        = 10 * time.Second
	signalingPingInterval      = 5 * time.Second
	signalingPongWait          = 15 * time.Second
	websocketCloseTimeout      = time.Second
	peerConnectionCloseTimeout = 2 * time.Second
	maxRTPPTSJump              = 10 * time.Second
)

type peer struct {
	id      uint64
	service *Service
	logger  *zap.Logger
	conn    *websocket.Conn
	pc      *pion.PeerConnection
	sender  *pion.RTPSender
	track   *sampleTrack
	samples chan media.Sample

	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once
	connected atomic.Bool

	signalWriteMu  sync.Mutex
	controlWriteMu sync.Mutex

	offerHandled             bool
	remoteDescriptionSet     bool
	pendingRemoteCandidates  []pion.ICECandidateInit
	localCandidateMu         sync.Mutex
	answerSent               bool
	pendingLocalCandidates   []pion.ICECandidateInit
	connectedKeyframeRequest sync.Once
	controlMu                sync.Mutex
	control                  *pion.DataChannel
}

func (s *Service) newPeer(connection *websocket.Conn) (*peer, error) {
	id, err := s.reservePeer()
	if err != nil {
		return nil, err
	}

	peerConnection, err := s.newPeerConnection()
	if err != nil {
		s.releaseReservation()
		return nil, err
	}

	ctx, cancel := context.WithCancel(s.ctx)
	track := newSampleTrack(s.capability, fmt.Sprintf("video-%d", id), "desktop")
	peer := &peer{
		id:      id,
		service: s,
		logger:  s.logger.With(zap.Uint64("peer_id", id)),
		conn:    connection,
		pc:      peerConnection,
		track:   track,
		samples: make(chan media.Sample, peerSampleQueueSize),
		ctx:     ctx,
		cancel:  cancel,
	}
	if err := s.registerPeer(peer); err != nil {
		cancel()
		_ = peerConnection.Close()
		return nil, err
	}

	sender, err := peerConnection.AddTrack(track)
	if err != nil {
		peer.Close()
		return nil, fmt.Errorf("add video track: %w", err)
	}
	peer.sender = sender

	peerConnection.OnICECandidate(peer.onLocalICECandidate)
	peerConnection.OnConnectionStateChange(func(state pion.PeerConnectionState) {
		peer.logger.Info("peer connection state changed", zap.String("state", state.String()))
		switch state {
		case pion.PeerConnectionStateConnected:
			peer.connected.Store(true)
			peer.connectedKeyframeRequest.Do(func() {
				s.requestKeyframe("peer-connected")
			})
		case pion.PeerConnectionStateFailed, pion.PeerConnectionStateClosed:
			go peer.closeWith(websocket.CloseGoingAway, "peer connection closed")
		}
	})
	peerConnection.OnICEConnectionStateChange(func(state pion.ICEConnectionState) {
		if state == pion.ICEConnectionStateFailed || state == pion.ICEConnectionStateClosed {
			go peer.closeWith(websocket.CloseGoingAway, "ICE connection closed")
		}
	})
	peerConnection.OnDataChannel(peer.onDataChannel)

	go peer.readRTCP()
	go peer.writeSamples()
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
			if !request.SDP.Set || request.SDP.Value == "" || request.Candidate.Set {
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
			if request.SDP.Set || !request.Candidate.Set {
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
	if p.service.cfg.Codec == media.CodecH264 {
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
	if p.service.cfg.Codec == media.CodecH264 {
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

func (p *peer) readRTCP() {
	for {
		packets, _, err := p.sender.ReadRTCP()
		if err != nil {
			if !errors.Is(err, io.EOF) &&
				!errors.Is(err, io.ErrClosedPipe) &&
				p.ctx.Err() == nil {
				p.logger.Debug("RTCP reader stopped", zap.Error(err))
			}
			return
		}
		for _, packet := range packets {
			switch packet.(type) {
			case *rtcp.PictureLossIndication:
				p.service.requestKeyframe("pli")
			case *rtcp.FullIntraRequest:
				p.service.requestKeyframe("fir")
			}
		}
	}
}

func (p *peer) onDataChannel(channel *pion.DataChannel) {
	if channel.Label() != "control" {
		p.logger.Info("rejecting unsupported data channel", zap.String("label", channel.Label()))
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
	})
	channel.OnError(func(err error) {
		p.logger.Debug("control data channel error", zap.Error(err))
	})
	channel.OnMessage(func(message pion.DataChannelMessage) {
		p.handleControlMessage(channel, message)
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

	p.service.qualityMu.Lock()
	current := p.service.source.Quality()
	quality := media.Quality{
		Codec:       current.Codec,
		Width:       current.Width,
		Height:      current.Height,
		Framerate:   current.Framerate,
		BitrateKbps: current.BitrateKbps,
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
		if err := media.ValidateH264Level4(quality); err != nil {
			p.service.qualityMu.Unlock()
			p.writeControlError(channel, request.ID.Value, "h264_level_incompatible", err.Error())
			return
		}
	}
	err = p.service.source.UpdateQuality(quality)
	effective := p.service.source.Quality()
	p.service.qualityMu.Unlock()
	if err != nil {
		p.writeControlError(channel, request.ID.Value, "quality_update_failed", err.Error())
		return
	}

	if !p.writeControl(channel, controlResponse{
		Version: controlVersion,
		ID:      request.ID.Value,
		Type:    controlTypeQualitySetResult,
		OK:      true,
		Quality: qualityResponse(effective),
	}) {
		go p.Close()
	}
}

func (p *peer) enqueueSample(sample media.Sample) {
	if !p.connected.Load() || p.ctx.Err() != nil {
		return
	}
	select {
	case p.samples <- sample:
		return
	default:
	}
	select {
	case <-p.samples:
	default:
	}
	select {
	case p.samples <- sample:
	default:
	}
}

func (p *peer) writeSamples() {
	var pending media.Sample
	havePending := false
	for {
		select {
		case <-p.ctx.Done():
			return
		case sample := <-p.samples:
			if !havePending {
				pending = sample
				havePending = true
				continue
			}

			duration := pending.Duration
			if delta := sample.PTS - pending.PTS; delta > 0 && delta <= maxRTPPTSJump {
				duration = delta
			} else if delta <= 0 || delta > maxRTPPTSJump {
				p.logger.Debug("RTP PTS discontinuity",
					zap.Duration("previous_pts", pending.PTS),
					zap.Duration("current_pts", sample.PTS),
					zap.Duration("sample_duration", pending.Duration),
				)
			}
			if err := p.track.WriteSample(pending.Data, duration); err != nil {
				if p.ctx.Err() == nil {
					p.logger.Debug("peer video writer stopped", zap.Error(err))
					go p.closeWith(websocket.CloseGoingAway, "video transport stopped")
				}
				return
			}
			pending = sample
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

// Close releases the socket, peer connection, and peer accounting exactly once.
func (p *peer) Close() {
	p.closeWith(websocket.CloseNormalClosure, "peer closed")
}

func (p *peer) closeWith(code int, reason string) {
	p.closeOnce.Do(func() {
		p.connected.Store(false)
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
