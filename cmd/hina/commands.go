package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/RenanQueiroz/hina-agent/internal/assets"
	"github.com/RenanQueiroz/hina-agent/internal/auth"
	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/doctor"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
)

// assetsRoot resolves the single local-inference asset root for the app's paths
// (shared by TTS + ASR, per config.AssetsRoot).
func assetsRoot(cfg config.Config, paths platform.Paths) string {
	return cfg.AssetsRoot(paths.Cache)
}

// cmdAssets manages the pinned local-inference downloads (ONNX Runtime + the
// Supertonic TTS models): `hina assets status|verify|pull`.
func cmdAssets(args []string) error {
	fs := flag.NewFlagSet("assets", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "output JSON (non-interactive)")
	_ = fs.Parse(args)
	sub := "status"
	if fs.NArg() > 0 {
		sub = fs.Arg(0)
	}

	a, err := openApp()
	if err != nil {
		return err
	}
	defer a.close()
	root := assetsRoot(a.cfg, a.paths)

	switch sub {
	case "status", "verify":
		st := assets.VerifyLocal(root)
		if *asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(st); err != nil {
				return err
			}
		} else {
			printAssetStatus(st)
		}
		if sub == "verify" && !st.Complete {
			return errors.New("local-inference assets are incomplete (run: hina assets pull)")
		}
		return nil
	case "pull":
		// Install into an owner-private root so the server can later trust it for
		// native-library loading (same invariant the server enforces at startup).
		if err := assets.SecureRoot(root); err != nil {
			return fmt.Errorf("secure asset root %s: %w", root, err)
		}
		if err := assets.PullLocal(context.Background(), root, a.log); err != nil {
			return err
		}
		fmt.Println("assets installed:", root)
		return nil
	default:
		return fmt.Errorf("unknown assets subcommand %q (use status|verify|pull)", sub)
	}
}

func printAssetStatus(st assets.Status) {
	fmt.Printf("Local-inference assets (root: %s)\n", st.Root)
	fmt.Printf("  ONNX Runtime %s / Supertonic %s / Nemotron %s / Silero %s\n\n", assets.ORTVersion, assets.SupertonicRevision[:12], assets.NemotronRevision[:12], assets.SileroRevision[:12])
	if st.ORTUnsupported {
		fmt.Println("  [unsupported] no ONNX Runtime CPU build for this platform — local TTS unavailable here")
	}
	for _, a := range st.Assets {
		mark := "missing"
		switch {
		case a.Verified:
			mark = "ok"
		case a.Present:
			mark = "bad"
		}
		line := fmt.Sprintf("  [%-7s] %s", mark, a.Name)
		if a.Reason != "" {
			line += " — " + a.Reason
		}
		fmt.Println(line)
	}
	fmt.Println()
	if st.Complete {
		fmt.Println("  All assets present and verified.")
	} else {
		fmt.Println("  Incomplete. Run: hina assets pull")
	}
}

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
	if err := ensureMasterKey(a); err != nil {
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

[tts]
# Local text-to-speech (Phase 4, Supertonic via ONNX Runtime). Off by default and
# only usable in the onnx-tagged build with the model assets installed
# ("hina assets pull"); otherwise the engine reports unavailable in "hina doctor".
enabled = false
voice = "M1"        # preset voice id (F1..F5, M1..M5); no voice cloning
lang = "en"         # default language tag
# speed = 1.05      # tempo multiplier (>1 is faster)
# steps = 8         # flow-matching denoise steps (latency/quality tradeoff)
# idle_ttl = "5m"   # unload models after this idle period (keeps memory bounded)
# threads = 0       # ORT intra-op CPU threads (0 = ORT default)
# assets_dir = ""   # override the model/runtime asset root (default: OS cache dir)

[asr]
# Local streaming speech-to-text (Phase 5, Nemotron via ONNX Runtime). Off by
# default; usable only in the onnx-tagged build with the model assets installed
# ("hina assets pull"). Name biasing + wake-word stripping use [agent].name /
# name_aliases, so the agent name transcribes reliably and a leading address is
# removed before the request reaches the LLM.
enabled = false
language = "auto"     # default language tag ("en", "es", ..., or "auto" to detect)
# idle_ttl = "5m"     # unload models after this idle period
# threads = 0         # ORT intra-op CPU threads (0 = ORT default)
# context_score = 1.0 # name-biasing boost for a phrase's first token (tune on fixtures)
# depth_scaling = 2.0 # name-biasing multiplier for deeper tokens
# assets_dir = ""     # override the asset root (default: shared with [tts])

[voice]
# Live conversation loop (Phase 6): continuous VAD -> ASR -> agent -> TTS with
# speak-to-interrupt barge-in. Off by default; needs local VAD (Silero) + [asr] +
# [tts] all available (onnx-tagged build + "hina assets pull"). The per-session
# turn_detection (server_vad/semantic_vad) is chosen by the client; these set the
# VAD engine's default tunables.
enabled = false
# threshold = 0.5       # Silero speech-onset probability (0..1)
# silence_ms = 700      # trailing silence that ends a turn (server_vad)
# pre_speech_ms = 300   # pre-roll kept before speech onset (prefix padding)
# min_speech_ms = 250   # discard speech shorter than this (false-start rejection)
# max_duration_s = 30   # force-commit a turn after this long
# idle_ttl = "5m"       # unload the VAD model after this idle period

[sandbox]
# Docker 'sbx' runner that backs tool execution (Phase 7): shell/file/HTTP tool
# calls run inside the calling user's sandbox with explicit grants, resource
# limits, an approval gate, and an audit log — never on the host. The network
# allow-list is enforced at request time for network-explicit tools (http_fetch);
# a raw shell command's egress is bounded only by the operator's 'sbx' container
# policy (run sbx in a default-deny / Balanced/Locked-Down mode). Off by default;
# needs a pinned 'sbx' install (see 'hina doctor'). When enabled but 'sbx' is
# absent the server still runs and tool calls report it unavailable.
enabled = false
# sbx_path = ""          # override the sbx binary path (default: PATH lookup)
# kit = ""               # admin-controlled sbx kit/template
approval = "always"      # always (prompt per tool call) | auto (run without prompting; still audited)
# cpus = "2"             # default per-run CPU limit
# memory = "2g"          # default per-run memory limit
# pids = 512             # default per-run process limit (0 = omit)
# timeout = "5m"         # default per-run wall-clock cap
# workspace_quota_mb = 2048  # per-user durable workspace quota (0 = unlimited)
# scratch_ttl = "1h"     # reap ephemeral run scratch older than this
# approval_timeout = "5m"  # deny a tool call left undecided this long
# allow_version_mismatch = false  # run tools even if the installed sbx minor != the pinned version (after verifying the smoke test)
# network_isolated = false  # set true ONLY after locking down sbx's container egress
#   (Balanced/Locked-Down). Hina can't gate a raw shell command's network, so granted
#   secrets are injected into runs ONLY when this is true — fail closed by default.

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
