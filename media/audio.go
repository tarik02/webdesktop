package media

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"
	"go.uber.org/zap"
)

const DefaultAudioMonitor = "@DEFAULT_MONITOR@"

// AudioConfig contains the implemented desktop audio settings.
type AudioConfig struct {
	Enabled     bool
	Device      string
	BitrateKbps int
}

// AudioSample is one encoded Opus frame ready for transport. PTS and Duration define the media timeline; transport does not derive RTP time from wall-clock production.
type AudioSample struct {
	Data     []byte
	PTS      time.Duration
	Duration time.Duration
}

// Validate checks the implemented desktop audio behavior.
func (cfg AudioConfig) Validate() error {
	var errs []error
	if cfg.Device != DefaultAudioMonitor && !strings.HasSuffix(cfg.Device, ".monitor") {
		errs = append(errs, errors.New("audio device must be @DEFAULT_MONITOR@ or a PulseAudio monitor source ending in .monitor"))
	}
	if cfg.BitrateKbps < 6 || cfg.BitrateKbps > 510 {
		errs = append(errs, errors.New("audio bitrate must be between 6 and 510 Kbit/s"))
	}
	return errors.Join(errs...)
}

// AudioService owns the optional desktop audio pipeline.
type AudioService struct {
	cfg    AudioConfig
	logger *zap.Logger

	mu      sync.Mutex
	started bool
	samples chan AudioSample
}

// NewAudio constructs the desktop audio service without opening an audio device.
func NewAudio(cfg AudioConfig, logger *zap.Logger) (*AudioService, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		return nil, errors.New("audio logger is required")
	}

	gstInitOnce.Do(func() {
		gst.Init(nil)
	})

	return &AudioService{
		cfg:     cfg,
		logger:  logger,
		samples: make(chan AudioSample, 32),
	}, nil
}

// Enabled reports whether audio capture is configured.
func (s *AudioService) Enabled() bool {
	return s.cfg.Enabled
}

// Samples returns encoded Opus frames from the active pipeline.
func (s *AudioService) Samples() <-chan AudioSample {
	return s.samples
}

// Run waits for portal authorization, then captures audio until cancellation.
func (s *AudioService) Run(ctx context.Context, authorized <-chan struct{}) (runErr error) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("audio service has already been run")
	}
	s.started = true
	s.mu.Unlock()

	defer close(s.samples)
	if !s.cfg.Enabled {
		return nil
	}
	if authorized == nil {
		return errors.New("portal authorization readiness is required")
	}

	select {
	case <-ctx.Done():
		return nil
	case <-authorized:
	}
	if ctx.Err() != nil {
		return nil
	}

	pipeline, err := newAudioPipeline(s.cfg, s.emitSample, s.logger.Named("gstreamer"))
	if err != nil {
		return fmt.Errorf("start audio pipeline: %w", err)
	}
	defer func() {
		runErr = errors.Join(runErr, pipeline.Close())
		s.logger.Info("desktop audio stopped")
	}()

	s.logger.Info("desktop audio started",
		zap.String("device", s.cfg.Device),
		zap.Int("sample_rate", 48000),
		zap.Int("channels", 2),
		zap.Int("bitrate_kbps", s.cfg.BitrateKbps),
		zap.Int("frame_duration_ms", 20),
	)

	select {
	case <-ctx.Done():
		return nil
	case <-pipeline.Done():
		if err := pipeline.Err(); err != nil {
			return err
		}
		return errors.New("audio pipeline stopped")
	}
}

func (s *AudioService) emitSample(sample AudioSample) {
	select {
	case s.samples <- sample:
	default:
	}
}
