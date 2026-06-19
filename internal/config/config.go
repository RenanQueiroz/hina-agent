// Package config loads and validates Hina's typed configuration from a TOML file
// plus HINA_* environment overrides. Precedence is env > file > defaults, matching
// V1. The config file is optional; defaults yield a working localhost server.
package config

import (
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"

	"github.com/BurntSushi/toml"
)

// Config is the full server configuration.
type Config struct {
	Server ServerConfig `toml:"server"`
	Agent  AgentConfig  `toml:"agent"`
	LLM    LLMConfig    `toml:"llm"`
	Paths  PathsConfig  `toml:"paths"`
	Log    LogConfig    `toml:"log"`
}

// PathsConfig optionally overrides the OS-resolved application directories.
// Empty fields keep the platform default. The config-file location itself is
// not overridable here (use env vars), since the config is read before it.
type PathsConfig struct {
	Data    string `toml:"data_dir"`    // SQLite DB, vault, workspaces
	Cache   string `toml:"cache_dir"`   // model/runtime downloads
	Log     string `toml:"log_dir"`     // process/setup logs
	Runtime string `toml:"runtime_dir"` // sockets, scratch, locks
}

// LLMConfig selects the active text LLM backend. Admin-owned; users do not pick
// STT/LLM/TTS. provider "mock" needs no credentials; "openai" targets any
// OpenAI-compatible endpoint (cloud OpenAI by default, or a local llama.cpp
// server via base_url).
type LLMConfig struct {
	Provider     string `toml:"provider"`      // mock | openai (default mock)
	Model        string `toml:"model"`         // e.g. gpt-5.4-mini, or a local model id
	BaseURL      string `toml:"base_url"`      // e.g. http://localhost:8080/v1 for llama.cpp
	APIKey       string `toml:"api_key"`       // literal or ${ENV_VAR}
	SystemPrompt string `toml:"system_prompt"` // system message prepended to context
}

// ServerConfig controls binding and TLS.
type ServerConfig struct {
	Host        string `toml:"host"`         // default 127.0.0.1
	Port        int    `toml:"port"`         // default 8733
	LANEnabled  bool   `toml:"lan_enabled"`  // required to bind a non-loopback host
	LANInsecure bool   `toml:"lan_insecure"` // opt out of the LAN-requires-TLS rule (dev only)
	TLSCert     string `toml:"tls_cert"`
	TLSKey      string `toml:"tls_key"`
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
		LLM:    LLMConfig{Provider: "mock", SystemPrompt: "You are Hina, a helpful, concise assistant."},
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
	if v := os.Getenv("HINA_SERVER_LAN_INSECURE"); v != "" {
		c.Server.LANInsecure = v == "1" || v == "true"
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
	if v := os.Getenv("HINA_LLM_PROVIDER"); v != "" {
		c.LLM.Provider = v
	}
	if v := os.Getenv("HINA_LLM_MODEL"); v != "" {
		c.LLM.Model = v
	}
	if v := os.Getenv("HINA_LLM_BASE_URL"); v != "" {
		c.LLM.BaseURL = v
	}
	if v := os.Getenv("HINA_LLM_API_KEY"); v != "" {
		c.LLM.APIKey = v
	}
	// Expand ${VAR} references (e.g. api_key = "${OPENAI_API_KEY}").
	c.LLM.APIKey = expandEnv(c.LLM.APIKey)
}

var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces ${VAR} references with the environment value (empty if unset).
func expandEnv(s string) string {
	return envRefRe.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(envRefRe.FindStringSubmatch(m)[1])
	})
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
	// A non-loopback bind serves session cookies; those must travel over TLS.
	// "LAN clients still authenticate — no trusted network" (Phase 1): cleartext
	// cookies on the LAN are exactly the leak the plan forbids. Refuse unless TLS
	// is configured, or the operator explicitly opts into insecure dev LAN.
	if !c.Server.IsLoopbackBind() && c.Server.LANEnabled && !c.Server.TLSEnabled() && !c.Server.LANInsecure {
		return fmt.Errorf("server.host %q is non-loopback without TLS: set server.tls_cert/tls_key, or server.lan_insecure=true (HINA_SERVER_LAN_INSECURE=1) to allow cleartext LAN (NOT recommended — session cookies would be sent unencrypted)", c.Server.Host)
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
	switch c.LLM.Provider {
	case "", "mock", "openai", "openai-compat":
	default:
		return fmt.Errorf("llm.provider %q must be mock|openai|openai-compat", c.LLM.Provider)
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
