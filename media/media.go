// Package media captures and encodes the selected desktop stream.
package media

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-gst/go-gst/gst"
	"github.com/tarik02/webdesktop/capture"
	"go.uber.org/zap"
)

const (
	CodecVP8  = "vp8"
	CodecH264 = "h264"
)

// Quality contains runtime-adjustable video settings.
type Quality struct {
	Codec       string
	Width       int
	Height      int
	Framerate   int
	BitrateKbps int
}

// Tuning contains static software encoder settings.
type Tuning struct {
	Threads          int
	KeyframeInterval int
	VP8CPUUsed       int
	H264SpeedPreset  string
}

// Config contains capture, quality, and encoder settings.
type Config struct {
	Capture capture.Config
	Quality Quality
	Tuning  Tuning
}

// Sample is one encoded video frame ready for a future transport.
type Sample struct {
	Data     []byte
	Codec    string
	PTS      time.Duration
	Duration time.Duration
	KeyFrame bool
}

// Validate checks the implemented media behavior.
func (cfg Config) Validate() error {
	var errs []error

	if err := cfg.Capture.Validate(); err != nil {
		errs = append(errs, err)
	}
	if err := cfg.Quality.Validate(); err != nil {
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

	switch cfg.Tuning.H264SpeedPreset {
	case "ultrafast", "superfast", "veryfast", "faster", "fast", "medium":
	default:
		errs = append(errs, errors.New("video tuning H.264 speed preset must be ultrafast, superfast, veryfast, faster, fast, or medium"))
	}

	return errors.Join(errs...)
}

// Validate checks runtime video quality settings.
func (quality Quality) Validate() error {
	var errs []error

	switch quality.Codec {
	case CodecVP8, CodecH264:
	default:
		errs = append(errs, errors.New("video codec must be vp8 or h264"))
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
	if quality.BitrateKbps < 100 || quality.BitrateKbps > 100000 {
		errs = append(errs, errors.New("video bitrate must be between 100 and 100000 Kbit/s"))
	}

	return errors.Join(errs...)
}

var gstInitOnce sync.Once

// Service owns portal capture and the active encoder pipeline.
type Service struct {
	cfg    Config
	logger *zap.Logger

	mu              sync.Mutex
	started         bool
	running         bool
	session         *capture.Session
	pipeline        *videoPipeline
	pipelineErr     error
	quality         Quality
	pipelineChanged chan struct{}
	samples         chan Sample
	closeSamples    sync.Once
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
		cfg:             cfg,
		logger:          logger,
		quality:         cfg.Quality,
		pipelineChanged: make(chan struct{}, 1),
		samples:         make(chan Sample, 8),
	}, nil
}

// Run requests capture authorization and encodes until the context is canceled.
func (s *Service) Run(ctx context.Context) (runErr error) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("media service has already been run")
	}
	s.started = true
	s.mu.Unlock()

	defer s.closeSamples.Do(func() {
		close(s.samples)
	})

	session, err := capture.Open(ctx, s.cfg.Capture)
	if err != nil {
		return fmt.Errorf("open desktop capture: %w", err)
	}

	stream, err := session.AcquireStream()
	if err != nil {
		return errors.Join(fmt.Errorf("acquire desktop capture stream: %w", err), session.Close())
	}
	pipeline, err := newVideoPipeline(
		stream,
		s.cfg.Quality,
		s.cfg.Tuning,
		s.emitSample,
		s.logger.Named("gstreamer"),
	)
	if err != nil {
		return errors.Join(fmt.Errorf("start video pipeline: %w", err), session.Close())
	}

	s.mu.Lock()
	s.session = session
	s.pipeline = pipeline
	s.running = true
	s.mu.Unlock()

	s.logger.Info("desktop capture started",
		zap.Uint32("pipewire_node_id", stream.NodeID),
		zap.String("codec", s.cfg.Quality.Codec),
		zap.Int("width", s.cfg.Quality.Width),
		zap.Int("height", s.cfg.Quality.Height),
		zap.Int("framerate", s.cfg.Quality.Framerate),
		zap.Int("bitrate_kbps", s.cfg.Quality.BitrateKbps),
	)

	defer func() {
		s.mu.Lock()
		current := s.pipeline
		s.pipeline = nil
		s.session = nil
		s.running = false
		s.mu.Unlock()

		if current != nil {
			runErr = errors.Join(runErr, current.Close())
		}
		runErr = errors.Join(runErr, session.Close())
		s.logger.Info("desktop capture stopped")
	}()

	for {
		s.mu.Lock()
		current := s.pipeline
		pipelineErr := s.pipelineErr
		s.mu.Unlock()

		if pipelineErr != nil {
			return pipelineErr
		}
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
			pipelineErr := s.pipelineErr
			s.mu.Unlock()

			if active != current {
				continue
			}
			if pipelineErr != nil {
				return pipelineErr
			}
			if err := current.Err(); err != nil {
				return err
			}
			return errors.New("video pipeline stopped")
		case <-s.pipelineChanged:
		}
	}
}

// Samples returns encoded frames from the active pipeline.
func (s *Service) Samples() <-chan Sample {
	return s.samples
}

// Quality returns the configured or active runtime quality.
func (s *Service) Quality() Quality {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quality
}

// UpdateQuality applies bitrate live when supported and rebuilds for other changes.
func (s *Service) UpdateQuality(quality Quality) error {
	if err := quality.Validate(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running || s.pipeline == nil || s.session == nil {
		return errors.New("media service is not running")
	}
	if s.pipelineErr != nil {
		return s.pipelineErr
	}
	if quality == s.quality {
		return nil
	}

	if quality.Codec == s.quality.Codec &&
		quality.Width == s.quality.Width &&
		quality.Height == s.quality.Height &&
		quality.Framerate == s.quality.Framerate {
		if err := s.pipeline.SetBitrate(quality.BitrateKbps); err != nil {
			return fmt.Errorf("update live encoder bitrate: %w", err)
		}
		s.quality = quality
		s.logger.Info("video bitrate updated",
			zap.String("codec", quality.Codec),
			zap.Int("bitrate_kbps", quality.BitrateKbps),
		)
		return nil
	}

	if err := s.pipeline.Close(); err != nil {
		return fmt.Errorf("stop video pipeline for quality update: %w", err)
	}

	stream, err := s.session.AcquireStream()
	if err != nil {
		s.pipeline = nil
		s.pipelineErr = fmt.Errorf("acquire desktop capture stream for quality update: %w", err)
		select {
		case s.pipelineChanged <- struct{}{}:
		default:
		}
		return s.pipelineErr
	}
	replacement, err := newVideoPipeline(
		stream,
		quality,
		s.cfg.Tuning,
		s.emitSample,
		s.logger.Named("gstreamer"),
	)
	if err != nil {
		s.pipeline = nil
		s.pipelineErr = fmt.Errorf("rebuild video pipeline: %w", err)
		select {
		case s.pipelineChanged <- struct{}{}:
		default:
		}
		return s.pipelineErr
	}

	s.pipeline = replacement
	s.quality = quality
	select {
	case s.pipelineChanged <- struct{}{}:
	default:
	}

	s.logger.Info("video pipeline rebuilt",
		zap.String("codec", quality.Codec),
		zap.Int("width", quality.Width),
		zap.Int("height", quality.Height),
		zap.Int("framerate", quality.Framerate),
		zap.Int("bitrate_kbps", quality.BitrateKbps),
	)
	return nil
}

func (s *Service) emitSample(sample Sample) {
	select {
	case s.samples <- sample:
	default:
	}
}
