package app

import (
	"context"
	"fmt"

	"github.com/gin-gonic/gin"
	"github.com/tarik02/webdesktop/config"
	"github.com/tarik02/webdesktop/httpserver"
	"github.com/tarik02/webdesktop/logging"
	"go.uber.org/zap"
)

// App wires the service dependencies.
type App struct {
	logger *zap.Logger
	server *httpserver.Server
}

// New constructs the application wiring.
func New(cfg config.Config) (*App, error) {
	logger, err := logging.New(cfg.Logging)
	if err != nil {
		return nil, err
	}

	gin.SetMode(gin.ReleaseMode)

	server, err := httpserver.New(cfg.Server, logger)
	if err != nil {
		return nil, err
	}

	return &App{
		logger: logger,
		server: server,
	}, nil
}

// Serve runs the HTTP service until the context is canceled.
func (a *App) Serve(ctx context.Context) error {
	a.logger.Info("webdesktop starting")
	if err := a.server.Serve(ctx); err != nil {
		return fmt.Errorf("run service: %w", err)
	}
	a.logger.Info("webdesktop stopped")
	return nil
}

// Close flushes buffered log entries.
func (a *App) Close() {
	_ = a.logger.Sync()
}
