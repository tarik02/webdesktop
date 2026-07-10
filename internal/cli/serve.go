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
		Short: "Run the webdesktop HTTP service",
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

	for _, key := range []string{
		"server.listen_address",
		"server.shutdown_timeout",
		"logging.level",
		"logging.format",
	} {
		if err := v.BindEnv(key); err != nil {
			return config.Config{}, fmt.Errorf("bind environment %s: %w", key, err)
		}
	}

	flagBindings := map[string]string{
		"listen-address":   "server.listen_address",
		"shutdown-timeout": "server.shutdown_timeout",
		"log-level":        "logging.level",
		"log-format":       "logging.format",
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
