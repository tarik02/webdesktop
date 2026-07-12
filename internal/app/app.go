package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/tarik02/webdesktop/capture"
	"github.com/tarik02/webdesktop/config"
	"github.com/tarik02/webdesktop/desktop"
	"github.com/tarik02/webdesktop/httpserver"
	remoteinput "github.com/tarik02/webdesktop/input"
	"github.com/tarik02/webdesktop/logging"
	"github.com/tarik02/webdesktop/media"
	webui "github.com/tarik02/webdesktop/web"
	rtc "github.com/tarik02/webdesktop/webrtc"
	"go.uber.org/zap"
)

// App wires the service dependencies.
type App struct {
	logger  *zap.Logger
	server  *httpserver.Server
	desktop *desktop.Service
	audio   *media.AudioService
	input   *remoteinput.Controller
	webrtc  *rtc.Service
}

// New constructs the application wiring.
func New(cfg config.Config) (*App, error) {
	logger, err := logging.New(cfg.Logging)
	if err != nil {
		return nil, err
	}

	gin.SetMode(gin.ReleaseMode)

	portalConfig := capture.Config{
		Source:     cfg.Video.Source,
		CursorMode: cfg.Video.CursorMode,
		Input: capture.InputConfig{
			Enabled:  cfg.Input.Enabled,
			Pointer:  cfg.Input.Pointer,
			Keyboard: cfg.Input.Keyboard,
		},
	}
	inputController, err := remoteinput.New(remoteinput.Config{
		Enabled:   cfg.Input.Enabled,
		Pointer:   cfg.Input.Pointer,
		Keyboard:  cfg.Input.Keyboard,
		QueueSize: cfg.Input.QueueSize,
	})
	if err != nil {
		_ = logger.Sync()
		return nil, err
	}

	mediaService, err := media.New(media.Config{
		Capture: portalConfig,
		Quality: media.Quality{
			Codec:       cfg.Video.Codec,
			Width:       cfg.Video.Width,
			Height:      cfg.Video.Height,
			Framerate:   cfg.Video.Framerate,
			BitrateKbps: cfg.Video.BitrateKbps,
		},
		Tuning: media.Tuning{
			Threads:          cfg.Video.Tuning.Threads,
			KeyframeInterval: cfg.Video.Tuning.KeyframeInterval,
			VP8CPUUsed:       cfg.Video.Tuning.VP8CPUUsed,
		},
	}, logger.Named("media"))
	if err != nil {
		_ = inputController.Close()
		_ = logger.Sync()
		return nil, err
	}
	audioService, err := media.NewAudio(media.AudioConfig{
		Enabled:     cfg.Audio.Enabled,
		Device:      cfg.Audio.Device,
		BitrateKbps: cfg.Audio.BitrateKbps,
	}, logger.Named("audio"))
	if err != nil {
		_ = inputController.Close()
		_ = logger.Sync()
		return nil, err
	}

	desktopService, err := desktop.New(
		portalConfig,
		mediaService,
		inputController,
		logger.Named("desktop"),
	)
	if err != nil {
		_ = inputController.Close()
		_ = logger.Sync()
		return nil, err
	}

	webrtcService, err := rtc.New(rtc.Config{
		Codec:          cfg.Video.Codec,
		AudioEnabled:   cfg.Audio.Enabled,
		ICEServers:     cfg.WebRTC.ICEServers,
		ICEUsername:    cfg.WebRTC.ICEUsername,
		ICECredential:  cfg.WebRTC.ICECredential,
		UDPPortMin:     uint16(cfg.WebRTC.UDPPortMin),
		UDPPortMax:     uint16(cfg.WebRTC.UDPPortMax),
		MaxPeers:       cfg.WebRTC.MaxPeers,
		AllowedOrigins: cfg.WebRTC.AllowedOrigins,
		TracingEnabled: cfg.Tracing.Enabled,
	}, mediaService, audioService, inputController, logger.Named("webrtc"))
	if err != nil {
		_ = inputController.Close()
		_ = logger.Sync()
		return nil, err
	}

	server, err := httpserver.New(cfg.Server, logger, func(router *gin.Engine) {
		router.GET("/api/config", func(c *gin.Context) {
			c.JSON(200, struct {
				Version       int           `json:"version"`
				SignalingPath string        `json:"signaling_path"`
				Video         media.Quality `json:"video"`
				Audio         struct {
					Enabled bool `json:"enabled"`
				} `json:"audio"`
				Tracing struct {
					Enabled bool `json:"enabled"`
				} `json:"tracing"`
				Input struct {
					Enabled  bool `json:"enabled"`
					Pointer  bool `json:"pointer"`
					Keyboard bool `json:"keyboard"`
				} `json:"input"`
			}{
				Version:       1,
				SignalingPath: cfg.WebRTC.SignalingPath,
				Video:         mediaService.Quality(),
				Audio: struct {
					Enabled bool `json:"enabled"`
				}{Enabled: cfg.Audio.Enabled},
				Tracing: struct {
					Enabled bool `json:"enabled"`
				}{Enabled: cfg.Tracing.Enabled},
				Input: struct {
					Enabled  bool `json:"enabled"`
					Pointer  bool `json:"pointer"`
					Keyboard bool `json:"keyboard"`
				}{
					Enabled:  cfg.Input.Enabled,
					Pointer:  cfg.Input.Pointer,
					Keyboard: cfg.Input.Keyboard,
				},
			})
		})
		router.GET("/api/status", func(c *gin.Context) {
			ready := false
			select {
			case <-desktopService.Ready():
				ready = true
			default:
			}
			c.JSON(200, struct {
				Status      string        `json:"status"`
				Ready       bool          `json:"ready"`
				ActivePeers int           `json:"active_peers"`
				Video       media.Quality `json:"video"`
			}{
				Status:      "ok",
				Ready:       ready,
				ActivePeers: webrtcService.PeerCount(),
				Video:       mediaService.Quality(),
			})
		})
		router.GET(cfg.WebRTC.SignalingPath, webrtcService.Handler())
		webui.Mount(router)
	})
	if err != nil {
		webrtcService.Close()
		_ = inputController.Close()
		_ = logger.Sync()
		return nil, err
	}

	return &App{
		logger:  logger,
		server:  server,
		desktop: desktopService,
		audio:   audioService,
		input:   inputController,
		webrtc:  webrtcService,
	}, nil
}

// Serve runs the HTTP service until the context is canceled.
func (a *App) Serve(ctx context.Context) error {
	a.logger.Info("webdesktop starting")

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type result struct {
		component string
		err       error
	}
	componentCount := 3
	if a.audio.Enabled() {
		componentCount++
	}
	results := make(chan result, componentCount)
	go func() {
		results <- result{component: "http server", err: a.server.Serve(runCtx)}
	}()
	go func() {
		results <- result{component: "desktop", err: a.desktop.Run(runCtx)}
	}()
	go func() {
		results <- result{component: "WebRTC", err: a.webrtc.Run(runCtx)}
	}()
	if a.audio.Enabled() {
		go func() {
			results <- result{component: "audio", err: a.audio.Run(runCtx, a.desktop.Ready())}
		}()
	}

	var runErr error
	for range componentCount {
		result := <-results
		if result.err != nil && runCtx.Err() != nil && errors.Is(result.err, context.Canceled) {
			continue
		}
		if result.err == nil && runCtx.Err() == nil {
			result.err = errors.New("stopped unexpectedly")
		}
		if result.err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("%s: %w", result.component, result.err))
			cancel()
		}
	}

	if runErr != nil {
		return fmt.Errorf("run service: %w", runErr)
	}
	a.logger.Info("webdesktop stopped")
	return nil
}

// Close flushes buffered log entries.
func (a *App) Close() {
	a.webrtc.Close()
	_ = a.input.Close()
	_ = a.logger.Sync()
}
