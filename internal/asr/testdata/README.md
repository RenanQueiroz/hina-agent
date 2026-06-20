# ASR test fixtures

`speech.wav` — 16 kHz mono 16-bit PCM of the phrase "please turn on the light",
synthesized with `espeak-ng -s 105 -p 30` (deterministic). It is fed to the REAL
Nemotron model by the onnx-tagged integration test (`integration_onnx_test.go`,
gated on `HINA_ASR_TEST_ASSETS`) to guard the end-to-end speech path — log-mel
front-end fidelity, tokenizer/detokenizer, and RNNT decode — against a regression
that would produce empty/garbage transcripts. The synthetic voice is robotic, so
the test asserts that stable, high-confidence words appear (not an exact WER); the
real word-accuracy/biasing benchmark is the Phase 6 harness with recorded speech.
