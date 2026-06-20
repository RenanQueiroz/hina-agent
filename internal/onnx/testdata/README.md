# onnx test fixtures

`example_dynamic_axes.onnx` is a 320-byte ONNX model vendored from the
[`yalue/onnxruntime_go`](https://github.com/yalue/onnxruntime_go) test suite
(`test_data/example_dynamic_axes.onnx`, MIT-licensed, © Nathan Otterness). It has
one float32 input `input_vectors` of shape `[-1, 10]` and one float32 output
`output_scalars` of shape `[-1]`; the graph sums each length-10 row. It exists so
the `onnx`-tagged integration test (`integration_onnx_test.go`) can prove the real
ONNX Runtime loads from the app-managed library path and runs a model end to end
through our `Backend`/`Session` abstraction, without depending on the large
Supertonic model assets.
