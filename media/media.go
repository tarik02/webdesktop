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
	CodecVP8  = "vp8"
	CodecH264 = "h264"

	MinBitrateKbps                 = 100
	VP8MaxBitrateKbps              = 2147483
	H264SDPProfileLevelID          = "42e02a"
	H264Level                      = "4.2"
	H264MaxMacroblocksPerDimension = 263
	H264MaxMacroblocksPerFrame     = 8704
	H264MaxMacroblocksPerSecond    = 522240
	H264MaxLevelBitrateKbps        = 50000
)

// Quality contains runtime-adjustable video settings.
type Quality struct {
	Codec       string `json:"codec"`
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
	Capture capture.Config
	Quality Quality
	Tuning  Tuning
}

// Sample is one encoded video frame ready for transport. ProducedAt records encoder latency instrumentation; PTS is diagnostic metadata and may be invalid.
type Sample struct {
	Data       []byte
	Codec      string
	ProducedAt time.Time
	PTS        time.Duration
	PTSValid   bool
	Duration   time.Duration
	KeyFrame   bool
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
	if quality.BitrateKbps < MinBitrateKbps {
		errs = append(errs, fmt.Errorf(
			"video bitrate must be at least %d Kbit/s",
			MinBitrateKbps,
		))
	}
	switch quality.Codec {
	case CodecVP8:
		if quality.BitrateKbps > VP8MaxBitrateKbps {
			errs = append(errs, fmt.Errorf(
				"VP8 bitrate must not exceed %d Kbit/s",
				VP8MaxBitrateKbps,
			))
		}
	case CodecH264:
		errs = append(errs, ValidateH264Level42(quality))
	}

	return errors.Join(errs...)
}

// ValidateH264Level42 checks constrained-baseline Level 4.2 limits.
func ValidateH264Level42(quality Quality) error {
	widthMacroblocks := (quality.Width + 15) / 16
	heightMacroblocks := (quality.Height + 15) / 16
	macroblocks := widthMacroblocks * heightMacroblocks
	var errs []error
	if widthMacroblocks > H264MaxMacroblocksPerDimension {
		errs = append(errs, fmt.Errorf(
			"H.264 Level 4.2 width must not exceed %d macroblocks; width %d requires %d",
			H264MaxMacroblocksPerDimension,
			quality.Width,
			widthMacroblocks,
		))
	}
	if heightMacroblocks > H264MaxMacroblocksPerDimension {
		errs = append(errs, fmt.Errorf(
			"H.264 Level 4.2 height must not exceed %d macroblocks; height %d requires %d",
			H264MaxMacroblocksPerDimension,
			quality.Height,
			heightMacroblocks,
		))
	}
	if macroblocks > H264MaxMacroblocksPerFrame {
		errs = append(errs, fmt.Errorf(
			"H.264 Level 4.2 supports at most %d macroblocks per frame; %dx%d requires %d",
			H264MaxMacroblocksPerFrame,
			quality.Width,
			quality.Height,
			macroblocks,
		))
	}
	if macroblocks*quality.Framerate > H264MaxMacroblocksPerSecond {
		errs = append(errs, fmt.Errorf(
			"H.264 Level 4.2 supports at most %d macroblocks per second; %dx%d at %d fps requires %d",
			H264MaxMacroblocksPerSecond,
			quality.Width,
			quality.Height,
			quality.Framerate,
			macroblocks*quality.Framerate,
		))
	}
	if quality.BitrateKbps > H264MaxLevelBitrateKbps {
		errs = append(errs, fmt.Errorf(
			"H.264 Level 4.2 bitrate must not exceed %d Kbit/s",
			H264MaxLevelBitrateKbps,
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
		zap.String("codec", s.cfg.Quality.Codec),
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

// UpdateQuality applies bitrate-only changes live. All other quality changes
// replace only the encoder branch.
func (s *Service) UpdateQuality(quality Quality) error {
	if err := quality.Validate(); err != nil {
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
	bitrateOnly := quality.Codec == current.Codec &&
		quality.Width == current.Width &&
		quality.Height == current.Height &&
		quality.Framerate == current.Framerate
	if !bitrateOnly {
		if err := pipeline.ReplaceQuality(quality, s.cfg.Tuning, videoPipelineReadyTimeout); err != nil {
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
			zap.String("codec", quality.Codec),
			zap.Int("bitrate_kbps", quality.BitrateKbps),
			zap.Duration("duration", time.Since(started)),
		)
		return nil
	}
	s.logger.Info("video quality updated",
		zap.String("codec", quality.Codec),
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
