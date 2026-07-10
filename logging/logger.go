package logging

import (
	"fmt"

	"github.com/tarik02/webdesktop/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New constructs a structured logger from configuration.
func New(cfg config.Logging) (*zap.Logger, error) {
	zapConfig := zap.NewProductionConfig()
	if err := zapConfig.Level.UnmarshalText([]byte(cfg.Level)); err != nil {
		return nil, fmt.Errorf("parse log level: %w", err)
	}

	zapConfig.Encoding = cfg.Format
	zapConfig.OutputPaths = []string{"stderr"}
	zapConfig.ErrorOutputPaths = []string{"stderr"}
	zapConfig.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder

	logger, err := zapConfig.Build()
	if err != nil {
		return nil, fmt.Errorf("build logger: %w", err)
	}
	return logger, nil
}
