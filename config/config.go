package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pion/stun/v3"
	"github.com/tarik02/webdesktop/media"
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

	AudioDefaultMonitor = media.DefaultAudioMonitor
)

// Config contains the resolved service configuration.
type Config struct {
	Server    Server    `mapstructure:"server" yaml:"server"`
	Logging   Logging   `mapstructure:"logging" yaml:"logging"`
	Tracing   Tracing   `mapstructure:"tracing" yaml:"tracing"`
	Video     Video     `mapstructure:"video" yaml:"video"`
	Audio     Audio     `mapstructure:"audio" yaml:"audio"`
	Input     Input     `mapstructure:"input" yaml:"input"`
	Clipboard Clipboard `mapstructure:"clipboard" yaml:"clipboard"`
	WebRTC    WebRTC    `mapstructure:"webrtc" yaml:"webrtc"`
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

// Tracing controls bounded WebRTC and browser diagnostics.
type Tracing struct {
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`
}

// Video contains portal capture and encoding settings.
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

// VideoTuning contains static encoder settings.
type VideoTuning struct {
	Threads          int `mapstructure:"threads" yaml:"threads"`
	KeyframeInterval int `mapstructure:"keyframe_interval" yaml:"keyframe_interval"`
	VP8CPUUsed       int `mapstructure:"vp8_cpu_used" yaml:"vp8_cpu_used"`
}

// Audio contains optional desktop audio capture settings.
type Audio struct {
	Enabled     bool   `mapstructure:"enabled" yaml:"enabled"`
	Device      string `mapstructure:"device" yaml:"device"`
	BitrateKbps int    `mapstructure:"bitrate_kbps" yaml:"bitrate_kbps"`
}

// Input contains static remote input settings.
type Input struct {
	Enabled   bool `mapstructure:"enabled" yaml:"enabled"`
	Pointer   bool `mapstructure:"pointer" yaml:"pointer"`
	Keyboard  bool `mapstructure:"keyboard" yaml:"keyboard"`
	QueueSize int  `mapstructure:"queue_size" yaml:"queue_size"`
}

// Clipboard contains desktop clipboard synchronization settings.
type Clipboard struct {
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`
}

// WebRTC contains static peer transport and signaling settings.
type WebRTC struct {
	SignalingPath  string   `mapstructure:"signaling_path" yaml:"signaling_path"`
	MaxPeers       int      `mapstructure:"max_peers" yaml:"max_peers"`
	ICEServers     []string `mapstructure:"ice_servers" yaml:"ice_servers"`
	ICEUsername    string   `mapstructure:"ice_username" yaml:"ice_username"`
	ICECredential  string   `mapstructure:"ice_credential" yaml:"ice_credential"`
	UDPPortMin     int      `mapstructure:"udp_port_min" yaml:"udp_port_min"`
	UDPPortMax     int      `mapstructure:"udp_port_max" yaml:"udp_port_max"`
	AllowedOrigins []string `mapstructure:"allowed_origins" yaml:"allowed_origins"`
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
				Threads:          8,
				KeyframeInterval: 60,
				VP8CPUUsed:       16,
			},
		},
		Audio: Audio{
			Enabled:     false,
			Device:      AudioDefaultMonitor,
			BitrateKbps: 128,
		},
		Input: Input{
			Enabled:   true,
			Pointer:   true,
			Keyboard:  true,
			QueueSize: 256,
		},
		Clipboard: Clipboard{Enabled: true},
		WebRTC: WebRTC{
			SignalingPath:  "/webrtc",
			MaxPeers:       2,
			ICEServers:     []string{},
			AllowedOrigins: []string{},
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
	if cfg.Video.BitrateKbps < media.MinBitrateKbps {
		errs = append(errs, fmt.Errorf(
			"video.bitrate_kbps must be at least %d",
			media.MinBitrateKbps,
		))
	}
	if cfg.Video.Codec == VideoCodecVP8 && cfg.Video.BitrateKbps > media.VP8MaxBitrateKbps {
		errs = append(errs, fmt.Errorf(
			"video.bitrate_kbps must not exceed %d for VP8",
			media.VP8MaxBitrateKbps,
		))
	}
	if cfg.Video.Codec == VideoCodecH264 {
		errs = append(errs, media.ValidateH264Level42(media.Quality{
			Codec:       cfg.Video.Codec,
			Width:       cfg.Video.Width,
			Height:      cfg.Video.Height,
			Framerate:   cfg.Video.Framerate,
			BitrateKbps: cfg.Video.BitrateKbps,
		}))
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

	if cfg.Audio.Device != AudioDefaultMonitor && !strings.HasSuffix(cfg.Audio.Device, ".monitor") {
		errs = append(errs, errors.New("audio.device must be @DEFAULT_MONITOR@ or a PulseAudio monitor source ending in .monitor"))
	}
	if cfg.Audio.BitrateKbps < 6 || cfg.Audio.BitrateKbps > 510 {
		errs = append(errs, errors.New("audio.bitrate_kbps must be between 6 and 510"))
	}

	if cfg.Input.Enabled && !cfg.Input.Pointer && !cfg.Input.Keyboard {
		errs = append(errs, errors.New("input requires pointer or keyboard when enabled"))
	}
	if cfg.Clipboard.Enabled && (!cfg.Input.Enabled || !cfg.Input.Keyboard) {
		errs = append(errs, errors.New("clipboard.enabled requires input.enabled and input.keyboard"))
	}
	if cfg.Input.QueueSize < 16 || cfg.Input.QueueSize > 4096 {
		errs = append(errs, errors.New("input.queue_size must be between 16 and 4096"))
	}

	if cfg.WebRTC.SignalingPath == "" ||
		!strings.HasPrefix(cfg.WebRTC.SignalingPath, "/") ||
		path.Clean(cfg.WebRTC.SignalingPath) != cfg.WebRTC.SignalingPath ||
		cfg.WebRTC.SignalingPath == "/" {
		errs = append(errs, errors.New("webrtc.signaling_path must be a clean absolute path below /"))
	}
	if cfg.WebRTC.SignalingPath == "/healthz" {
		errs = append(errs, errors.New("webrtc.signaling_path must not replace /healthz"))
	}
	if cfg.WebRTC.MaxPeers < 1 || cfg.WebRTC.MaxPeers > 64 {
		errs = append(errs, errors.New("webrtc.max_peers must be between 1 and 64"))
	}
	if (cfg.WebRTC.ICEUsername == "") != (cfg.WebRTC.ICECredential == "") {
		errs = append(errs, errors.New("webrtc.ice_username and webrtc.ice_credential must both be set or both be empty"))
	}
	if len(cfg.WebRTC.ICEServers) == 0 && cfg.WebRTC.ICEUsername != "" {
		errs = append(errs, errors.New("webrtc ICE credentials require at least one ICE server"))
	}
	for _, server := range cfg.WebRTC.ICEServers {
		uri, err := stun.ParseURI(server)
		if err != nil {
			errs = append(errs, fmt.Errorf("webrtc.ice_servers contains invalid URL %q: %w", server, err))
			continue
		}
		switch uri.Scheme {
		case stun.SchemeTypeTURN, stun.SchemeTypeTURNS:
			if cfg.WebRTC.ICEUsername == "" {
				errs = append(errs, fmt.Errorf("webrtc.ice_servers TURN URL %q requires ICE credentials", server))
			}
		}
	}
	if (cfg.WebRTC.UDPPortMin == 0) != (cfg.WebRTC.UDPPortMax == 0) {
		errs = append(errs, errors.New("webrtc.udp_port_min and webrtc.udp_port_max must both be set or both be zero"))
	} else if cfg.WebRTC.UDPPortMin != 0 &&
		(cfg.WebRTC.UDPPortMin < 1 ||
			cfg.WebRTC.UDPPortMax > 65535 ||
			cfg.WebRTC.UDPPortMax < cfg.WebRTC.UDPPortMin) {
		errs = append(errs, errors.New("webrtc UDP port range must be between 1 and 65535 with min not greater than max"))
	}
	for _, origin := range cfg.WebRTC.AllowedOrigins {
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
			errs = append(errs, fmt.Errorf("webrtc.allowed_origins contains invalid origin %q", origin))
		}
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
