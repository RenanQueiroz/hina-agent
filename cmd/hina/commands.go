package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/doctor"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
)

func cmdMigrate(args []string) error {
	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()

	// `hina migrate`            -> apply pending up migrations
	// `hina migrate down [N]`   -> roll back the last N (default 1) migrations
	// `hina migrate down all`   -> roll back everything
	if len(args) > 0 && args[0] == "down" {
		steps := 1
		if len(args) > 1 {
			if args[1] == "all" {
				steps = 0
			} else {
				n, err := strconv.Atoi(args[1])
				if err != nil || n < 1 {
					return fmt.Errorf("migrate down: invalid step count %q", args[1])
				}
				steps = n
			}
		}
		n, err := a.store.MigrateDown(context.Background(), steps)
		if err != nil {
			return err
		}
		fmt.Printf("migrations reverted: %d\n", n)
		return nil
	}

	n, err := a.store.Migrate(context.Background())
	if err != nil {
		return err
	}
	fmt.Printf("migrations applied: %d\n", n)
	return nil
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	_ = fs.Parse(args)

	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	ctx := context.Background()

	if _, err := a.store.Migrate(ctx); err != nil {
		return err
	}
	if _, err := platform.LoadOrCreateMasterKey(a.paths.MasterKeyPath()); err != nil {
		return err
	}
	if err := writeDefaultConfigIfMissing(a.paths.ConfigFilePath()); err != nil {
		return err
	}
	res, err := auth.EnsureAdmin(ctx, a.store)
	if err != nil {
		return err
	}

	fmt.Println("Setup complete.")
	fmt.Println("  data dir: ", a.paths.Data)
	fmt.Println("  config:   ", a.paths.ConfigFilePath())
	if res.Created {
		printBootstrapCredential(res)
	} else {
		fmt.Println("  admin:     already configured")
	}
	return nil
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "output JSON (non-interactive)")
	_ = fs.Parse(args)

	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()

	rep := doctor.Run(context.Background(), a.cfg, a.paths)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printReport(rep)
	return nil
}

func printReport(r doctor.Report) {
	fmt.Printf("Hina doctor — %s/%s — %s — %s\n\n", r.OS, r.Arch, r.Tier, r.GoVersion)
	for _, c := range r.Checks {
		fmt.Printf("  [%-11s] %-34s %s\n", c.Status, c.Name, c.Detail)
	}
}

func printBootstrapCredential(res auth.BootstrapResult) {
	fmt.Println()
	fmt.Println("  +-- Admin bootstrap credential (shown once) ----------------")
	fmt.Printf("  |   username: %s\n", res.Username)
	fmt.Printf("  |   password: %s\n", res.Password)
	fmt.Println("  |   Change this on first login; LAN binding stays disabled")
	fmt.Println("  |   until it is changed.")
	fmt.Println("  +----------------------------------------------------------")
	fmt.Println()
}

const defaultConfig = `# Hina configuration. Environment variables (HINA_*) override these values.

[server]
host = "127.0.0.1"   # loopback by default; set a LAN address only after changing the admin password
port = 8733
lan_enabled = false  # required (with a non-loopback host) to bind the LAN
# tls_cert = "/path/to/cert.pem"
# tls_key  = "/path/to/key.pem"

[agent]
name = "Hina"
name_aliases = []

[llm]
# Admin-owned text backend (users never choose). "mock" needs no credentials.
# "openai" = cloud OpenAI via the Responses API. "openai-compat" = any
# OpenAI-compatible /chat/completions endpoint, e.g. a local llama.cpp server.
provider = "mock"   # mock | openai | openai-compat
# model = "gpt-5.4-mini"
# base_url = "http://localhost:8080/v1"   # required for openai-compat (local)
# api_key = "${OPENAI_API_KEY}"
system_prompt = "You are Hina, a helpful, concise assistant."

# [paths]  # optional overrides of the OS-resolved app directories
# data_dir = "/var/lib/hina"
# cache_dir = "/var/cache/hina"
# log_dir = "/var/log/hina"

[log]
level = "info"    # debug|info|warn|error
format = "text"   # text|json
`

func writeDefaultConfigIfMissing(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfig), 0o600)
}
