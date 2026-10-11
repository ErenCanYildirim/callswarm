# Quickstart

This gets callswarm running against its built-in fake agent, then shows how to write a scenario and point it at your own agent. For what the numbers mean, see [metrics.md](metrics.md).

## Install

callswarm needs Go 1.26 or newer.

```
go install github.com/ErenCanYildirim/callswarm/cmd/callswarm@latest
```

That puts `callswarm` in Go's bin directory (`go env GOPATH` plus `\bin` or `/bin`), which needs to be on your PATH. To build from a checkout instead:

```
go build -o callswarm ./cmd/callswarm        # Linux and macOS: run it as ./callswarm
go build -o callswarm.exe ./cmd/callswarm    # Windows: run it as .\callswarm
```

PowerShell doesn't run programs from the current directory by name, so a local build must be run as `.\callswarm`.

## Run it against the fake agent

The fake agent echoes whatever the caller says, with fixed timing: it answers exactly 500 ms after the caller stops (300 ms of silence to end the turn plus a 200 ms delay) and stops within about 60 ms when interrupted. Since the right answer is known, it shows whether callswarm is measuring correctly on your machine.

In one terminal, from the repository root:

```
callswarm fake-agent
```

In a second terminal, also from the repository root:

```
callswarm run examples/smoke/scenario.yaml --url ws://127.0.0.1:8080
```

The output looks like this:

```
callswarm dev - smoke
1 calls, peak 1 concurrent, 10.1s against ws://127.0.0.1:8080
health: HEALTHY

Time to first audio
  2 replies to 2 turns

  calls active        p50      p90      p95      p99      max      n
  all               501ms    501ms    501ms    501ms    501ms      2

Interruptions
  1 interruption: stopped within 1s in 1 (100%), sent clear in 1 (100%)
  time talking over the caller: p50 65ms, p90 65ms, max 65ms (n=1)

Caller pacing
  0.40% of 503 frames sent late, mean lateness 0.7ms, worst 42ms

Warnings
  - only 2 first-audio samples: p99 is close to the maximum and not a stable estimate.

Checks: all 2 passed
  ok    failed calls  0 of 1, want at most 0
  ok    health        healthy, want healthy or degraded
```

First audio close to 500 ms and a stop around 60 ms mean the measurement works. Now the same scenario as a load test, 50 calls with up to 20 at once, ramping up over 5 seconds:

```
callswarm run examples/smoke/scenario.yaml --url ws://127.0.0.1:8080 --calls 50 --concurrency 20 --ramp-up 5s --out report.json
```

The first-audio table gains a row for each range of concurrent calls, so you can see whether latency grows with load. `--out` writes the full JSON report, including every call's event timeline. Stop the fake agent with Ctrl-C when you're done.

## Write a scenario

A scenario is a YAML file listing what the caller does, in order. Clip paths are relative to the scenario file.

```yaml
name: book-appointment
reply_gap: 1.2s
steps:
  - listen                       # wait for the agent's greeting
  - say: clips/hello.wav         # speak, then wait for the reply
  - say: clips/question.wav
    wait: none                   # don't wait for the reply...
  - interrupt: clips/stop.wav    # ...talk over it instead,
    after: 800ms                 # 800 ms after the agent starts speaking
  - pause: 2s                    # stay silent
```

**Steps.** Each step does exactly one thing:

| Step | What the caller does |
|---|---|
| `listen` | Waits for the agent to speak and finish, without saying anything. Use it for a greeting. |
| `say: clip.wav` | Plays the clip, then by default waits for the agent's reply and for the reply to finish. |
| `interrupt: clip.wav` with `after: 800ms` | Waits until the agent is speaking, then plays the clip `after` that long into its speech. By default it then waits until the agent is quiet. |
| `pause: 2s` | Stays silent. The caller's line still carries silence, as a phone would. |

**What to wait for.** `wait:` on a `say` or `interrupt` step sets what happens after the clip:

| `wait:` | Meaning |
|---|---|
| `reply` | Wait for the agent to start a reply and finish it. The default for `say`. |
| `quiet` | Wait until the agent has stopped talking. The default for `interrupt`. |
| `none` | Go straight to the next step. Use it before an `interrupt`, so the caller talks over the reply. |
| a duration, such as `2s` | Wait that long. |

**Settings.** These apply to the whole scenario:

| Setting | Default | Meaning |
|---|---|---|
| `name` | none | Shown in the report. |
| `reply_timeout` | `10s` | How long to wait for the agent to start speaking before the step fails. |
| `reply_gap` | `700ms` | How long the agent must be silent for its reply to count as finished. |
| `max_reply` | `60s` | The longest one agent reply may run before the step fails. |

`reply_gap` needs care. It has to be longer than the longest pause the agent makes between sentences of one reply, or callswarm takes that pause for the end of the reply and the next step talks over the rest of it. LLM agents often pause 700 to 900 ms between sentences, so 1.2s is a safer start for them than the default; see the [benchmark](benchmark.md) for a case where 700 ms was too short. Setting it much longer only makes calls slower.

**Clips.** Clips are WAV files with 16-bit samples at any sample rate. Stereo is mixed down to mono, and everything is converted to 8 kHz telephone audio when the scenario loads. Speech boundaries come from the audio itself, so silence at the start or end of a recording doesn't distort the timing. For a real agent, use real recorded speech: its speech recognition has to understand the caller. Test tones like the smoke example's are only for the fake agent.

**Interruptions.** `after` has to be shorter than the agent's reply. If the agent finishes before that point, the step fails with "agent stopped speaking ... before the interruption point". To test whether an agent stops, make the interrupted reply long, at least a few seconds, so an agent that ignores the interruption is still talking when `--stop-within` runs out.

**When a step fails,** for example because the agent didn't answer within `reply_timeout`, that call stops there and counts as failed. The steps it completed still count in the metrics.

## Point it at your agent

**Over Twilio Media Streams.** Any agent that accepts phone calls through Twilio speaks this protocol, and callswarm connects to its WebSocket endpoint as Twilio would:

```
callswarm run scenario.yaml --url wss://agent.example.com/ws --header "Authorization: Bearer ..." --param customer_id=42
```

`--header` adds a header to the WebSocket request. `--param` becomes a custom parameter in the stream's start message, where Twilio puts the `<Parameter>` values from TwiML. Many Twilio bots are set up to hang up through Twilio's REST API and need small changes before a test client can call them: see [Pipecat](pipecat.md) and [Jargo](jargo.md).

**Through LiveKit.** For agents that answer in a LiveKit room, use `--transport livekit`; see [livekit.md](livekit.md).

## Use it in CI

`run` exits nonzero when a check fails, so it can gate a pipeline. Set limits from a few runs of your own agent rather than guessing:

```
callswarm run scenario.yaml --url ws://127.0.0.1:8080 --calls 20 --concurrency 10 \
  --max-first-audio p95=1500ms --max-stop-latency p90=600ms --min-reply-rate 1 --min-stop-rate 0.95
```

[`.github/workflows/smoke.yml`](../.github/workflows/smoke.yml) is a complete GitHub Actions example, and [metrics.md](metrics.md#checks) lists every check.
