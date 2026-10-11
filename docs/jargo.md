# Testing a Jargo agent

[Jargo](https://github.com/gojargo/jargo) is a Go port of Pipecat. Its Twilio transport (`transport/wsserver` with the `transport/wsserver/twilio` serializer) speaks the Twilio Media Streams protocol, so callswarm can call a Jargo agent directly with no Twilio account and no phone number. This page was written against Jargo v0.2.0, from its source; the Jargo bot in [`bench/jargo`](../bench/jargo) is a complete example.

## Building

- **Go 1.27.** Jargo's `go.mod` requires it. With an older Go and the default `GOTOOLCHAIN=auto`, `go build` downloads Go 1.27 for you.
- **Windows: use Docker.** Turn taking runs on the ONNX Runtime. Jargo's default, cgo-free build loads that library through purego's `Dlopen`, which only exists on Linux, macOS and the BSDs, so on Windows without a C compiler any program that imports Jargo's VAD or turn packages fails to compile. Build and run the bot on Linux or macOS; from Windows, run it in Docker. Jargo publishes base images for this (`gojargo/jargo-build` and `gojargo/jargo`), and [`bench/jargo/Dockerfile`](../bench/jargo/Dockerfile) builds the same way.
- **Windows: use WSL2, or cgo.** Turn taking runs on the ONNX Runtime. Jargo's default, cgo-free build loads that library through purego's `Dlopen`, which only exists on Linux, macOS and the BSDs, so on Windows without a C compiler any program that imports Jargo's VAD or turn packages fails to compile. Build and run the bot on Linux (WSL2 works) or macOS.
- **The ONNX Runtime library.** Turn taking (Silero VAD and Smart Turn v3) needs ONNX Runtime at run time; Jargo's Makefile fetches 1.30.0. Point Jargo at it with `JARGO_ONNXRUNTIME_LIB`. Jargo's own examples fall back to the speech-to-text service's endpointing when it is missing, which also loses barge-in; the benchmark bot refuses to start instead, so a missing library can't quietly change what is being measured.

## Point callswarm at the bot

Use the path your TwiML `<Stream>` names, usually `/ws`:

```
callswarm run scenario.yaml --url ws://127.0.0.1:8765/ws
```

## Turn off auto hang-up

Like Pipecat, the Twilio serializer hangs up through Twilio's REST API when the pipeline ends, and that is on by default. With it on and no credentials, the serializer's `Validate` fails when the pipeline starts (`twilio: AutoHangUp is on but credentials are missing`), so the bot never answers. callswarm is not a Twilio call, so turn it off:

```go
off := false
ser := twilio.New(twilio.Config{AutoHangUp: &off})
```

Jargo's `examples/twiliobot` describes `TWILIO_ACCOUNT_SID` and `TWILIO_AUTH_TOKEN` as optional, but passes them to the serializer without turning hang-up off, so with them unset it should fail this way, against callswarm or a real Twilio call.

## What callswarm sees

- **No marks.** Jargo neither sends `mark` messages nor reads the ones callswarm echoes back, so reports have no `agent_mark` events. callswarm finds the agent's speech from the audio itself.
- **Barge-in by `clear`.** On an interruption the serializer sends `clear`, and callswarm discards the agent's queued audio.
- **The stream SID comes from `start`.** The serializer learns the stream and call SIDs from Twilio's `start` message and drops outgoing audio until it has one. callswarm always sends `start` before any audio, so this only matters for a custom client.
- **The call ends when the socket closes.** `stop` is ignored; the transport's `Done` channel closes with the WebSocket, and the bot should cancel its pipeline then, as the example does.