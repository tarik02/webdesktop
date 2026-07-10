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
)

// Config contains the resolved service configuration.
type Config struct {
	Server  Server  `mapstructure:"server" yaml:"server"`
	Logging Logging `mapstructure:"logging" yaml:"logging"`
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
