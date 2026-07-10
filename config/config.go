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
)

// Config contains the resolved service configuration.
type Config struct {
	Server  Server  `mapstructure:"server" yaml:"server"`
	Logging Logging `mapstructure:"logging" yaml:"logging"`
	Video   Video   `mapstructure:"video" yaml:"video"`
	WebRTC  WebRTC  `mapstructure:"webrtc" yaml:"webrtc"`
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
				Threads:          4,
				KeyframeInterval: 60,
				VP8CPUUsed:       8,
				H264SpeedPreset:  "veryfast",
			},
		},
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
	if cfg.Video.BitrateKbps < 100 || cfg.Video.BitrateKbps > 100000 {
		errs = append(errs, errors.New("video.bitrate_kbps must be between 100 and 100000"))
	}
	if cfg.Video.Codec == VideoCodecH264 {
		errs = append(errs, media.ValidateH264Level4(media.Quality{
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

	switch cfg.Video.Tuning.H264SpeedPreset {
	case "ultrafast", "superfast", "veryfast", "faster", "fast", "medium":
	default:
		errs = append(errs, errors.New("video.tuning.h264_speed_preset must be ultrafast, superfast, veryfast, faster, fast, or medium"))
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
