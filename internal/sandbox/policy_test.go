package sandbox

import "testing"

func TestDefaultEnvironment(t *testing.T) {
	e := DefaultEnvironment()
	for _, tool := range BuiltinTools {
		if !e.ToolAllowed(tool) {
			t.Fatalf("default should allow built-in tool %q", tool)
		}
	}
	if e.NetworkAllowed("localhost", 8080) {
		t.Fatal("default network must be deny")
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("default environment should validate: %v", err)
	}
}

func TestToolAllowed(t *testing.T) {
	e := Environment{AllowedTools: []string{ToolShell}}
	if !e.ToolAllowed(ToolShell) {
		t.Fatal("shell should be allowed")
	}
	if e.ToolAllowed(ToolHTTP) {
		t.Fatal("http should not be allowed")
	}
}

func TestNetworkAllowed(t *testing.T) {
	e := Environment{Network: NetworkPolicy{Default: "deny", Allow: []NetworkRule{{Host: "localhost", Port: 8080}}}}
	if !e.NetworkAllowed("localhost", 8080) {
		t.Fatal("explicit allow entry should pass")
	}
	if e.NetworkAllowed("localhost", 9090) {
		t.Fatal("un-listed port should be denied")
	}
	open := Environment{Network: NetworkPolicy{Default: "allow"}}
	if !open.NetworkAllowed("anything", 1) {
		t.Fatal("allow default should pass everything")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		env  Environment
		ok   bool
	}{
		{"unknown tool", Environment{AllowedTools: []string{"rm-rf"}}, false},
		{"bad network default", Environment{Network: NetworkPolicy{Default: "maybe"}}, false},
		{"bad port", Environment{Network: NetworkPolicy{Allow: []NetworkRule{{Host: "h", Port: 0}}}}, false},
		{"mcp needs url", Environment{MCPServers: []MCPServer{{Name: "x"}}}, false},
		{"invalid env name", Environment{SecretGrants: []SecretGrant{{SecretID: "s", EnvName: "1bad"}}}, false},
		{"loader env name", Environment{SecretGrants: []SecretGrant{{SecretID: "s", EnvName: "LD_PRELOAD"}}}, false},
		{"docker env name", Environment{SecretGrants: []SecretGrant{{SecretID: "s", EnvName: "DOCKER_HOST"}}}, false},
		{"path env name", Environment{SecretGrants: []SecretGrant{{SecretID: "s", EnvName: "PATH"}}}, false},
		{"dup env name", Environment{SecretGrants: []SecretGrant{{SecretID: "a", EnvName: "K"}, {SecretID: "b", EnvName: "K"}}}, false},
		{"empty mount", Environment{WritableMounts: []string{"  "}}, false},
		{"valid", Environment{AllowedTools: []string{ToolShell}, Network: NetworkPolicy{Default: "deny"}, SecretGrants: []SecretGrant{{SecretID: "s", EnvName: "API_KEY"}}}, true},
	}
	for _, tc := range cases {
		err := tc.env.Validate()
		if tc.ok && err != nil {
			t.Fatalf("%s: want valid, got %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Fatalf("%s: want invalid, got nil", tc.name)
		}
	}
}

func TestNormalize(t *testing.T) {
	e := Environment{}.Normalize()
	if e.Network.Default != "deny" {
		t.Fatalf("normalize should default network to deny, got %q", e.Network.Default)
	}
}
