# Attribution

The `internal/jarvis` package draws architectural inspiration from open-source
projects. None of their source code is copied verbatim - this is a Go rewrite
informed by reading their codebases.

## Architectural inspiration

- **github.com/dnhkng/GLaDOS** (MIT) - the conversation-loop architecture
  (capture → VAD → wake → STT → LLM → TTS → playback with sentence-streaming
  and barge-in) follows GLaDOS's design. We do not copy any GLaDOS code.

## Bundled / referenced models

- **github.com/jgkawell/jarvis** (MIT) - Piper TTS voice model
  `en_GB-jarvis-high.onnx`. Finetuned from the upstream Piper `en_US-lessac`
  voice over 1000 epochs. Distributed under MIT. Used at runtime by shelling
  out to `piper` with this model file.

## Wake word

- **github.com/dscripka/openWakeWord** (Apache-2.0) - pretrained `hey_jarvis`
  ONNX model. Loaded via `yalue/onnxruntime_go` at runtime.

## Speech-to-text

- **github.com/ggml-org/whisper.cpp** (MIT) - `bindings/go` for inference,
  `tiny.en` model + CoreML companion for ANE acceleration.

## Audio I/O

- **github.com/gen2brain/malgo** (Unlicense) - miniaudio Go bindings for
  microphone capture and speaker playback.
