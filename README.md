# callswarm

A Go library and CLI that plays the caller against a voice agent. It runs scripted conversations at scale and measures how the agent behaves, so you don't have to test by calling your agent yourself.

> Status: early development. Nothing here is stable yet.

## Scope

- **Transports:**
  - WebSocket audio streaming using the Twilio Media Streams protocol (μ-law, 8 kHz, 20 ms frames). Agent frameworks that accept phone calls through Twilio, such as Pipecat and Jargo, can be tested without changes.
  - LiveKit rooms, joined through the LiveKit Go SDK as an ordinary participant, for agents built on LiveKit Agents or anything else that answers in a LiveKit room. See [docs/livekit.md](docs/livekit.md).
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

### In CI

`run` checks the results and exits nonzero when a check fails, so it can gate a pipeline:

```
callswarm run scenario.yaml --url ws://127.0.0.1:8080 --calls 20 --concurrency 10 \
  --max-first-audio p95=700ms --max-stop-latency p95=300ms --min-reply-rate 1 --min-stop-rate 1
```

By default a run fails if any call fails (`--max-failed`) or if the test machine was too overloaded for its numbers to be trusted (`--fail-on unreliable`). Latency limits take `mean`, `p50`, `p90`, `p95`, `p99` or `max`. A limit with nothing to measure, such as an interruption check on a scenario without interruptions, fails rather than passes.

Exit codes: 0 all checks passed, 3 a check failed, 1 the run could not be carried out, 2 usage error, 130 cancelled.

[`.github/workflows/smoke.yml`](.github/workflows/smoke.yml) runs [`examples/smoke`](examples/smoke) against the fake agent on every push, and is written to be copied for your own agent.

### Agent frameworks

- [LiveKit](docs/livekit.md): calling an agent through a LiveKit room with `--transport livekit`, and how its measurements differ.
- [Pipecat](docs/pipecat.md) and [Jargo](docs/jargo.md): what to change in a Twilio bot so callswarm can call it, and what to expect in the results.
- [Benchmark](docs/benchmark.md): the two frameworks compared with the same providers, models and caller.

## Not included

- Phone network (SIP/PSTN) testing.
- Synthetic caller voices (TTS-generated callers).
- LLM-based judging of conversation quality.
- Hosted service, dashboards or accounts.
- WebRTC transports other than LiveKit.

## Layout

```
cmd/callswarm/           CLI entry point
audio/                   WAV loading, μ-law codec, resampling, frame chunking
twilio/                  Twilio Media Streams protocol messages
transport/               The Call interface, real-time caller audio pacing, and the Twilio Media Streams call
transport/livekit/       Calls through a LiveKit room, with pure-Go Opus
scenario/                Scenario file format and the scripted caller that runs it
vad/                     Voice activity detection for caller clips and agent audio
timeline/                Timestamped event log of a call
metrics/                 Time to first audio and interruption handling, from the timeline
load/                    Concurrent calls with ramp-up, per-call timeouts and cancellation
summary/                 Latency percentiles, latency by load, and whether the run can be trusted
check/                   Pass/fail checks on a run summary, for CI
report/                  JSON report with a reproducibility manifest, and a plain-text summary
internal/cli/            Command-line interface
fakeagent/               Key-free echo agent with configurable timing, used as the test target
fakeagent/livekitagent/  The same fake agent, joining LiveKit rooms
examples/                Example scenarios
bench/                   Pipecat and Jargo bots, their containers and the scenario for the framework benchmark
docs/                    Framework setup notes and benchmark results
```

## Development

Requires Go 1.26 or newer, and golangci-lint 2.14 or newer for linting.

```
go test -race ./...
golangci-lint run
```

The LiveKit tests skip unless `CALLSWARM_LIVEKIT_URL` points at a LiveKit server. With a development server running (see [docs/livekit.md](docs/livekit.md#trying-it-locally)):

```
CALLSWARM_LIVEKIT_URL=ws://127.0.0.1:7880 go test -race ./transport/livekit
```

## License

MIT. See [LICENSE](LICENSE).
