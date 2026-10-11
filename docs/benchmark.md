# Pipecat and Jargo benchmark

Two bots built to be identical except for the framework, called by callswarm with the same recorded caller. This page is how to run it; the results go at the end.

## What is compared

| | Pipecat bot | Jargo bot |
| --- | --- | --- |
| Framework | Pipecat 1.12.0 (Python) | Jargo v0.2.0 (Go) |
| Speech to text | Deepgram `nova-3-general` | same |
| LLM | Gemini `gemini-3.6-flash`, minimal thinking, 1024 max tokens | same |
| Text to speech | ElevenLabs `eleven_flash_v2_5`, voice `21m00Tcm4TlvDq8ikWAM` | same |
| Turn taking | Silero VAD, Smart Turn v3 | same |
| Pipeline sample rate | 16 kHz | same |
| Prompt and greeting | [`bot.py`](../bench/pipecat/bot.py) | [`main.go`](../bench/jargo/main.go), same text |

The caller ([`bench/scenario.yaml`](../bench/scenario.yaml)) listens to the greeting, asks two questions, cuts into the second answer one second in with a new question, waits for that answer and says goodbye. Each call gives three time-to-first-audio samples and one interruption.

Keep in mind what the numbers can and can't show:

- **Time to first audio is mostly the providers.** It runs from the end of the caller's speech through Smart Turn's end-of-turn decision, Deepgram's final transcript, Gemini's first tokens and ElevenLabs' first audio. The framework's own share is small, so a difference of a few tens of milliseconds between the two is within run-to-run noise unless it holds across rounds.
- **Replies differ between runs.** The LLM doesn't answer the same way twice, so reply lengths vary, and with them the interruption timing.
- **Same design, two implementations.** Jargo is a Go port of Pipecat, so this compares two implementations of one architecture, not two approaches.
- **Results depend on where you run them.** Provider latency depends on the distance to their servers; record where the run happened.

## Requirements

- **Linux.** Jargo's turn taking does not build on Windows without cgo ([details](jargo.md#building)), so run everything, both bots and callswarm, under WSL2 or on a Linux machine. Keep the repository on the Linux filesystem (under `~`, not `/mnt/c`).
- **Go 1.25 or newer** (it fetches Go 1.27 for the Jargo bot by itself) and **Python 3.11 or newer**.
- **API keys** for Deepgram, Gemini and ElevenLabs, exported as `DEEPGRAM_API_KEY`, `GEMINI_API_KEY` and `ELEVENLABS_API_KEY`.
- **Quota.** Each call speaks roughly 500 characters of ElevenLabs text (the greeting and three replies) and streams about 40 seconds of audio to Deepgram. 40 calls is about 20,000 characters; check your ElevenLabs plan, and its concurrency limit, before you start. Gemini's free tier has a low requests-per-minute limit, and a rate-limited reply shows up as a slow or missing one.

## Setup

Generate the caller's clips once and commit them:

```
cd bench/clips
python3 gen_clips.py
```

ONNX Runtime for the Jargo bot's turn taking:

```
cd ~
curl -LO https://github.com/microsoft/onnxruntime/releases/download/v1.30.0/onnxruntime-linux-x64-1.30.0.tgz
tar xzf onnxruntime-linux-x64-1.30.0.tgz
export JARGO_ONNXRUNTIME_LIB=$HOME/onnxruntime-linux-x64-1.30.0/lib/libonnxruntime.so
```

Pipecat bot, in its own terminal:

```
cd bench/pipecat
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
.venv/bin/python bot.py
```

Jargo bot, in another (with `JARGO_ONNXRUNTIME_LIB` set there too):

```
cd bench/jargo
go mod tidy
go run .
```

callswarm, in a third:

```
go install ./cmd/callswarm
```

## Run

First one call to each bot, to warm up connections and confirm the setup. Listen for problems in the bots' logs and check that each report shows replies to all three turns:

```
callswarm run bench/scenario.yaml --url ws://127.0.0.1:7860/ws
callswarm run bench/scenario.yaml --url ws://127.0.0.1:8765/ws
```

Then alternate between the frameworks, so that a provider having a slow few minutes affects both alike:

```
mkdir -p bench/results
callswarm run bench/scenario.yaml --url ws://127.0.0.1:7860/ws --calls 10 --concurrency 2 --out bench/results/pipecat-1.json
callswarm run bench/scenario.yaml --url ws://127.0.0.1:8765/ws --calls 10 --concurrency 2 --out bench/results/jargo-1.json
callswarm run bench/scenario.yaml --url ws://127.0.0.1:7860/ws --calls 10 --concurrency 2 --out bench/results/pipecat-2.json
callswarm run bench/scenario.yaml --url ws://127.0.0.1:8765/ws --calls 10 --concurrency 2 --out bench/results/jargo-2.json
```

Every report should say `health: HEALTHY`. If one doesn't, the test machine was too busy for its numbers to be trusted; close other programs and rerun that round.

## Results

Not run yet.