# callswarm

A Go library and CLI that plays the caller against a voice agent. It runs scripted conversations at scale and measures how the agent behaves, so you don't have to test by calling your agent yourself.

## Scope

- **Transport:** WebSocket audio streaming using the Twilio Media Streams protocol (μ-law, 8 kHz, 20 ms frames). Agent frameworks that accept phone calls through Twilio, such as Pipecat and Jargo, can be tested without changes.
- **Scripted callers:** conversations built from pre-recorded audio files, pauses and interruption offsets.
- **Load:** a concurrency runner that ramps up simultaneous sessions.
- **Metrics:**
  - *Time to first audio:* from the end of caller speech (detected by VAD on the caller's own audio) to the first non-silent agent audio.
  - *Interruption handling:* caller audio injected at a set offset while the agent speaks; time until agent audio stops, and whether it stops at all within a timeout.
  - *Latency distribution:* p50, p90, p99 and max per run, reported alongside the load generator's own pacing jitter.
- **CI:** reproducible runs with threshold assertions and nonzero exit codes.

## Usage

Install with `go install github.com/ErenCanYildirim/callswarm/cmd/callswarm@latest`, then try it against the built-in fake agent:

```
callswarm fake-agent &
callswarm run scenario.yaml --url ws://127.0.0.1:8080 --calls 50 --concurrency 20 --ramp-up 10s --out report.json
```

`run` prints a summary and, with `--out`, writes the full JSON report. Ctrl-C stops a run and still prints the report for the calls that started. Run `callswarm run -h` for all flags.

## Not included for now

- Phone network (SIP/PSTN) testing.
- Synthetic caller voices (TTS-generated callers).
- LLM-based judging of conversation quality.
- Hosted service, dashboards or accounts.
- WebRTC transports. A LiveKit transport may follow once the WebSocket path is solid.

## Layout

```
cmd/callswarm/  CLI entry point
audio/          WAV loading, μ-law codec, resampling, frame chunking
twilio/         Twilio Media Streams protocol messages
fakeagent/      Key-free echo agent with configurable timing, used as the test target
transport/      Caller side of a Twilio Media Streams call: handshake, paced audio, playback
scenario/       Scenario file format and the scripted caller that runs it
vad/            Voice activity detection for caller clips and agent audio
timeline/       Timestamped event log of a call
metrics/        Time to first audio and interruption handling, from the timeline
load/           Concurrent calls with ramp-up, per-call timeouts and cancellation
summary/        Latency percentiles, latency by load, and whether the run can be trusted
report/         JSON report with a reproducibility manifest, and a plain-text summary
internal/cli/   Command-line interface
```

## Development

Requires Go 1.24 or newer.

```
go test -race ./...
golangci-lint run
```

## License

MIT. See [LICENSE](LICENSE).