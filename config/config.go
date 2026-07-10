package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"go.uber.org/zap/zapcore"
)

const (
	LogFormatJSON    = "json"
	LogFormatConsole = "console"

	VideoSourceMonitor = "monitor"

	VideoCursorModeHidden   = "hidden"
	VideoCursorModeEmbedded = "embedded"

	VideoCodecVP8  = "vp8"
	VideoCodecH264 = "h264"
)

// Config contains the resolved service configuration.
type Config struct {
	Server  Server  `mapstructure:"server" yaml:"server"`
	Logging Logging `mapstructure:"logging" yaml:"logging"`
	Video   Video   `mapstructure:"video" yaml:"video"`
}

// Server contains HTTP server settings.
type Server struct {
	ListenAddress   string `mapstructure:"listen_address" yaml:"listen_address"`
	ShutdownTimeout string `mapstructure:"shutdown_timeout" yaml:"shutdown_timeout"`
}

// Logging contains structured logger settings.
type Logging struct {
	Level  string `mapstructure:"level" yaml:"level"`
	Format string `mapstructure:"format" yaml:"format"`
}

// Video contains portal capture and software encoding settings.
type Video struct {
	Source      string      `mapstructure:"source" yaml:"source"`
	CursorMode  string      `mapstructure:"cursor_mode" yaml:"cursor_mode"`
	Codec       string      `mapstructure:"codec" yaml:"codec"`
	Width       int         `mapstructure:"width" yaml:"width"`
	Height      int         `mapstructure:"height" yaml:"height"`
	Framerate   int         `mapstructure:"framerate" yaml:"framerate"`
	BitrateKbps int         `mapstructure:"bitrate_kbps" yaml:"bitrate_kbps"`
	Tuning      VideoTuning `mapstructure:"tuning" yaml:"tuning"`
}

// VideoTuning contains static software encoder settings.
type VideoTuning struct {
	Threads          int    `mapstructure:"threads" yaml:"threads"`
	KeyframeInterval int    `mapstructure:"keyframe_interval" yaml:"keyframe_interval"`
	VP8CPUUsed       int    `mapstructure:"vp8_cpu_used" yaml:"vp8_cpu_used"`
	H264SpeedPreset  string `mapstructure:"h264_speed_preset" yaml:"h264_speed_preset"`
}

// Defaults returns the built-in service configuration.
func Defaults() Config {
	return Config{
		Server: Server{
			ListenAddress:   "127.0.0.1:8080",
			ShutdownTimeout: "10s",
		},
		Logging: Logging{
			Level:  "info",
			Format: LogFormatJSON,
		},
		Video: Video{
			Source:      VideoSourceMonitor,
			CursorMode:  VideoCursorModeEmbedded,
			Codec:       VideoCodecVP8,
			Width:       1920,
			Height:      1080,
			Framerate:   30,
			BitrateKbps: 4000,
			Tuning: VideoTuning{
				Threads:          4,
				KeyframeInterval: 60,
				VP8CPUUsed:       8,
				H264SpeedPreset:  "veryfast",
			},
		},
	}
}

// Validate checks configuration before service startup.
func (cfg Config) Validate() error {
	var errs []error

	_, port, err := net.SplitHostPort(cfg.Server.ListenAddress)
	if err != nil {
		errs = append(errs, fmt.Errorf("server.listen_address: %w", err))
	} else if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		errs = append(errs, errors.New("server.listen_address port must be between 1 and 65535"))
	}

	if _, err := cfg.Server.ShutdownDuration(); err != nil {
		errs = append(errs, err)
	}

	var level zapcore.Level
	if err := level.UnmarshalText([]byte(cfg.Logging.Level)); err != nil {
		errs = append(errs, fmt.Errorf("logging.level: %w", err))
	}

	switch cfg.Logging.Format {
	case LogFormatJSON, LogFormatConsole:
	default:
		errs = append(errs, errors.New("logging.format must be json or console"))
	}

	if cfg.Video.Source != VideoSourceMonitor {
		errs = append(errs, errors.New("video.source must be monitor"))
	}

	switch cfg.Video.CursorMode {
	case VideoCursorModeHidden, VideoCursorModeEmbedded:
	default:
		errs = append(errs, errors.New("video.cursor_mode must be hidden or embedded"))
	}

	switch cfg.Video.Codec {
	case VideoCodecVP8, VideoCodecH264:
	default:
		errs = append(errs, errors.New("video.codec must be vp8 or h264"))
	}

	if cfg.Video.Width < 320 || cfg.Video.Width > 7680 || cfg.Video.Width%2 != 0 {
		errs = append(errs, errors.New("video.width must be an even number between 320 and 7680"))
	}
	if cfg.Video.Height < 240 || cfg.Video.Height > 4320 || cfg.Video.Height%2 != 0 {
		errs = append(errs, errors.New("video.height must be an even number between 240 and 4320"))
	}
	if cfg.Video.Framerate < 1 || cfg.Video.Framerate > 120 {
		errs = append(errs, errors.New("video.framerate must be between 1 and 120"))
	}
	if cfg.Video.BitrateKbps < 100 || cfg.Video.BitrateKbps > 100000 {
		errs = append(errs, errors.New("video.bitrate_kbps must be between 100 and 100000"))
	}
	if cfg.Video.Tuning.Threads < 1 || cfg.Video.Tuning.Threads > 64 {
		errs = append(errs, errors.New("video.tuning.threads must be between 1 and 64"))
	}
	if cfg.Video.Tuning.KeyframeInterval < 1 || cfg.Video.Tuning.KeyframeInterval > 600 {
		errs = append(errs, errors.New("video.tuning.keyframe_interval must be between 1 and 600"))
	}
	if cfg.Video.Tuning.VP8CPUUsed < 0 || cfg.Video.Tuning.VP8CPUUsed > 16 {
		errs = append(errs, errors.New("video.tuning.vp8_cpu_used must be between 0 and 16"))
	}

	switch cfg.Video.Tuning.H264SpeedPreset {
	case "ultrafast", "superfast", "veryfast", "faster", "fast", "medium":
	default:
		errs = append(errs, errors.New("video.tuning.h264_speed_preset must be ultrafast, superfast, veryfast, faster, fast, or medium"))
	}

	return errors.Join(errs...)
}

// ShutdownDuration parses the configured unit-bearing shutdown timeout.
func (cfg Server) ShutdownDuration() (time.Duration, error) {
	switch cfg.ShutdownTimeout {
	case "0", "+0", "-0":
		return 0, errors.New("server.shutdown_timeout must be a duration string with a unit")
	}

	duration, err := time.ParseDuration(cfg.ShutdownTimeout)
	if err != nil {
		return 0, fmt.Errorf("server.shutdown_timeout must be a duration string with a unit: %w", err)
	}
	if duration <= 0 {
		return 0, errors.New("server.shutdown_timeout must be positive")
	}
	return duration, nil
}
