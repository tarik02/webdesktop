package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/tarik02/webdesktop/capture"
	"github.com/tarik02/webdesktop/config"
	"github.com/tarik02/webdesktop/httpserver"
	"github.com/tarik02/webdesktop/logging"
	"github.com/tarik02/webdesktop/media"
	rtc "github.com/tarik02/webdesktop/webrtc"
	"go.uber.org/zap"
)

// App wires the service dependencies.
type App struct {
	logger *zap.Logger
	server *httpserver.Server
	media  *media.Service
	webrtc *rtc.Service
}

// New constructs the application wiring.
func New(cfg config.Config) (*App, error) {
	logger, err := logging.New(cfg.Logging)
	if err != nil {
		return nil, err
	}

	gin.SetMode(gin.ReleaseMode)

	mediaService, err := media.New(media.Config{
		Capture: capture.Config{
			Source:     cfg.Video.Source,
			CursorMode: cfg.Video.CursorMode,
		},
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
			H264SpeedPreset:  cfg.Video.Tuning.H264SpeedPreset,
		},
	}, logger.Named("media"))
	if err != nil {
		_ = logger.Sync()
		return nil, err
	}

	webrtcService, err := rtc.New(rtc.Config{
		Codec:          cfg.Video.Codec,
		ICEServers:     cfg.WebRTC.ICEServers,
		ICEUsername:    cfg.WebRTC.ICEUsername,
		ICECredential:  cfg.WebRTC.ICECredential,
		UDPPortMin:     uint16(cfg.WebRTC.UDPPortMin),
		UDPPortMax:     uint16(cfg.WebRTC.UDPPortMax),
		MaxPeers:       cfg.WebRTC.MaxPeers,
		AllowedOrigins: cfg.WebRTC.AllowedOrigins,
	}, mediaService, logger.Named("webrtc"))
	if err != nil {
		_ = logger.Sync()
		return nil, err
	}

	server, err := httpserver.New(cfg.Server, logger, func(router *gin.Engine) {
		router.GET(cfg.WebRTC.SignalingPath, webrtcService.Handler())
	})
	if err != nil {
		webrtcService.Close()
		_ = logger.Sync()
		return nil, err
	}

	return &App{
		logger: logger,
		server: server,
		media:  mediaService,
		webrtc: webrtcService,
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
	results := make(chan result, 3)
	go func() {
		results <- result{component: "http server", err: a.server.Serve(runCtx)}
	}()
	go func() {
		results <- result{component: "media", err: a.media.Run(runCtx)}
	}()
	go func() {
		results <- result{component: "WebRTC", err: a.webrtc.Run(runCtx)}
	}()

	var runErr error
	for range 3 {
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
	_ = a.logger.Sync()
}
