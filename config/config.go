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

	VideoProfileVP8          = "vp8"
	VideoProfileH264VAAPI    = "h264-vaapi"
	VideoProfileH264Software = "h264-software"

	AudioDefaultMonitor = media.DefaultAudioMonitor
	opusPayloadType     = 111
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
	Source     string                          `mapstructure:"source" yaml:"source"`
	CursorMode string                          `mapstructure:"cursor_mode" yaml:"cursor_mode"`
	Profile    string                          `mapstructure:"profile" yaml:"profile"`
	Option     string                          `mapstructure:"option" yaml:"option"`
	Profiles   map[string]media.EncoderProfile `mapstructure:"profiles" yaml:"profiles"`
	Tuning     VideoTuning                     `mapstructure:"tuning" yaml:"tuning"`
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
	Locking   bool `mapstructure:"locking" yaml:"locking"`
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
	SignalingPath       string   `mapstructure:"signaling_path" yaml:"signaling_path"`
	MaxPeers            int      `mapstructure:"max_peers" yaml:"max_peers"`
	ReplaceExistingPeer bool     `mapstructure:"replace_existing_peer" yaml:"replace_existing_peer"`
	ICEServers          []string `mapstructure:"ice_servers" yaml:"ice_servers"`
	ICEUsername         string   `mapstructure:"ice_username" yaml:"ice_username"`
	ICECredential       string   `mapstructure:"ice_credential" yaml:"ice_credential"`
	UDPPortMin          int      `mapstructure:"udp_port_min" yaml:"udp_port_min"`
	UDPPortMax          int      `mapstructure:"udp_port_max" yaml:"udp_port_max"`
	AllowedOrigins      []string `mapstructure:"allowed_origins" yaml:"allowed_origins"`
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
			Source:     VideoSourceMonitor,
			CursorMode: VideoCursorModeEmbedded,
			Profile:    VideoProfileVP8,
			Option:     "balanced",
			Profiles:   DefaultVideoProfiles(),
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
			Locking:   false,
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

// DefaultVideoProfiles returns the built-in runtime-configurable encoder profiles.
func DefaultVideoProfiles() map[string]media.EncoderProfile {
	feedback := []media.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
		{Type: "ccm", Parameter: "fir"},
	}
	h264Codec := media.RTPCodec{
		ID:           "h264",
		MimeType:     "video/H264",
		ClockRate:    90000,
		PayloadType:  102,
		Payloader:    media.PayloaderH264,
		SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e02a",
		RTCPFeedback: feedback,
		SDP: media.SDPRequirements{
			OfferFmtp: map[string]string{
				"packetization-mode": "^1$",
				"profile-level-id":   "(?i)^42e0[0-9a-f]{2}$",
			},
			AnswerFmtp: map[string]string{
				"profile-level-id": "42e02a",
			},
		},
	}
	h264Limits := media.QualityLimits{
		MaxBitrateKbps:             50000,
		MaxMacroblocksPerDimension: 263,
		MaxMacroblocksPerFrame:     8704,
		MaxMacroblocksPerSecond:    522240,
	}
	qualityOptions := func() map[string]media.QualityOption {
		return map[string]media.QualityOption{
			"low": {
				Label:       "720p 30 FPS",
				Width:       1280,
				Height:      720,
				Framerate:   30,
				BitrateKbps: 2500,
			},
			"balanced": {
				Label:       "1080p 30 FPS",
				Width:       1920,
				Height:      1080,
				Framerate:   30,
				BitrateKbps: 4000,
			},
			"high": {
				Label:       "1080p 60 FPS",
				Width:       1920,
				Height:      1080,
				Framerate:   60,
				BitrateKbps: 8000,
			},
			"maximum": {
				Label:       "1080p 60 FPS",
				Width:       1920,
				Height:      1080,
				Framerate:   60,
				BitrateKbps: 10000,
			},
		}
	}
	return map[string]media.EncoderProfile{
		VideoProfileVP8: {
			Label:             "VP8",
			DefaultOption:     "balanced",
			Options:           qualityOptions(),
			FrontendTransform: media.FrontendTransformNone,
			EncoderElement:    "encoder",
			Pipeline: `videoconvert !
videoscale method=nearest-neighbour !
video/x-raw,format=I420,width={{ .Width }},height={{ .Height }},framerate={{ .Framerate }}/1 !
vp8enc name={{ element "encoder" }}
  target-bitrate={{ mul .BitrateKbps 1000 }}
  cpu-used={{ .VP8CPUUsed }}
  threads={{ .Threads }}
  deadline=1
  buffer-initial-size=6144
  buffer-optimal-size=9216
  buffer-size=12288
  undershoot=95
  min-quantizer=4
  max-quantizer=20
  error-resilient=default
  keyframe-max-dist={{ .KeyframeInterval }}
  end-usage=cbr !
video/x-vp8`,
			Bitrate: []media.EncoderProperty{
				{
					Element:  "encoder",
					Property: "target-bitrate",
					Type:     media.PropertyTypeInt,
					Value:    `{{ mul .BitrateKbps 1000 }}`,
				},
			},
			Codec: media.RTPCodec{
				ID:           "vp8",
				MimeType:     "video/VP8",
				ClockRate:    90000,
				PayloadType:  96,
				Payloader:    media.PayloaderVP8,
				RTCPFeedback: feedback,
				SDP: media.SDPRequirements{
					OfferFmtp:  map[string]string{},
					AnswerFmtp: map[string]string{},
				},
			},
			Limits: media.QualityLimits{MaxBitrateKbps: 2147483},
		},
		VideoProfileH264VAAPI: {
			Label:             "H.264 (VA-API)",
			DefaultOption:     "balanced",
			Options:           qualityOptions(),
			FrontendTransform: media.FrontendTransformNone,
			EncoderElement:    "encoder",
			Pipeline: `vapostproc name={{ element "postproc" }} qos=true scale-method=fast !
video/x-raw(memory:VAMemory),format=NV12,width={{ .Width }},height={{ .Height }},framerate={{ .Framerate }}/1 !
vah264enc name={{ element "encoder" }}
  bitrate={{ .BitrateKbps }}
  cpb-size={{ ceilDiv (mul .BitrateKbps 3) .Framerate }}
  key-int-max={{ .KeyframeInterval }}
  b-frames=0
  cabac=false
  dct8x8=false
  mbbrc=disabled
  num-slices=4
  ref-frames=1
  target-usage=6
  rate-control=cbr
  cc-insert=false !
h264parse name={{ element "parser" }} config-interval=-1 !
video/x-h264,stream-format=byte-stream,alignment=au,profile=constrained-baseline`,
			Bitrate: []media.EncoderProperty{
				{
					Element:  "encoder",
					Property: "cpb-size",
					Type:     media.PropertyTypeUint,
					Value:    `{{ ceilDiv (mul .BitrateKbps 3) .Framerate }}`,
				},
				{
					Element:  "encoder",
					Property: "bitrate",
					Type:     media.PropertyTypeUint,
					Value:    `{{ .BitrateKbps }}`,
				},
			},
			Codec:  h264Codec,
			Limits: h264Limits,
		},
		VideoProfileH264Software: {
			Label:             "H.264 (software)",
			DefaultOption:     "balanced",
			Options:           qualityOptions(),
			FrontendTransform: media.FrontendTransformNone,
			EncoderElement:    "encoder",
			Pipeline: `videoconvert !
videoscale method=nearest-neighbour !
video/x-raw,format=I420,width={{ .Width }},height={{ .Height }},framerate={{ .Framerate }}/1 !
x264enc name={{ element "encoder" }}
  option-string=level=4.2
  bitrate={{ .BitrateKbps }}
  vbv-buf-capacity={{ ceilDiv 3000 .Framerate }}
  key-int-max={{ .KeyframeInterval }}
  threads={{ .Threads }}
  speed-preset=ultrafast
  tune=zerolatency
  pass=cbr
  bframes=0
  cabac=false
  dct8x8=false
  ref=1
  sliced-threads=true
  byte-stream=true !
h264parse name={{ element "parser" }} config-interval=-1 !
video/x-h264,stream-format=byte-stream,alignment=au,profile=constrained-baseline,level=(string)4.2`,
			Bitrate: []media.EncoderProperty{
				{
					Element:  "encoder",
					Property: "bitrate",
					Type:     media.PropertyTypeUint,
					Value:    `{{ .BitrateKbps }}`,
				},
			},
			Codec:  h264Codec,
			Limits: h264Limits,
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

	selectedQuality := media.Quality{Profile: cfg.Video.Profile, Option: cfg.Video.Option}
	if profile, exists := cfg.Video.Profiles[cfg.Video.Profile]; exists {
		if option, exists := profile.Options[cfg.Video.Option]; exists {
			selectedQuality = option.Quality(cfg.Video.Profile, cfg.Video.Option)
		}
	}
	errs = append(errs, media.ValidateProfiles(
		cfg.Video.Profiles,
		selectedQuality,
		media.Tuning{
			Threads:          cfg.Video.Tuning.Threads,
			KeyframeInterval: cfg.Video.Tuning.KeyframeInterval,
			VP8CPUUsed:       cfg.Video.Tuning.VP8CPUUsed,
		},
	))
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
	if cfg.Audio.Enabled {
		for name, profile := range cfg.Video.Profiles {
			if profile.Codec.PayloadType == opusPayloadType {
				errs = append(errs, fmt.Errorf("video profile %q payload type conflicts with Opus", name))
			}
		}
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
	if cfg.WebRTC.ReplaceExistingPeer && cfg.WebRTC.MaxPeers != 1 {
		errs = append(errs, errors.New("webrtc.replace_existing_peer requires webrtc.max_peers to be 1"))
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
