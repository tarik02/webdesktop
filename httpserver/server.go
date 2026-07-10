package httpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tarik02/webdesktop/config"
	"go.uber.org/zap"
)

const readHeaderTimeout = 5 * time.Second

// Server owns the HTTP transport and graceful shutdown lifecycle.
type Server struct {
	httpServer      *http.Server
	logger          *zap.Logger
	shutdownTimeout time.Duration
}

// New constructs an HTTP server without starting it.
func New(cfg config.Server, logger *zap.Logger) (*Server, error) {
	shutdownTimeout, err := cfg.ShutdownDuration()
	if err != nil {
		return nil, err
	}

	router := gin.New()
	router.Use(requestLogger(logger))
	router.Use(gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, recovered any) {
		logger.Error("http request panic",
			zap.Any("panic", recovered),
			zap.Stack("stack"),
		)
		c.AbortWithStatusJSON(http.StatusInternalServerError, struct {
			Status string `json:"status"`
		}{Status: "error"})
	}))
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, struct {
			Status string `json:"status"`
		}{Status: "ok"})
	})

	return &Server{
		httpServer: &http.Server{
			Addr:              cfg.ListenAddress,
			Handler:           router,
			ReadHeaderTimeout: readHeaderTimeout,
			ErrorLog:          zap.NewStdLog(logger.Named("http")),
		},
		logger:          logger,
		shutdownTimeout: shutdownTimeout,
	}, nil
}

// Serve runs until the context is canceled or the listener fails.
func (s *Server) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- s.httpServer.ListenAndServe()
	}()

	s.logger.Info("http server listening", zap.String("address", s.httpServer.Addr))

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve http: %w", err)
	case <-ctx.Done():
		s.logger.Info("http server shutting down")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()

		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			_ = s.httpServer.Close()
			return fmt.Errorf("shutdown http: %w", err)
		}

		err := <-errCh
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve http: %w", err)
		}
		return nil
	}
}

func requestLogger(logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		startedAt := time.Now()
		c.Next()

		logger.Info("http request",
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.Int("response_bytes", c.Writer.Size()),
			zap.Duration("duration", time.Since(startedAt)),
			zap.String("client_ip", c.ClientIP()),
		)
	}
}
