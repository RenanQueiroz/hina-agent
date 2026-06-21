// Package assets manages Hina's pinned local-inference downloads: the ONNX
// Runtime shared library and the Supertonic 3 TTS model files. Per
// research-findings B10 the rule is "download from the original source at install
// time, never re-host, pin every asset to a specific release/revision and verify
// checksums." Everything here is pinned to an exact ORT release and an exact
// HuggingFace commit, with SHA256 verification, and installs into an app-managed
// directory surfaced by `hina doctor` and the admin UI.
package assets

import (
	"path/filepath"
)

// Pins. ORT 1.26.0 is the release the yalue v1.31.0 binding's C API (v26)
// requires; the Supertonic revision is the HF main commit these checksums match.
// The Nemotron revision is the smcleod int8 export commit (the primary ASR model
// per research-findings B3/B10: combined decoder_joint, full chunk range,
// OpenMDW-1.1) these checksums match.
const (
	ORTVersion         = "1.26.0"
	SupertonicRevision = "3cadd1ee6394adea1bd021217a0e650ede09a323"
	NemotronRevision   = "f1f26d22dab5c4eabe6d01b63c906889e7e817d3"
	// SileroRevision is the snakers4/silero-vad commit for tag v5.1.2 (the MIT
	// silero_vad.onnx these checksums match; research-findings B4). The model is
	// served raw from GitHub at the pinned commit — an immutable URL, so the pin +
	// SHA256 verify exactly the bytes the VAD engine loads.
	SileroRevision    = "6478567951ae5c9979ad7b234185b5515f4be7a1"
	supertonicBaseURL = "https://huggingface.co/Supertone/supertonic-3/resolve/" + SupertonicRevision + "/"
	nemotronBaseURL   = "https://huggingface.co/smcleod/nemotron-3.5-asr-streaming-0.6b-int8/resolve/" + NemotronRevision + "/"
	sileroBaseURL     = "https://raw.githubusercontent.com/snakers4/silero-vad/" + SileroRevision + "/"
	ortBaseURL        = "https://github.com/microsoft/onnxruntime/releases/download/v" + ORTVersion + "/"
)

// ArchiveKind tags how a downloaded artifact is unpacked.
type ArchiveKind int

const (
	ArchiveNone  ArchiveKind = iota // the download IS the installed file
	ArchiveTarGz                    // extract Member from a .tgz
	ArchiveZip                      // extract Member from a .zip
)

// Asset is one pinned download. SHA256/Size describe the downloaded artifact (the
// archive for ORT, the file itself otherwise). Dest is the installed path
// relative to the assets root. For archives, Member is the path inside the
// archive extracted to Dest, and MemberSHA256/MemberSize pin the extracted file
// itself, so the installed library is checksum-verified on disk (not merely
// present) and a truncated/tampered/partial install is detected.
type Asset struct {
	Name         string
	URL          string
	SHA256       string
	Size         int64
	Dest         string
	Archive      ArchiveKind
	Member       string
	MemberSHA256 string
	MemberSize   int64
}

// DiskDigest returns the expected SHA256 and size of the installed file at Dest:
// for a direct download that is the artifact itself; for an archive it is the
// extracted member. Either may be empty/zero if not pinned.
func (a Asset) DiskDigest() (sha string, size int64) {
	if a.Archive == ArchiveNone {
		return a.SHA256, a.Size
	}
	return a.MemberSHA256, a.MemberSize
}

// Layout returns the directories the runtime + engine read from, under root.
// libDir is passed to onnx as the ORT search dir; onnxDir/voiceDir are the
// Supertonic model + voice dirs.
func Layout(root string) (libDir, onnxDir, voiceDir string) {
	return filepath.Join(root, "ort"),
		filepath.Join(root, "supertonic", "onnx"),
		filepath.Join(root, "supertonic", "voice_styles")
}

// ASRDir is the Nemotron model directory under root (encoder.onnx + .data,
// decoder_joint.onnx, tokenizer.model).
func ASRDir(root string) string { return filepath.Join(root, "nemotron") }

// ASREncoderPath is the installed encoder.onnx path. The ASR engine loads the
// encoder by PATH (not bytes) because ORT resolves its external weights file
// (encoder.onnx.data) relative to the model on disk; pass this after verifying
// the full manifest so the loaded graph is the checksum-verified one.
func ASREncoderPath(root string) string { return filepath.Join(ASRDir(root), "encoder.onnx") }

// VADDir is the Silero VAD model directory under root (silero_vad.onnx). VADModelPath
// is the installed model file. The VAD engine loads the model from VERIFIED BYTES
// (it has no external-data file), so the path is only for presence reporting.
func VADDir(root string) string       { return filepath.Join(root, "vad") }
func VADModelPath(root string) string { return filepath.Join(VADDir(root), "silero_vad.onnx") }

// ortAssets is the ORT shared library per platform. Microsoft dropped CPU builds
// for some targets, so not every Tier-1 platform has one (e.g. macOS x64 after
// 1.23.2); those return ok=false and local TTS stays unavailable there.
var ortAssets = map[string]Asset{
	"linux/amd64": {
		Name: "onnxruntime-linux-x64", URL: ortBaseURL + "onnxruntime-linux-x64-" + ORTVersion + ".tgz",
		SHA256: "1254da24fb389cf39dc0ff3451ab48301740ffbfcbaf646849df92f80ee92c57", Size: 8590023,
		Archive: ArchiveTarGz, Member: "onnxruntime-linux-x64-" + ORTVersion + "/lib/libonnxruntime.so." + ORTVersion,
		Dest:         filepath.Join("ort", "lib", "libonnxruntime.so."+ORTVersion),
		MemberSHA256: "5bd5bedf736fc501692435d0ec4f6e8b2bdf48cd30af8e6d00d61b3ddc9a7ab8", MemberSize: 23023576,
	},
	"darwin/arm64": {
		Name: "onnxruntime-osx-arm64", URL: ortBaseURL + "onnxruntime-osx-arm64-" + ORTVersion + ".tgz",
		SHA256: "7a1280bbb1701ea514f71828765237e7896e0f2e1cd332f1f70dbd5c3e33aca3", Size: 31717869,
		Archive: ArchiveTarGz, Member: "onnxruntime-osx-arm64-" + ORTVersion + "/lib/libonnxruntime." + ORTVersion + ".dylib",
		Dest:         filepath.Join("ort", "lib", "libonnxruntime."+ORTVersion+".dylib"),
		MemberSHA256: "30afadcfc3c704f7671f8430d6252956651c1972373901d2be629da2e6a4d8ee", MemberSize: 37310032,
	},
	"windows/amd64": {
		Name: "onnxruntime-win-x64", URL: ortBaseURL + "onnxruntime-win-x64-" + ORTVersion + ".zip",
		SHA256: "6ebe99b5564bf4d029b6e93eac9ff423682b6212eade769e9ca3f685eaf500b4", Size: 75675381,
		Archive: ArchiveZip, Member: "onnxruntime-win-x64-" + ORTVersion + "/lib/onnxruntime.dll",
		Dest:         filepath.Join("ort", "lib", "onnxruntime.dll"),
		MemberSHA256: "b2ba7ca16e0e4fe71ad5148744ab885a2f5809e52a0c3de4d9ba3853a03977f9", MemberSize: 14897976,
	},
}

// WindowsLocalVoiceGated keeps Windows local voice behind the Phase 11 ORT-DLL
// validation gate (phase-04-local-tts.md "Explicitly out", phase-11; research-
// findings C1). While true, Windows is treated as having no ORT build, so
// `hina assets pull` refuses and `hina doctor` reports local TTS unavailable
// there — even though the Windows ORT asset is pinned and ready. Flip to false
// once Phase 11 validates the DLL-load path on a real Windows host.
const WindowsLocalVoiceGated = true

// ORTAsset returns the pinned ORT library for a GOOS/GOARCH, ok=false if Microsoft
// ships no CPU build for that platform at this version, or (for Windows) while it
// remains behind the Phase 11 validation gate.
func ORTAsset(goos, goarch string) (Asset, bool) {
	if WindowsLocalVoiceGated && goos == "windows" {
		return Asset{}, false
	}
	a, ok := ortAssets[goos+"/"+goarch]
	return a, ok
}

// supModel is one direct-download Supertonic file: HF path -> installed Dest, with
// its pinned size + sha256.
type supModel struct {
	path   string
	dest   string
	size   int64
	sha256 string
}

var supModels = []supModel{
	// ONNX graphs (LFS; sha256 = HF lfs.oid).
	{"onnx/duration_predictor.onnx", filepath.Join("supertonic", "onnx", "duration_predictor.onnx"), 3700147, "c3eb91414d5ff8a7a239b7fe9e34e7e2bf8a8140d8375ffb14718b1c639325db"},
	{"onnx/text_encoder.onnx", filepath.Join("supertonic", "onnx", "text_encoder.onnx"), 36416150, "c7befd5ea8c3119769e8a6c1486c4edc6a3bc8365c67621c881bbb774b9902ff"},
	{"onnx/vector_estimator.onnx", filepath.Join("supertonic", "onnx", "vector_estimator.onnx"), 256534781, "883ac868ea0275ef0e991524dc64f16b3c0376efd7c320af6b53f5b780d7c61c"},
	{"onnx/vocoder.onnx", filepath.Join("supertonic", "onnx", "vocoder.onnx"), 101424195, "085de76dd8e8d5836d6ca66826601f615939218f90e519f70ee8a36ed2a4c4ba"},
	// Config + tokenizer (non-LFS; sha256 computed at the pinned revision).
	{"onnx/tts.json", filepath.Join("supertonic", "onnx", "tts.json"), 8253, "42078d3aef1cd43ab43021f3c54f47d2d75ceb4e75f627f118890128b06a0d09"},
	{"onnx/unicode_indexer.json", filepath.Join("supertonic", "onnx", "unicode_indexer.json"), 277676, "9bf7346e43883a81f8645c81224f786d43c5b57f3641f6e7671a7d6c493cb24f"},
	// Preset voices (no cloning — research-findings B10).
	{"voice_styles/F1.json", filepath.Join("supertonic", "voice_styles", "F1.json"), 292046, "bbdec6ee00231c2c742ad05483df5334cab3b52fda3ba38e6a07059c4563dbc2"},
	{"voice_styles/F2.json", filepath.Join("supertonic", "voice_styles", "F2.json"), 292423, "7c722c6a72707b1a77f035d67f0d1351ba187738e06f7683e8c72b1df3477fc6"},
	{"voice_styles/F3.json", filepath.Join("supertonic", "voice_styles", "F3.json"), 290794, "12f6ef2573baa2defa1128069cb59f203e3ab67c92af77b42df8a0e3a2f7c6ab"},
	{"voice_styles/F4.json", filepath.Join("supertonic", "voice_styles", "F4.json"), 291808, "c2fa764c1225a76dfc3e2c73e8aa4f70d9ee48793860eb34c295fff01c2e032b"},
	{"voice_styles/F5.json", filepath.Join("supertonic", "voice_styles", "F5.json"), 291479, "45966e73316415626cf41a7d1c6f3b4c70dbc1ba2bee5c1978ef0ce33244fc8d"},
	{"voice_styles/M1.json", filepath.Join("supertonic", "voice_styles", "M1.json"), 291748, "e35604687f5d23694b8e91593a93eec0e4eca6c0b02bb8ed69139ab2ea6b0a5b"},
	{"voice_styles/M2.json", filepath.Join("supertonic", "voice_styles", "M2.json"), 292055, "b76cbf62bac707c710cf0ae5aba5e31eea1a6339a9734bfae33ab98499534a50"},
	{"voice_styles/M3.json", filepath.Join("supertonic", "voice_styles", "M3.json"), 290198, "ea1ac35ccb91b0d7ecad533a2fbd0eec10c91513d8951e3b25fbba99954e159b"},
	{"voice_styles/M4.json", filepath.Join("supertonic", "voice_styles", "M4.json"), 291522, "ca8eefad4fcd989c9379032ff3e50738adc547eeb5e221b82593a6d7b3bac303"},
	{"voice_styles/M5.json", filepath.Join("supertonic", "voice_styles", "M5.json"), 291469, "dd22b92740314321f8ae11c5e87f8dd60d060f15dd3a632b5adf77f471f77af2"},
}

// SupertonicAssets returns the platform-independent Supertonic model assets.
func SupertonicAssets() []Asset {
	out := make([]Asset, 0, len(supModels))
	for _, m := range supModels {
		out = append(out, Asset{
			Name: m.path, URL: supertonicBaseURL + m.path,
			SHA256: m.sha256, Size: m.size, Dest: m.dest, Archive: ArchiveNone,
		})
	}
	return out
}

// nemoModels are the pinned Nemotron 3.5 streaming int8 files (sha256 = HF
// lfs.oid for the LFS objects). The encoder carries its weights in an external
// encoder.onnx.data file; both the graph and the data file are pinned + verified
// so the by-path encoder load consumes only checksum-verified bytes. config.json
// is intentionally omitted — the language prompt dictionary and all model dims
// are embedded in internal/asr, so no sidecar config is read at run time.
var nemoModels = []supModel{
	{"encoder.onnx", filepath.Join("nemotron", "encoder.onnx"), 42963073, "a6fd0bbedae97047cb444dba928273b66b9cae36249cf697f4bf7b6f0e167c5d"},
	{"encoder.onnx.data", filepath.Join("nemotron", "encoder.onnx.data"), 614649600, "c2f230b026aa4f29b1b5ce099b2fba853db361773157d478d67127b877f64c42"},
	{"decoder_joint.onnx", filepath.Join("nemotron", "decoder_joint.onnx"), 24483962, "7fe1a8c2e247b55bbb8ca917ef64cf60227909c6fe63be2da7ea6fc3858d6a69"},
	{"tokenizer.model", filepath.Join("nemotron", "tokenizer.model"), 406554, "ce3895e40806f02a26c3a225161b96ef682d6c0054bae32a245dec4258d7d291"},
}

// NemotronAssets returns the platform-independent Nemotron ASR model assets.
func NemotronAssets() []Asset {
	out := make([]Asset, 0, len(nemoModels))
	for _, m := range nemoModels {
		out = append(out, Asset{
			Name: "nemotron/" + m.path, URL: nemotronBaseURL + m.path,
			SHA256: m.sha256, Size: m.size, Dest: m.dest, Archive: ArchiveNone,
		})
	}
	return out
}

// vadModels is the pinned Silero VAD model (MIT). It is a single self-contained
// ONNX graph (no external weights), loaded from verified bytes. sha256 is the raw
// file digest at the pinned commit. The HF/GitHub "path" is the in-repo path the
// raw URL appends to sileroBaseURL.
var vadModels = []supModel{
	{"src/silero_vad/data/silero_vad.onnx", filepath.Join("vad", "silero_vad.onnx"), 2327524, "2623a2953f6ff3d2c1e61740c6cdb7168133479b267dfef114a4a3cc5bdd788f"},
}

// VADAssets returns the platform-independent Silero VAD model assets.
func VADAssets() []Asset {
	out := make([]Asset, 0, len(vadModels))
	for _, m := range vadModels {
		out = append(out, Asset{
			Name: "vad/silero_vad.onnx", URL: sileroBaseURL + m.path,
			SHA256: m.sha256, Size: m.size, Dest: m.dest, Archive: ArchiveNone,
		})
	}
	return out
}

// Manifest is the full pinned asset set for a platform: the ORT library (when
// available) followed by the Supertonic (TTS), Nemotron (ASR), and Silero (VAD)
// models. unsupported is true when no ORT CPU build exists for the platform (no
// local voice can run there).
func Manifest(goos, goarch string) (list []Asset, ortUnsupported bool) {
	if a, ok := ORTAsset(goos, goarch); ok {
		list = append(list, a)
	} else {
		ortUnsupported = true
	}
	list = append(list, SupertonicAssets()...)
	list = append(list, NemotronAssets()...)
	list = append(list, VADAssets()...)
	return list, ortUnsupported
}

// TotalBytes is the sum of artifact sizes in a manifest (for "how much to
// download" reporting).
func TotalBytes(list []Asset) int64 {
	var n int64
	for _, a := range list {
		n += a.Size
	}
	return n
}
