package cli

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/tarik02/webdesktop/config"
	"github.com/tarik02/webdesktop/internal/app"
)

func newServeCommand() *cobra.Command {
	defaults := config.Defaults()
	var configFile string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the webdesktop streaming service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(cmd, configFile)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			application, err := app.New(cfg)
			if err != nil {
				return fmt.Errorf("initialize application: %w", err)
			}
			defer application.Close()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			return application.Serve(ctx)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&configFile, "config", "", "path to a YAML, TOML, or JSON config file")
	flags.String("listen-address", defaults.Server.ListenAddress, "HTTP listen address")
	flags.String("shutdown-timeout", defaults.Server.ShutdownTimeout, "graceful shutdown timeout with unit")
	flags.String("log-level", defaults.Logging.Level, "log level")
	flags.String("log-format", defaults.Logging.Format, "log format (json or console)")
	flags.Bool("tracing-enabled", defaults.Tracing.Enabled, "enable bounded WebRTC and browser diagnostics")
	flags.String("video-source", defaults.Video.Source, "portal video source (monitor)")
	flags.String("video-cursor-mode", defaults.Video.CursorMode, "captured cursor mode (hidden or embedded)")
	flags.String("video-codec", defaults.Video.Codec, "video codec (vp8 or h264)")
	flags.Int("video-width", defaults.Video.Width, "encoded video width")
	flags.Int("video-height", defaults.Video.Height, "encoded video height")
	flags.Int("video-framerate", defaults.Video.Framerate, "encoded video frames per second")
	flags.Int("video-bitrate-kbps", defaults.Video.BitrateKbps, "encoded video bitrate in Kbit/s")
	flags.Int("video-encoder-threads", defaults.Video.Tuning.Threads, "VP8 encoder thread count")
	flags.Int("video-keyframe-interval", defaults.Video.Tuning.KeyframeInterval, "maximum frames between keyframes")
	flags.Int("video-vp8-cpu-used", defaults.Video.Tuning.VP8CPUUsed, "VP8 speed setting from 0 to 16")
	flags.Bool("audio-enabled", defaults.Audio.Enabled, "capture desktop audio from a PulseAudio monitor")
	flags.String("audio-device", defaults.Audio.Device, "PulseAudio monitor source")
	flags.Int("audio-bitrate-kbps", defaults.Audio.BitrateKbps, "Opus audio bitrate in Kbit/s")
	flags.Bool("input-enabled", defaults.Input.Enabled, "request remote pointer and keyboard input")
	flags.Bool("input-pointer", defaults.Input.Pointer, "enable remote pointer input")
	flags.Bool("input-keyboard", defaults.Input.Keyboard, "enable remote keyboard input")
	flags.Int("input-queue-size", defaults.Input.QueueSize, "bounded remote input event queue size")
	flags.Bool("clipboard-enabled", defaults.Clipboard.Enabled, "synchronize the desktop clipboard")
	flags.String("webrtc-signaling-path", defaults.WebRTC.SignalingPath, "WebSocket signaling path")
	flags.Int("webrtc-max-peers", defaults.WebRTC.MaxPeers, "maximum concurrent WebRTC peers")
	flags.StringSlice("webrtc-ice-server", defaults.WebRTC.ICEServers, "ICE server URL; may be repeated")
	flags.String("webrtc-ice-username", defaults.WebRTC.ICEUsername, "ICE server username")
	flags.String("webrtc-ice-credential", defaults.WebRTC.ICECredential, "ICE server credential")
	flags.Int("webrtc-udp-port-min", defaults.WebRTC.UDPPortMin, "minimum ICE UDP port, or zero for the system range")
	flags.Int("webrtc-udp-port-max", defaults.WebRTC.UDPPortMax, "maximum ICE UDP port, or zero for the system range")
	flags.StringSlice("webrtc-allowed-origin", defaults.WebRTC.AllowedOrigins, "allowed WebSocket origin; may be repeated")

	return cmd
}

func loadConfig(cmd *cobra.Command, configFile string) (config.Config, error) {
	defaults := config.Defaults()
	v := viper.New()
	v.SetEnvPrefix("WEBDESKTOP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AllowEmptyEnv(true)
	v.AutomaticEnv()

	v.SetDefault("server.listen_address", defaults.Server.ListenAddress)
	v.SetDefault("server.shutdown_timeout", defaults.Server.ShutdownTimeout)
	v.SetDefault("logging.level", defaults.Logging.Level)
	v.SetDefault("logging.format", defaults.Logging.Format)
	v.SetDefault("tracing.enabled", defaults.Tracing.Enabled)
	v.SetDefault("video.source", defaults.Video.Source)
	v.SetDefault("video.cursor_mode", defaults.Video.CursorMode)
	v.SetDefault("video.codec", defaults.Video.Codec)
	v.SetDefault("video.width", defaults.Video.Width)
	v.SetDefault("video.height", defaults.Video.Height)
	v.SetDefault("video.framerate", defaults.Video.Framerate)
	v.SetDefault("video.bitrate_kbps", defaults.Video.BitrateKbps)
	v.SetDefault("video.tuning.threads", defaults.Video.Tuning.Threads)
	v.SetDefault("video.tuning.keyframe_interval", defaults.Video.Tuning.KeyframeInterval)
	v.SetDefault("video.tuning.vp8_cpu_used", defaults.Video.Tuning.VP8CPUUsed)
	v.SetDefault("audio.enabled", defaults.Audio.Enabled)
	v.SetDefault("audio.device", defaults.Audio.Device)
	v.SetDefault("audio.bitrate_kbps", defaults.Audio.BitrateKbps)
	v.SetDefault("input.enabled", defaults.Input.Enabled)
	v.SetDefault("input.pointer", defaults.Input.Pointer)
	v.SetDefault("input.keyboard", defaults.Input.Keyboard)
	v.SetDefault("input.queue_size", defaults.Input.QueueSize)
	v.SetDefault("clipboard.enabled", defaults.Clipboard.Enabled)
	v.SetDefault("webrtc.signaling_path", defaults.WebRTC.SignalingPath)
	v.SetDefault("webrtc.max_peers", defaults.WebRTC.MaxPeers)
	v.SetDefault("webrtc.ice_servers", defaults.WebRTC.ICEServers)
	v.SetDefault("webrtc.ice_username", defaults.WebRTC.ICEUsername)
	v.SetDefault("webrtc.ice_credential", defaults.WebRTC.ICECredential)
	v.SetDefault("webrtc.udp_port_min", defaults.WebRTC.UDPPortMin)
	v.SetDefault("webrtc.udp_port_max", defaults.WebRTC.UDPPortMax)
	v.SetDefault("webrtc.allowed_origins", defaults.WebRTC.AllowedOrigins)

	for _, key := range []string{
		"server.listen_address",
		"server.shutdown_timeout",
		"logging.level",
		"logging.format",
		"tracing.enabled",
		"video.source",
		"video.cursor_mode",
		"video.codec",
		"video.width",
		"video.height",
		"video.framerate",
		"video.bitrate_kbps",
		"video.tuning.threads",
		"video.tuning.keyframe_interval",
		"video.tuning.vp8_cpu_used",
		"audio.enabled",
		"audio.device",
		"audio.bitrate_kbps",
		"input.enabled",
		"input.pointer",
		"input.keyboard",
		"input.queue_size",
		"clipboard.enabled",
		"webrtc.signaling_path",
		"webrtc.max_peers",
		"webrtc.ice_servers",
		"webrtc.ice_username",
		"webrtc.ice_credential",
		"webrtc.udp_port_min",
		"webrtc.udp_port_max",
		"webrtc.allowed_origins",
	} {
		if err := v.BindEnv(key); err != nil {
			return config.Config{}, fmt.Errorf("bind environment %s: %w", key, err)
		}
	}

	flagBindings := map[string]string{
		"listen-address":          "server.listen_address",
		"shutdown-timeout":        "server.shutdown_timeout",
		"log-level":               "logging.level",
		"log-format":              "logging.format",
		"tracing-enabled":         "tracing.enabled",
		"video-source":            "video.source",
		"video-cursor-mode":       "video.cursor_mode",
		"video-codec":             "video.codec",
		"video-width":             "video.width",
		"video-height":            "video.height",
		"video-framerate":         "video.framerate",
		"video-bitrate-kbps":      "video.bitrate_kbps",
		"video-encoder-threads":   "video.tuning.threads",
		"video-keyframe-interval": "video.tuning.keyframe_interval",
		"video-vp8-cpu-used":      "video.tuning.vp8_cpu_used",
		"audio-enabled":           "audio.enabled",
		"audio-device":            "audio.device",
		"audio-bitrate-kbps":      "audio.bitrate_kbps",
		"input-enabled":           "input.enabled",
		"input-pointer":           "input.pointer",
		"input-keyboard":          "input.keyboard",
		"input-queue-size":        "input.queue_size",
		"clipboard-enabled":       "clipboard.enabled",
		"webrtc-signaling-path":   "webrtc.signaling_path",
		"webrtc-max-peers":        "webrtc.max_peers",
		"webrtc-ice-server":       "webrtc.ice_servers",
		"webrtc-ice-username":     "webrtc.ice_username",
		"webrtc-ice-credential":   "webrtc.ice_credential",
		"webrtc-udp-port-min":     "webrtc.udp_port_min",
		"webrtc-udp-port-max":     "webrtc.udp_port_max",
		"webrtc-allowed-origin":   "webrtc.allowed_origins",
	}
	for flagName, key := range flagBindings {
		if err := v.BindPFlag(key, cmd.Flags().Lookup(flagName)); err != nil {
			return config.Config{}, fmt.Errorf("bind flag %s: %w", flagName, err)
		}
	}

	if configFile != "" {
		v.SetConfigFile(configFile)
		if err := v.ReadInConfig(); err != nil {
			return config.Config{}, fmt.Errorf("read config file: %w", err)
		}
	}

	var cfg config.Config
	if err := v.UnmarshalExact(&cfg); err != nil {
		return config.Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}
