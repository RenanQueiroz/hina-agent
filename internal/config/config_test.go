package config

import (
	"path/filepath"
	"testing"
	"time"
)

// TestValidateTTS locks the [tts] validation: non-negative steps/speed/threads and
// a parseable idle_ttl; the helpers resolve defaults and the asset root.
func TestValidateTTS(t *testing.T) {
	c := Default()
	c.TTS.Steps = -1
	if err := c.Validate(); err == nil {
		t.Fatal("negative tts.steps must fail")
	}
	c = Default()
	c.TTS.Speed = -0.5
	if err := c.Validate(); err == nil {
		t.Fatal("negative tts.speed must fail")
	}
	c = Default()
	c.TTS.Steps = 1000
	if err := c.Validate(); err == nil {
		t.Fatal("out-of-range tts.steps must fail")
	}
	c = Default()
	c.TTS.Speed = 10
	if err := c.Validate(); err == nil {
		t.Fatal("out-of-range tts.speed must fail")
	}
	c = Default()
	c.TTS.IdleTTL = "not-a-duration"
	if err := c.Validate(); err == nil {
		t.Fatal("invalid tts.idle_ttl must fail")
	}
	c = Default()
	c.TTS.Steps, c.TTS.Speed, c.TTS.IdleTTL = 8, 1.05, "5m"
	if err := c.Validate(); err != nil {
		t.Fatalf("valid tts config should pass: %v", err)
	}

	if got := c.TTS.IdleTTLOr(time.Minute); got != 5*time.Minute {
		t.Fatalf("IdleTTLOr = %v, want 5m", got)
	}
	if got := (TTSConfig{}).IdleTTLOr(time.Minute); got != time.Minute {
		t.Fatalf("empty IdleTTLOr = %v, want default 1m", got)
	}
	// The single shared asset root: default models/ dir, or whichever engine's
	// override is set (TTS preferred, then ASR).
	if got := (Config{}).AssetsRoot("/cache"); got != filepath.Join("/cache", "models") {
		t.Fatalf("AssetsRoot default = %q", got)
	}
	if got := (Config{TTS: TTSConfig{AssetsDir: "/custom"}}).AssetsRoot("/cache"); got != "/custom" {
		t.Fatalf("AssetsRoot tts override = %q, want /custom", got)
	}
	if got := (Config{ASR: ASRConfig{AssetsDir: "/asr"}}).AssetsRoot("/cache"); got != "/asr" {
		t.Fatalf("AssetsRoot asr override = %q, want /asr", got)
	}
}

func TestValidateRejectsDivergentAssetsDir(t *testing.T) {
	c := Default()
	c.TTS.AssetsDir = "/a"
	c.ASR.AssetsDir = "/b"
	if err := c.Validate(); err == nil {
		t.Fatal("divergent tts/asr assets_dir must be rejected")
	}
	// Equal dirs are fine.
	c.ASR.AssetsDir = "/a"
	if err := c.Validate(); err != nil {
		t.Fatalf("matching assets_dir should validate: %v", err)
	}
}

// TestValidateCompatBaseURL locks the fail-closed rule: the local openai-compat
// provider requires an explicit base_url (never silently routes to cloud).
func TestValidateCompatBaseURL(t *testing.T) {
	c := Default()
	c.LLM.Provider = "openai-compat"
	if err := c.Validate(); err == nil {
		t.Fatal("openai-compat without base_url must fail validation")
	}
	c.LLM.BaseURL = "http://localhost:8080/v1"
	if err := c.Validate(); err != nil {
		t.Fatalf("openai-compat with base_url should validate: %v", err)
	}
}

// TestValidateICEServers locks the realtime ICE checks: only stun/turn schemes
// are accepted, and a TURN relay (which Pion rejects without credentials) must
// carry username+credential or fail closed at config load.
func TestValidateICEServers(t *testing.T) {
	c := Default()
	c.Realtime.ICEServers = []ICEServer{
		{URLs: []string{"stun:stun.l.google.com:19302"}},
		{URLs: []string{"turns:turn.example.com:5349"}, Username: "u", Credential: "p"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid ICE servers should pass: %v", err)
	}

	c.Realtime.ICEServers = []ICEServer{{URLs: []string{"http://not-an-ice-server"}}}
	if err := c.Validate(); err == nil {
		t.Fatal("non-stun/turn ICE URL must fail validation")
	}

	c.Realtime.ICEServers = []ICEServer{{URLs: []string{"stun:"}}} // scheme only, no host
	if err := c.Validate(); err == nil {
		t.Fatal("ICE URL with no host must fail validation")
	}

	c.Realtime.ICEServers = []ICEServer{{URLs: []string{"turn:turn.example.com:3478"}}} // TURN, no creds
	if err := c.Validate(); err == nil {
		t.Fatal("turn: URL without credentials must fail validation")
	}

	c.Realtime.ICEServers = []ICEServer{{Username: "u", Credential: "p"}} // no urls
	if err := c.Validate(); err == nil {
		t.Fatal("ICE server with no urls must fail validation")
	}
}

// TestValidateLANTLS locks the LAN security invariants: a non-loopback bind
// needs both an explicit LAN opt-in AND TLS (cookies must not cross the LAN in
// cleartext), unless the operator explicitly opts into insecure dev LAN.
func TestValidateLANTLS(t *testing.T) {
	base := func() Config {
		c := Default()
		return c
	}
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"loopback default", func(*Config) {}, false},
		{
			"non-loopback without lan_enabled",
			func(c *Config) { c.Server.Host = "0.0.0.0" },
			true,
		},
		{
			"non-loopback lan without tls",
			func(c *Config) { c.Server.Host = "0.0.0.0"; c.Server.LANEnabled = true },
			true,
		},
		{
			"non-loopback lan with tls",
			func(c *Config) {
				c.Server.Host = "0.0.0.0"
				c.Server.LANEnabled = true
				c.Server.TLSCert = "cert.pem"
				c.Server.TLSKey = "key.pem"
			},
			false,
		},
		{
			"non-loopback lan insecure opt-in",
			func(c *Config) {
				c.Server.Host = "0.0.0.0"
				c.Server.LANEnabled = true
				c.Server.LANInsecure = true
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}
