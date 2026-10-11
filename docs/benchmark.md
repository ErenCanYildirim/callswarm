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

- **Docker.** Both bots run in containers, built from [`bench/compose.yaml`](../bench/compose.yaml). Jargo's turn taking does not build on Windows ([details](jargo.md#building)), and running both bots the same way keeps the comparison fair: same operating system, same network path from callswarm.
- **callswarm** on the host, installed with `go install ./cmd/callswarm`.
- **API keys** for Deepgram, Gemini and ElevenLabs.
- **Quota.** Each call speaks roughly 500 characters of ElevenLabs text (the greeting and three replies) and streams about 40 seconds of audio to Deepgram. 40 calls is about 20,000 characters; check your ElevenLabs plan, and its concurrency limit, before you start. Gemini's free tier has a low requests-per-minute limit, and a rate-limited reply shows up as a slow or missing one.

## Setup

Generate the caller's clips once and commit them (needs `GEMINI_API_KEY` in the environment):

```
cd bench/clips
python gen_clips.py
```

Put the keys in `bench/.env`, which is gitignored:

```
DEEPGRAM_API_KEY=...
GEMINI_API_KEY=...
ELEVENLABS_API_KEY=...
```

Build and start both bots from `bench/`. The first build downloads the base images and dependencies, a few hundred MB:

```
docker compose up --build
```

Leave it running; both bots log to that terminal. Pipecat listens on port 7860 and Jargo on 8765. If the Jargo bot exits at start with "turn taking unavailable", the ONNX Runtime library did not load, and the benchmark would not be like for like; don't run it until that is fixed.

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