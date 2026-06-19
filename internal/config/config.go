// Package config loads and validates Hina's typed configuration from a TOML file
// plus HINA_* environment overrides. Precedence is env > file > defaults, matching
// V1. The config file is optional; defaults yield a working localhost server.
package config

import (
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/BurntSushi/toml"
)

// Config is the full server configuration.
type Config struct {
	Server ServerConfig `toml:"server"`
	Agent  AgentConfig  `toml:"agent"`
	Log    LogConfig    `toml:"log"`
}

// ServerConfig controls binding and TLS.
type ServerConfig struct {
	Host       string `toml:"host"`        // default 127.0.0.1
	Port       int    `toml:"port"`        // default 8733
	LANEnabled bool   `toml:"lan_enabled"` // required to bind a non-loopback host
	TLSCert    string `toml:"tls_cert"`
	TLSKey     string `toml:"tls_key"`
}

// AgentConfig holds the spoken-agent identity (used later by ASR name biasing).
type AgentConfig struct {
	Name        string   `toml:"name"`         // default Hina
	NameAliases []string `toml:"name_aliases"` // mis-hearing/spelling variants
}

// LogConfig controls structured logging.
type LogConfig struct {
	Level  string `toml:"level"`  // debug|info|warn|error (default info)
	Format string `toml:"format"` // text|json (default text)
}

// Default returns the built-in defaults (localhost-only, no TLS).
func Default() Config {
	return Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 8733},
		Agent:  AgentConfig{Name: "Hina"},
		Log:    LogConfig{Level: "info", Format: "text"},
	}
}

// Load reads defaults, overlays the TOML file at path (if it exists), applies
// HINA_* env overrides, and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if _, err := toml.DecodeFile(path, &cfg); err != nil {
				return Config{}, fmt.Errorf("parse config %s: %w", path, err)
			}
		} else if !os.IsNotExist(err) {
			return Config{}, fmt.Errorf("stat config %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("HINA_SERVER_HOST"); v != "" {
		c.Server.Host = v
	}
	if v := os.Getenv("HINA_SERVER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.Server.Port = p
		}
	}
	if v := os.Getenv("HINA_SERVER_LAN"); v != "" {
		c.Server.LANEnabled = v == "1" || v == "true"
	}
	if v := os.Getenv("HINA_AGENT_NAME"); v != "" {
		c.Agent.Name = v
	}
	if v := os.Getenv("HINA_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := os.Getenv("HINA_LOG_FORMAT"); v != "" {
		c.Log.Format = v
	}
}

// Validate checks values and the LAN/loopback invariant.
func (c Config) Validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d out of range", c.Server.Port)
	}
	if c.Server.Host == "" {
		return fmt.Errorf("server.host is empty")
	}
	if !c.Server.IsLoopbackBind() && !c.Server.LANEnabled {
		return fmt.Errorf("server.host %q is non-loopback; set server.lan_enabled=true (or HINA_SERVER_LAN=1) to allow LAN binding", c.Server.Host)
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level %q must be debug|info|warn|error", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format %q must be text|json", c.Log.Format)
	}
	if c.Agent.Name == "" {
		return fmt.Errorf("agent.name is empty")
	}
	return nil
}

// Addr is the host:port bind address.
func (s ServerConfig) Addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// IsLoopbackBind reports whether the configured host is loopback-only.
func (s ServerConfig) IsLoopbackBind() bool {
	if s.Host == "localhost" {
		return true
	}
	ip := net.ParseIP(s.Host)
	return ip != nil && ip.IsLoopback()
}

// TLSEnabled reports whether both a cert and key are configured.
func (s ServerConfig) TLSEnabled() bool { return s.TLSCert != "" && s.TLSKey != "" }
