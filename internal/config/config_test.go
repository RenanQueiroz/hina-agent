package config

import "testing"

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
