// Package webrtc provides shared video transport and WebSocket signaling.
package webrtc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/stun/v3"
	pion "github.com/pion/webrtc/v4"
	"go.uber.org/zap"
)

const (
	defaultSignalingWriteTimeout = 5 * time.Second
	servicePeerCloseTimeout      = 4 * time.Second
	opusPayloadType              = 111
)

var (
	errPeerLimit          = errors.New("maximum peer count reached")
	errServiceUnavailable = errors.New("WebRTC service is unavailable")
)

// Config contains static WebRTC and signaling settings.
type Config struct {
	AudioEnabled        bool
	ICEServers          []string
	ICEUsername         string
	ICECredential       string
	UDPPortMin          uint16
	UDPPortMax          uint16
	Subprotocols        []string
	MaxPeers            int
	ReplaceExistingPeer bool
	AllowedOrigins      []string
	TracingEnabled      bool
	Observer            Observer
}

// Validate checks the implemented transport settings.
func (cfg Config) Validate() error {
	var errs []error

	if cfg.MaxPeers < 1 || cfg.MaxPeers > 64 {
		errs = append(errs, errors.New("WebRTC max peers must be between 1 and 64"))
	}
	if cfg.ReplaceExistingPeer && cfg.MaxPeers != 1 {
		errs = append(errs, errors.New("replacing an existing peer requires WebRTC max peers to be 1"))
	}
	if (cfg.ICEUsername == "") != (cfg.ICECredential == "") {
		errs = append(errs, errors.New("ICE username and credential must both be set or both be empty"))
	}
	if len(cfg.ICEServers) == 0 && cfg.ICEUsername != "" {
		errs = append(errs, errors.New("ICE credentials require at least one ICE server"))
	}
	for _, server := range cfg.ICEServers {
		uri, err := stun.ParseURI(server)
		if err != nil {
			errs = append(errs, fmt.Errorf("invalid ICE server URL %q: %w", server, err))
			continue
		}
		switch uri.Scheme {
		case stun.SchemeTypeTURN, stun.SchemeTypeTURNS:
			if cfg.ICEUsername == "" {
				errs = append(errs, fmt.Errorf("TURN server %q requires ICE credentials", server))
			}
		}
	}
	if (cfg.UDPPortMin == 0) != (cfg.UDPPortMax == 0) || cfg.UDPPortMax < cfg.UDPPortMin {
		errs = append(errs, errors.New("ICE UDP port range is invalid"))
	}
	for _, origin := range cfg.AllowedOrigins {
		if origin == "*" {
			continue
		}
		parsed, err := url.Parse(origin)
		if err != nil ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") ||
			parsed.Host == "" ||
			parsed.User != nil ||
			parsed.Path != "" ||
			parsed.RawQuery != "" ||
			parsed.Fragment != "" {
			errs = append(errs, fmt.Errorf("invalid allowed WebSocket origin %q", origin))
		}
	}

	return errors.Join(errs...)
}

// Service fans one encoded media source out to active peer connections.
type Service struct {
	cfg        Config
	source     MediaSource
	audio      AudioSource
	input      InputController
	clipboard  ClipboardController
	logger     *zap.Logger
	audioCodec pion.RTPCodecCapability

	ctx    context.Context
	cancel context.CancelFunc

	runMu   sync.Mutex
	started bool
	admitMu sync.Mutex

	peersMu         sync.Mutex
	closed          bool
	closeDone       chan struct{}
	closeDoneClosed bool
	nextID          uint64
	reservations    int
	peers           map[*peer]struct{}

	qualityMu         sync.Mutex
	qualityChangeMu   sync.Mutex
	qualityGeneration uint64
	keyframeRequests  chan string
}

// New constructs a reusable WebRTC service without opening listeners.
func New(
	cfg Config,
	source MediaSource,
	audio AudioSource,
	inputController InputController,
	clipboardController ClipboardController,
	logger *zap.Logger,
) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("WebRTC media source is required")
	}
	if cfg.AudioEnabled && audio == nil {
		return nil, errors.New("WebRTC audio source is required when audio is enabled")
	}
	if logger == nil {
		return nil, errors.New("WebRTC logger is required")
	}
	quality := source.Quality()
	profile, exists := source.Profile(quality.Profile)
	if !exists {
		return nil, fmt.Errorf("media profile %q is unavailable", quality.Profile)
	}
	if cfg.AudioEnabled && profile.Codec.PayloadType == opusPayloadType {
		return nil, fmt.Errorf("video profile %q payload type conflicts with Opus", quality.Profile)
	}

	audioCodec := pion.RTPCodecCapability{
		MimeType:    pion.MimeTypeOpus,
		ClockRate:   48000,
		Channels:    2,
		SDPFmtpLine: "minptime=10;useinbandfec=1",
	}

	ctx, cancel := context.WithCancel(context.Background())
	if inputController == nil {
		inputController = disabledInputController{}
	}
	if clipboardController == nil {
		clipboardController = disabledClipboardController{}
	}
	source.SetActive(false)
	return &Service{
		cfg:              cfg,
		source:           source,
		audio:            audio,
		input:            inputController,
		clipboard:        clipboardController,
		logger:           logger,
		audioCodec:       audioCodec,
		ctx:              ctx,
		cancel:           cancel,
		peers:            make(map[*peer]struct{}),
		closeDone:        make(chan struct{}),
		keyframeRequests: make(chan string, 1),
	}, nil
}

// Handler returns the signaling handler for mounting behind application middleware.
func (s *Service) Handler() http.Handler {
	upgrader := websocket.Upgrader{
		HandshakeTimeout: defaultSignalingWriteTimeout,
		CheckOrigin:      s.originAllowed,
		Subprotocols:     append([]string(nil), s.cfg.Subprotocols...),
	}

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			s.logger.Debug("WebSocket upgrade rejected", zap.Error(err))
			return
		}
		s.admitMu.Lock()
		if s.cfg.ReplaceExistingPeer {
			s.replaceActivePeer()
		}

		peer, err := s.newPeer(connection)
		s.admitMu.Unlock()
		if err != nil {
			code := "internal_error"
			status := websocket.CloseInternalServerErr
			if errors.Is(err, errPeerLimit) {
				code = "peer_limit"
				status = websocket.CloseTryAgainLater
			} else if errors.Is(err, errServiceUnavailable) {
				code = "service_unavailable"
				status = websocket.CloseGoingAway
			}
			_ = connection.SetWriteDeadline(time.Now().Add(defaultSignalingWriteTimeout))
			_ = connection.WriteJSON(signalResponse{
				Version: signalingVersion,
				Type:    signalTypeError,
				Error: &protocolError{
					Code:    code,
					Message: err.Error(),
				},
			})
			_ = connection.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(status, err.Error()),
				time.Now().Add(defaultSignalingWriteTimeout),
			)
			_ = connection.Close()
			return
		}

		peer.run(request.Context())
	})
}

// Run forwards encoded samples until the context ends or media stops.
func (s *Service) Run(ctx context.Context) error {
	s.runMu.Lock()
	if s.started {
		s.runMu.Unlock()
		return errors.New("WebRTC service has already been run")
	}
	s.started = true
	s.runMu.Unlock()

	defer s.Close()

	var audioSamples <-chan AudioSample
	if s.cfg.AudioEnabled {
		audioSamples = s.audio.Samples()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.ctx.Done():
			return nil
		case reason := <-s.keyframeRequests:
			if err := s.source.RequestKeyframe(); err != nil {
				s.logger.Debug("keyframe request was not applied",
					zap.String("reason", reason),
					zap.Error(err),
				)
			}
		case sample, ok := <-s.source.Samples():
			if !ok {
				if ctx.Err() != nil || s.ctx.Err() != nil {
					return nil
				}
				return errors.New("media sample stream stopped")
			}
			if sample.ProducedAt.IsZero() {
				s.logger.Warn("dropping media sample without a production timestamp")
				continue
			}
			for _, peer := range s.peerSnapshot() {
				if peer.videoCodec.ID == sample.Codec {
					peer.enqueueVideo(sample)
				}
			}
		case sample, ok := <-audioSamples:
			if !ok {
				if ctx.Err() != nil || s.ctx.Err() != nil {
					return nil
				}
				return errors.New("audio sample stream stopped")
			}
			if sample.Duration <= 0 {
				s.logger.Warn("dropping audio sample without a positive duration")
				continue
			}
			for _, peer := range s.peerSnapshot() {
				peer.enqueueAudio(sample)
			}
		}
	}
}

// Close stops signaling and closes every active peer.
func (s *Service) Close() {
	s.peersMu.Lock()
	if s.closed {
		closeDone := s.closeDone
		s.peersMu.Unlock()
		<-closeDone
		return
	}
	s.closed = true
	if s.reservations == 0 && !s.closeDoneClosed {
		close(s.closeDone)
		s.closeDoneClosed = true
	}
	s.cancel()
	s.source.SetActive(false)
	peers := make([]*peer, 0, len(s.peers))
	for peer := range s.peers {
		peers = append(peers, peer)
	}
	s.peersMu.Unlock()

	for _, peer := range peers {
		peer.closeWith(websocket.CloseGoingAway, "service stopping")
	}
	deadline := time.NewTimer(servicePeerCloseTimeout)
	defer deadline.Stop()
	select {
	case <-s.closeDone:
	case <-deadline.C:
		s.logger.Warn("timed out closing WebRTC peers", zap.Int("peer_count", len(peers)))
	}

}

// PeerCount returns the number of active and initializing peers.
func (s *Service) PeerCount() int {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	return s.reservations
}

func videoCodecCapability(codec RTPCodec) pion.RTPCodecCapability {
	feedback := make([]pion.RTCPFeedback, len(codec.RTCPFeedback))
	for index, item := range codec.RTCPFeedback {
		feedback[index] = pion.RTCPFeedback{Type: item.Type, Parameter: item.Parameter}
	}
	return pion.RTPCodecCapability{
		MimeType:     codec.MimeType,
		ClockRate:    codec.ClockRate,
		Channels:     codec.Channels,
		SDPFmtpLine:  codec.SDPFmtpLine,
		RTCPFeedback: feedback,
	}
}

func (s *Service) newPeerConnection(
	codec RTPCodec,
	videoCodec pion.RTPCodecCapability,
) (*pion.PeerConnection, error) {
	mediaEngine := &pion.MediaEngine{}
	if err := mediaEngine.RegisterCodec(pion.RTPCodecParameters{
		RTPCodecCapability: videoCodec,
		PayloadType:        pion.PayloadType(codec.PayloadType),
	}, pion.RTPCodecTypeVideo); err != nil {
		return nil, fmt.Errorf("register video codec: %w", err)
	}
	if s.cfg.AudioEnabled {
		if err := mediaEngine.RegisterCodec(pion.RTPCodecParameters{
			RTPCodecCapability: s.audioCodec,
			PayloadType:        opusPayloadType,
		}, pion.RTPCodecTypeAudio); err != nil {
			return nil, fmt.Errorf("register audio codec: %w", err)
		}
	}

	registry := &interceptor.Registry{}
	if err := pion.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		return nil, fmt.Errorf("configure WebRTC interceptors: %w", err)
	}

	var settings pion.SettingEngine
	if s.cfg.UDPPortMin != 0 {
		if err := settings.SetEphemeralUDPPortRange(s.cfg.UDPPortMin, s.cfg.UDPPortMax); err != nil {
			return nil, fmt.Errorf("set ICE UDP port range: %w", err)
		}
	}

	configuration := pion.Configuration{}
	if len(s.cfg.ICEServers) > 0 {
		iceServer := pion.ICEServer{
			URLs: append([]string(nil), s.cfg.ICEServers...),
		}
		if s.cfg.ICEUsername != "" {
			iceServer.Username = s.cfg.ICEUsername
			iceServer.Credential = s.cfg.ICECredential
			iceServer.CredentialType = pion.ICECredentialTypePassword
		}
		configuration.ICEServers = []pion.ICEServer{iceServer}
	}

	connection, err := pion.NewAPI(
		pion.WithMediaEngine(mediaEngine),
		pion.WithSettingEngine(settings),
		pion.WithInterceptorRegistry(registry),
	).NewPeerConnection(configuration)
	if err != nil {
		return nil, err
	}
	return connection, nil
}

func (s *Service) reservePeer() (uint64, error) {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()

	if s.closed {
		return 0, errServiceUnavailable
	}
	if s.reservations >= s.cfg.MaxPeers {
		return 0, errPeerLimit
	}
	s.nextID++
	s.reservations++
	return s.nextID, nil
}

func (s *Service) registerPeer(peer *peer) error {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()

	if s.closed {
		return errServiceUnavailable
	}
	first := len(s.peers) == 0
	s.peers[peer] = struct{}{}
	if first {
		s.source.SetActive(true)
	}
	return nil
}

func (s *Service) releaseReservation() {
	s.peersMu.Lock()
	s.reservations--
	if s.closed && s.reservations == 0 && !s.closeDoneClosed {
		close(s.closeDone)
		s.closeDoneClosed = true
	}
	s.peersMu.Unlock()
}

func (s *Service) removePeer(peer *peer) {
	s.peersMu.Lock()
	if _, ok := s.peers[peer]; ok {
		delete(s.peers, peer)
		if len(s.peers) == 0 {
			s.source.SetActive(false)
		}
	}
	s.peersMu.Unlock()
}

func (s *Service) peerSnapshot() []*peer {
	s.peersMu.Lock()
	defer s.peersMu.Unlock()
	peers := make([]*peer, 0, len(s.peers))
	for peer := range s.peers {
		if !peer.isClosing() {
			peers = append(peers, peer)
		}
	}
	return peers
}

func (s *Service) replaceActivePeer() {
	peers := s.peerSnapshot()
	for _, peer := range peers {
		peer.closeWith(websocket.CloseNormalClosure, "replaced by a new peer")
	}
	for _, peer := range peers {
		select {
		case <-peer.done:
		case <-time.After(servicePeerCloseTimeout):
			return
		}
	}
}

func (s *Service) peerInfo(id uint64) PeerInfo {
	return PeerInfo{ID: id, ActivePeers: s.PeerCount()}
}

func (s *Service) closePeerForProfileChange(peer *peer, generation uint64) {
	s.qualityMu.Lock()
	current := s.qualityGeneration == generation
	s.qualityMu.Unlock()
	if current {
		peer.closeWith(websocket.CloseServiceRestart, "video profile changed")
	}
}

func (s *Service) requestKeyframe(reason string) {
	select {
	case s.keyframeRequests <- reason:
	default:
	}
}

func (s *Service) originAllowed(request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if len(s.cfg.AllowedOrigins) == 0 {
		parsed, err := url.Parse(origin)
		return err == nil &&
			(parsed.Scheme == "http" || parsed.Scheme == "https") &&
			parsed.Host == request.Host
	}
	for _, allowed := range s.cfg.AllowedOrigins {
		if allowed == "*" || allowed == origin {
			return true
		}
	}
	return false
}
