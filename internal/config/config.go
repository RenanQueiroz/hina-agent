// Package config loads and validates Hina's typed configuration from a TOML file
// plus HINA_* environment overrides. Precedence is env > file > defaults, matching
// V1. The config file is optional; defaults yield a working localhost server.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the full server configuration.
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Agent    AgentConfig    `toml:"agent"`
	LLM      LLMConfig      `toml:"llm"`
	Realtime RealtimeConfig `toml:"realtime"`
	TTS      TTSConfig      `toml:"tts"`
	Paths    PathsConfig    `toml:"paths"`
	Log      LogConfig      `toml:"log"`
}

// TTSConfig configures the local text-to-speech engine (Phase 4, Supertonic via
// ONNX Runtime). It is off by default: local TTS needs the onnx-tagged build and
// the downloaded model assets, so the default control-plane build leaves it
// disabled and `hina doctor` reports it unavailable.
type TTSConfig struct {
	Enabled   bool    `toml:"enabled"`    // turn on local TTS (needs the onnx build + assets)
	Voice     string  `toml:"voice"`      // default preset voice id, e.g. "M1"
	Lang      string  `toml:"lang"`       // default language tag, e.g. "en"
	Speed     float64 `toml:"speed"`      // default tempo multiplier (0 -> engine default)
	Steps     int     `toml:"steps"`      // flow-matching denoise steps (0 -> engine default 8)
	IdleTTL   string  `toml:"idle_ttl"`   // unload models after this idle duration, e.g. "5m"
	AssetsDir string  `toml:"assets_dir"` // override the model/runtime asset root (default: cache dir)
	Threads   int     `toml:"threads"`    // ORT intra-op CPU threads (0 -> ORT default)
}

// AssetsRoot resolves the local-inference asset root: the configured override, or
// a models/ subdir of the given cache dir (model downloads belong in the cache).
func (t TTSConfig) AssetsRoot(cacheDir string) string {
	if t.AssetsDir != "" {
		return t.AssetsDir
	}
	return filepath.Join(cacheDir, "models")
}

// IdleTTLOr parses IdleTTL, falling back to def when empty or invalid.
func (t TTSConfig) IdleTTLOr(def time.Duration) time.Duration {
	if t.IdleTTL == "" {
		return def
	}
	d, err := time.ParseDuration(t.IdleTTL)
	if err != nil || d < 0 {
		return def
	}
	return d
}

// RealtimeConfig configures the WebRTC voice bridge (Phase 3). ICEServers is
// optional: localhost and most LANs connect on host candidates alone, so the
// default (none) needs no external STUN/TURN.
type RealtimeConfig struct {
	ICEServers []ICEServer `toml:"ice_servers"`
}

// ICEServer is one STUN/TURN server. URLs use the standard ICE scheme
// (stun:/stuns:/turn:/turns:). TURN relays require Username+Credential; STUN
// does not. Configured as a TOML array of tables:
//
//	[[realtime.ice_servers]]
//	urls = ["stun:stun.l.google.com:19302"]
//
//	[[realtime.ice_servers]]
//	urls = ["turn:turn.example.com:3478"]
//	username = "user"
//	credential = "secret"
type ICEServer struct {
	URLs       []string `toml:"urls"`
	Username   string   `toml:"username"`
	Credential string   `toml:"credential"`
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
	if v := os.Getenv("HINA_TTS_ENABLED"); v != "" {
		c.TTS.Enabled = v == "1" || v == "true"
	}
	if v := os.Getenv("HINA_TTS_VOICE"); v != "" {
		c.TTS.Voice = v
	}
	if v := os.Getenv("HINA_TTS_ASSETS_DIR"); v != "" {
		c.TTS.AssetsDir = v
	}
	if v := os.Getenv("HINA_REALTIME_ICE_SERVERS"); v != "" {
		// Env is the simple STUN path: a comma-separated URL list, each its own
		// server with no credentials. TURN (which needs credentials) is config-file
		// only — Validate rejects a credential-less turn: URL.
		var servers []ICEServer
		for _, u := range splitAndTrim(v) {
			servers = append(servers, ICEServer{URLs: []string{u}})
		}
		c.Realtime.ICEServers = servers
	}
	// Expand ${VAR} references (e.g. api_key = "${OPENAI_API_KEY}").
	c.LLM.APIKey = expandEnv(c.LLM.APIKey)
}

// splitAndTrim splits a comma-separated env value into non-empty trimmed items.
func splitAndTrim(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
	// openai-compat is the LOCAL OpenAI-compatible path; require an explicit
	// base_url so a misconfigured local backend fails closed rather than silently
	// sending conversation history to cloud OpenAI.
	if c.LLM.Provider == "openai-compat" && c.LLM.BaseURL == "" {
		return fmt.Errorf("llm.base_url is required when llm.provider=openai-compat (e.g. http://localhost:8080/v1); refusing to default to cloud OpenAI")
	}
	// Bounded ranges (0 = use the engine default) defend against a config typo
	// driving an unbounded flow loop or a near-zero speed inflating the latent
	// allocation. The engine clamps too, but failing closed at load is clearer.
	if c.TTS.Steps != 0 && (c.TTS.Steps < 1 || c.TTS.Steps > 100) {
		return fmt.Errorf("tts.steps %d out of range (1..100, or 0 for the default)", c.TTS.Steps)
	}
	if c.TTS.Speed != 0 && (c.TTS.Speed < 0.25 || c.TTS.Speed > 4.0) {
		return fmt.Errorf("tts.speed %v out of range (0.25..4.0, or 0 for the default)", c.TTS.Speed)
	}
	if c.TTS.Threads < 0 {
		return fmt.Errorf("tts.threads %d must be >= 0", c.TTS.Threads)
	}
	if c.TTS.IdleTTL != "" {
		if d, err := time.ParseDuration(c.TTS.IdleTTL); err != nil || d < 0 {
			return fmt.Errorf("tts.idle_ttl %q must be a non-negative duration (e.g. \"5m\")", c.TTS.IdleTTL)
		}
	}
	for _, srv := range c.Realtime.ICEServers {
		if len(srv.URLs) == 0 {
			return fmt.Errorf("realtime.ice_servers entry has no urls")
		}
		needsCred := false
		for _, u := range srv.URLs {
			if !isICEURL(u) {
				return fmt.Errorf("realtime.ice_servers url %q must be a stun:/stuns:/turn:/turns: URL", u)
			}
			if strings.HasPrefix(u, "turn:") || strings.HasPrefix(u, "turns:") {
				needsCred = true
			}
		}
		// A credential-less TURN server is rejected by Pion at NewPeerConnection,
		// which would break every call. Fail closed here with a clear message
		// instead of surfacing as a runtime 502.
		if needsCred && (srv.Username == "" || srv.Credential == "") {
			return fmt.Errorf("realtime.ice_servers entry with a turn:/turns: url requires username and credential")
		}
	}
	return nil
}

// isICEURL reports whether u is a syntactically valid ICE server URL (scheme +
// non-empty remainder; Pion does the deeper host parse at connect time).
func isICEURL(u string) bool {
	for _, scheme := range []string{"stun:", "stuns:", "turn:", "turns:"} {
		if strings.HasPrefix(u, scheme) && len(u) > len(scheme) {
			return true
		}
	}
	return false
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
