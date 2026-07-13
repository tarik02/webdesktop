// Package media captures and encodes the selected desktop stream.
package media

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gst/go-gst/gst"
	"github.com/tarik02/webdesktop/capture"
	"go.uber.org/zap"
)

const (
	MinBitrateKbps = 100
)

// Quality contains runtime-adjustable video settings.
type Quality struct {
	Profile     string `json:"profile"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Framerate   int    `json:"framerate"`
	BitrateKbps int    `json:"bitrate_kbps"`
}

// Tuning contains static encoder settings.
type Tuning struct {
	Threads          int
	KeyframeInterval int
	VP8CPUUsed       int
}

// Config contains capture, quality, and encoder settings.
type Config struct {
	Capture  capture.Config
	Profiles map[string]EncoderProfile
	Quality  Quality
	Tuning   Tuning
}

// Sample is one encoded video frame ready for transport.
type Sample struct {
	Data       []byte
	Codec      string
	ProducedAt time.Time
	PTS        time.Duration
	Duration   time.Duration
	KeyFrame   bool
}

// Validate checks the implemented media behavior.
func (cfg Config) Validate() error {
	var errs []error

	if err := cfg.Capture.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateProfiles(cfg.Profiles, cfg.Quality, cfg.Tuning); err != nil {
		errs = append(errs, err)
	}

	if cfg.Tuning.Threads < 1 || cfg.Tuning.Threads > 64 {
		errs = append(errs, errors.New("video tuning threads must be between 1 and 64"))
	}
	if cfg.Tuning.KeyframeInterval < 1 || cfg.Tuning.KeyframeInterval > 600 {
		errs = append(errs, errors.New("video tuning keyframe interval must be between 1 and 600"))
	}
	if cfg.Tuning.VP8CPUUsed < 0 || cfg.Tuning.VP8CPUUsed > 16 {
		errs = append(errs, errors.New("video tuning VP8 CPU used must be between 0 and 16"))
	}

	return errors.Join(errs...)
}

// ValidateProfiles checks all profile definitions and the selected quality.
func ValidateProfiles(profiles map[string]EncoderProfile, quality Quality, tuning Tuning) error {
	var errs []error
	if len(profiles) == 0 {
		errs = append(errs, errors.New("at least one video profile is required"))
	}
	for name, profile := range profiles {
		profileQuality := quality
		profileQuality.Profile = name
		if err := profile.Validate(name, profileQuality, tuning); err != nil {
			errs = append(errs, err)
		}
		for otherName, otherProfile := range profiles {
			if name < otherName && profile.Codec.ID == otherProfile.Codec.ID && !profile.Codec.Compatible(otherProfile.Codec) {
				errs = append(errs, fmt.Errorf(
					"video profiles %q and %q use codec id %q with different WebRTC metadata",
					name,
					otherName,
					profile.Codec.ID,
				))
			}
		}
	}
	if err := quality.Validate(profiles); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Validate checks runtime video quality settings.
func (quality Quality) Validate(profiles map[string]EncoderProfile) error {
	var errs []error
	profile, exists := profiles[quality.Profile]
	if !exists {
		errs = append(errs, fmt.Errorf("video profile %q is not configured", quality.Profile))
	}

	if quality.Width < 320 || quality.Width > 7680 || quality.Width%2 != 0 {
		errs = append(errs, errors.New("video width must be an even number between 320 and 7680"))
	}
	if quality.Height < 240 || quality.Height > 4320 || quality.Height%2 != 0 {
		errs = append(errs, errors.New("video height must be an even number between 240 and 4320"))
	}
	if quality.Framerate < 1 || quality.Framerate > 120 {
		errs = append(errs, errors.New("video framerate must be between 1 and 120"))
	}
	if quality.BitrateKbps < MinBitrateKbps {
		errs = append(errs, fmt.Errorf(
			"video bitrate must be at least %d Kbit/s",
			MinBitrateKbps,
		))
	}
	if exists {
		errs = append(errs, profile.Limits.Validate(quality.Profile, quality))
	}

	return errors.Join(errs...)
}

// Validate checks the profile-specific quality limits.
func (limits QualityLimits) Validate(profileName string, quality Quality) error {
	widthMacroblocks := (quality.Width + 15) / 16
	heightMacroblocks := (quality.Height + 15) / 16
	macroblocks := widthMacroblocks * heightMacroblocks
	var errs []error
	if limits.MaxMacroblocksPerDimension > 0 && widthMacroblocks > limits.MaxMacroblocksPerDimension {
		errs = append(errs, fmt.Errorf(
			"video profile %q width must not exceed %d macroblocks; width %d requires %d",
			profileName,
			limits.MaxMacroblocksPerDimension,
			quality.Width,
			widthMacroblocks,
		))
	}
	if limits.MaxMacroblocksPerDimension > 0 && heightMacroblocks > limits.MaxMacroblocksPerDimension {
		errs = append(errs, fmt.Errorf(
			"video profile %q height must not exceed %d macroblocks; height %d requires %d",
			profileName,
			limits.MaxMacroblocksPerDimension,
			quality.Height,
			heightMacroblocks,
		))
	}
	if limits.MaxMacroblocksPerFrame > 0 && macroblocks > limits.MaxMacroblocksPerFrame {
		errs = append(errs, fmt.Errorf(
			"video profile %q supports at most %d macroblocks per frame; %dx%d requires %d",
			profileName,
			limits.MaxMacroblocksPerFrame,
			quality.Width,
			quality.Height,
			macroblocks,
		))
	}
	if limits.MaxMacroblocksPerSecond > 0 && macroblocks*quality.Framerate > limits.MaxMacroblocksPerSecond {
		errs = append(errs, fmt.Errorf(
			"video profile %q supports at most %d macroblocks per second; %dx%d at %d fps requires %d",
			profileName,
			limits.MaxMacroblocksPerSecond,
			quality.Width,
			quality.Height,
			quality.Framerate,
			macroblocks*quality.Framerate,
		))
	}
	if limits.MaxBitrateKbps > 0 && quality.BitrateKbps > limits.MaxBitrateKbps {
		errs = append(errs, fmt.Errorf(
			"video profile %q bitrate must not exceed %d Kbit/s",
			profileName,
			limits.MaxBitrateKbps,
		))
	}
	return errors.Join(errs...)
}

var gstInitOnce sync.Once

const videoPipelineReadyTimeout = 5 * time.Second

// Service owns portal capture and the active encoder pipeline.
type Service struct {
	cfg    Config
	logger *zap.Logger

	mu           sync.Mutex
	started      bool
	running      bool
	session      *capture.Session
	pipeline     *persistentVideoPipeline
	quality      Quality
	samples      chan Sample
	stopSamples  chan struct{}
	closeSamples sync.Once
	updateMu     sync.Mutex
	active       atomic.Bool
}

// New constructs the media service without requesting portal authorization.
func New(cfg Config, logger *zap.Logger) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	gstInitOnce.Do(func() {
		gst.Init(nil)
	})

	return &Service{
		cfg:         cfg,
		logger:      logger,
		quality:     cfg.Quality,
		samples:     make(chan Sample),
		stopSamples: make(chan struct{}),
	}, nil
}

// Run encodes one already-authorized portal session until the context is canceled.
func (s *Service) Run(ctx context.Context, session *capture.Session) (runErr error) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("media service has already been run")
	}
	s.started = true
	s.mu.Unlock()
	if session == nil {
		return errors.New("portal capture session is required")
	}

	defer s.closeSamples.Do(func() {
		close(s.samples)
	})

	stream, err := session.AcquireStream()
	if err != nil {
		return fmt.Errorf("acquire desktop capture stream: %w", err)
	}
	pipeline, err := newPersistentVideoPipeline(
		stream,
		s.cfg.Quality,
		s.cfg.Profiles[s.cfg.Quality.Profile],
		s.cfg.Tuning,
		s.emitSample,
		&s.active,
		s.logger.Named("gstreamer"),
	)
	if err != nil {
		return fmt.Errorf("build video pipeline: %w", err)
	}
	if err := pipeline.Start(); err != nil {
		return fmt.Errorf("start video pipeline: %w", err)
	}

	s.mu.Lock()
	s.session = session
	s.pipeline = pipeline
	s.running = true
	s.mu.Unlock()

	s.logger.Info("desktop capture started",
		zap.Uint32("pipewire_node_id", stream.NodeID),
		zap.String("profile", s.cfg.Quality.Profile),
		zap.String("codec", s.cfg.Profiles[s.cfg.Quality.Profile].Codec.ID),
		zap.Int("width", s.cfg.Quality.Width),
		zap.Int("height", s.cfg.Quality.Height),
		zap.Int("framerate", s.cfg.Quality.Framerate),
		zap.Int("bitrate_kbps", s.cfg.Quality.BitrateKbps),
	)

	defer func() {
		close(s.stopSamples)

		s.mu.Lock()
		current := s.pipeline
		s.pipeline = nil
		s.session = nil
		s.running = false
		s.mu.Unlock()

		if current != nil {
			runErr = errors.Join(runErr, current.Close())
		}
		s.logger.Info("desktop capture stopped")
	}()

	for {
		s.mu.Lock()
		current := s.pipeline
		s.mu.Unlock()

		if current == nil {
			return errors.New("video pipeline is not running")
		}

		select {
		case <-ctx.Done():
			return nil
		case <-session.Done():
			if err := session.Err(); err != nil {
				return err
			}
			return errors.New("desktop capture session stopped")
		case <-current.Done():
			s.mu.Lock()
			active := s.pipeline
			s.mu.Unlock()

			if active != current {
				continue
			}
			if err := current.Err(); err != nil {
				return err
			}
			return errors.New("video pipeline stopped")
		}
	}
}

// Samples returns encoded frames from the active pipeline.
func (s *Service) Samples() <-chan Sample {
	return s.samples
}

// SetActive controls whether capture frames are delivered to the video encoder.
func (s *Service) SetActive(active bool) {
	previous := s.active.Swap(active)
	if !active || previous {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.pipeline == nil {
		return
	}
	if err := s.pipeline.RequestKeyframe(); err != nil {
		s.logger.Debug("keyframe request after video delivery resumed was not applied",
			zap.Error(err),
		)
	}
}

// Quality returns the configured or active runtime quality.
func (s *Service) Quality() Quality {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quality
}

// Profile returns one configured encoder profile.
func (s *Service) Profile(name string) (EncoderProfile, bool) {
	profile, exists := s.cfg.Profiles[name]
	return profile, exists
}

// UpdateQuality applies bitrate-only changes live. All other quality changes
// replace only the encoder branch.
func (s *Service) UpdateQuality(quality Quality) error {
	if err := quality.Validate(s.cfg.Profiles); err != nil {
		return err
	}

	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	s.mu.Lock()
	if !s.running || s.pipeline == nil || s.session == nil {
		s.mu.Unlock()
		return errors.New("media service is not running")
	}
	pipeline := s.pipeline
	current := s.quality
	s.mu.Unlock()

	if quality == current {
		return nil
	}

	started := time.Now()
	bitrateOnly := quality.Profile == current.Profile &&
		quality.Width == current.Width &&
		quality.Height == current.Height &&
		quality.Framerate == current.Framerate
	if !bitrateOnly {
		if err := pipeline.ReplaceQuality(
			quality,
			s.cfg.Profiles[quality.Profile],
			s.cfg.Tuning,
			videoPipelineReadyTimeout,
		); err != nil {
			return fmt.Errorf("replace video encoder branch: %w", err)
		}
	} else if err := pipeline.SetBitrate(quality.BitrateKbps); err != nil {
		return fmt.Errorf("update live encoder bitrate: %w", err)
	}

	s.mu.Lock()
	if !s.running || s.pipeline != pipeline || s.session == nil {
		s.mu.Unlock()
		return errors.New("media service stopped during video quality update")
	}
	s.quality = quality
	s.mu.Unlock()

	if bitrateOnly {
		s.logger.Info("video bitrate updated",
			zap.String("profile", quality.Profile),
			zap.Int("bitrate_kbps", quality.BitrateKbps),
			zap.Duration("duration", time.Since(started)),
		)
		return nil
	}
	s.logger.Info("video quality updated",
		zap.String("profile", quality.Profile),
		zap.String("codec", s.cfg.Profiles[quality.Profile].Codec.ID),
		zap.Int("width", quality.Width),
		zap.Int("height", quality.Height),
		zap.Int("framerate", quality.Framerate),
		zap.Int("bitrate_kbps", quality.BitrateKbps),
		zap.Duration("duration", time.Since(started)),
	)
	return nil
}

// RequestKeyframe asks the active encoder to emit a new independently decodable frame.
func (s *Service) RequestKeyframe() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running || s.pipeline == nil {
		return errors.New("media service is not running")
	}
	if err := s.pipeline.RequestKeyframe(); err != nil {
		return fmt.Errorf("request encoder keyframe: %w", err)
	}
	return nil
}

func (s *Service) emitSample(stop <-chan struct{}, sample Sample) bool {
	select {
	case s.samples <- sample:
		return true
	case <-stop:
		return false
	case <-s.stopSamples:
		return false
	}
}
