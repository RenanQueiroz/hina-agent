# vad testdata

`speech.wav` — a small committed 16 kHz mono PCM fixture ("please turn on the
light", synthesized with `espeak-ng -s 105 -p 30`), identical to the one in
`internal/asr/testdata`. The onnx-tagged `TestSileroRealVAD` feeds it through the
real Silero model and asserts the detector fires a speech segment on it while
staying silent on pure silence — a real-model check the fake-Model unit tests
can't provide. Kept as a copy (not a cross-package path) so the `vad` package
test is self-contained.
