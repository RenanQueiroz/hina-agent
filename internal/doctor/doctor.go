// Package doctor reports host capabilities and per-feature availability. It is
// the user's primary "what works on my machine" surface and is built before the
// features it reports on, returning "unavailable" until each lands.
package doctor

import (
	"context"
	"runtime"
	"strings"

	"github.com/RenanQueiroz/hina-agent/internal/config"
	"github.com/RenanQueiroz/hina-agent/internal/platform"
	"github.com/RenanQueiroz/hina-agent/internal/store"
)

// Check is one capability result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | missing | unavailable | warn | error
	Detail string `json:"detail"`
}

// Report is the full host capability report.
type Report struct {
	OS        string  `json:"os"`
	Arch      string  `json:"arch"`
	Tier      string  `json:"tier"`
	GoVersion string  `json:"go_version"`
	Checks    []Check `json:"checks"`
}

// Run gathers the capability report. It does not mutate state beyond ensuring
// the application directories exist.
func Run(ctx context.Context, cfg config.Config, paths platform.Paths) Report {
	r := Report{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Tier:      tier(),
		GoVersion: runtime.Version(),
	}

	// Application directories.
	if err := platform.EnsureAll(paths); err != nil {
		r.add("app directories", "error", err.Error())
	} else {
		r.add("app directories", "ok", paths.Data)
	}

	// Database.
	if st, err := store.Open(paths.DBPath()); err != nil {
		r.add("database (sqlite)", "error", err.Error())
	} else {
		_ = st.Close()
		r.add("database (sqlite)", "ok", paths.DBPath())
	}

	// External runtimes (present-or-not; deeper validation in their phases).
	r.addTool(ctx, "docker", "Phase 7 sandbox prerequisite", "docker", "--version")
	r.addTool(ctx, "sbx", "Phase 7 sandbox runtime", "sbx", "--version")
	r.addTool(ctx, "llama.cpp (llama-server)", "Phase 4 local LLM", "llama-server", "--version")

	// WebRTC voice bridge — pure Go (Pion), so always available with no native
	// toolchain. Hands-on browser loopback is validated in Phase 11.
	r.add("webrtc voice bridge (pion)", "ok", "pure-Go media bridge; no native toolchain")

	// HTTPS / LAN. Browser mic capture (getUserMedia) requires a secure context:
	// localhost is exempt, but a second LAN device needs HTTPS with a real cert.
	if cfg.Server.TLSEnabled() {
		r.add("https cert", "ok", cfg.Server.TLSCert)
	} else {
		r.add("https cert", "unavailable", "no cert configured (localhost mic is fine; LAN mic needs HTTPS — see mkcert/reverse-proxy guidance)")
	}

	// Local ONNX voice — gated on the ORT/DLL spike; not yet implemented.
	r.add("local voice (ASR/TTS via ONNX)", "unavailable", "not yet implemented (Phases 4–5; Windows gated to Phase 11)")

	return r
}

func (r *Report) add(name, status, detail string) {
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Detail: detail})
}

func (r *Report) addTool(ctx context.Context, name, purpose, bin string, args ...string) {
	path, err := platform.LookPath(bin)
	if err != nil {
		r.add(name, "missing", "not installed — "+purpose)
		return
	}
	out, err := platform.Output(ctx, path, args...)
	if err != nil {
		r.add(name, "ok", path)
		return
	}
	r.add(name, "ok", firstLine(string(out)))
}

func firstLine(s string) string {
	return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
}

func tier() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64", "darwin/arm64":
		return "Tier 1"
	case "windows/amd64":
		return "Tier 1 (built; hands-on validation pending — Phase 11)"
	default:
		return "Tier 2 / unvalidated"
	}
}
