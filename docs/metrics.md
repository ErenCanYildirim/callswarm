# Metrics

What callswarm measures, exactly how, and where the numbers stop being trustworthy. The code that implements each definition is named so you can check it.

## How callswarm hears a call

Every measurement comes from one timeline of events per call: when the caller's speech started and ended, when the agent's speech started and stopped, and when the agent sent control messages such as `clear`. The JSON report (`--out`) includes each call's timeline, so any number in the summary can be traced back to the events behind it.

**Speech, not audio.** Both sides are run through the same voice activity detector (`vad/`), so timing follows the words rather than the audio stream:

- A 20 ms frame counts as speech when it is louder than -45 dBFS and at least 12 dB above the background noise. The background is the quietest frame in the last 2 seconds.
- Gaps shorter than 250 ms are bridged, as pauses between words. Bursts shorter than 60 ms are dropped, as clicks.

So silence at the end of a caller's recording, or silent frames an agent streams while thinking, don't count as speech.

**Caller speech** is found in each clip when the scenario loads, and placed on the timeline by when each frame was actually sent. Caller audio is streamed in real time, one 20 ms frame every 20 ms, with silence between clips, as a phone would.

**Agent speech** is timed by when it would be heard, not when it arrived:

- Agents often send audio faster than real time, so callswarm keeps track of a playback queue, as a phone does. Audio that arrives while earlier audio is still playing is placed after it.
- Over Twilio Media Streams, a `clear` message empties that queue, and audio that was never played is dropped from the timeline.
- Over LiveKit there is no `clear`. Audio is taken to play as it arrives, queued behind audio still playing.

## Time to first audio

Measured for every `say` step: **from the end of the caller's speech to the start of the agent's reply**, as heard.

- The reply is the first agent speech that starts after the caller began speaking in that step and before the caller next speaks. Agent speech already under way when the caller started belongs to an earlier turn.
- If the agent starts before the caller has finished, the value is negative, and the report counts the turn as "started before the caller finished".
- If no reply starts before the caller's next speech or the end of the call, the turn counts as not replied. It is not part of the latency distribution, but it does count against `--min-reply-rate`.
- `listen` and `interrupt` steps are not turns, and aren't measured here.

Code: `metrics.turn`.

## Interruptions

Measured for every `interrupt` step.

**Stop latency** runs from the start of the caller's interrupting speech to the moment the agent stopped talking. That's how long the two talked over each other.

"Stopped" means the first silence of at least 250 ms after the caller started, after which the agent didn't start talking again while the caller was still speaking. A pause between words that the agent follows by carrying on over the caller is not a stop. Speech after the caller has finished, such as the agent answering what it was interrupted with, doesn't undo a stop.

**Stopped within the limit:** the interruption counts as handled when stop latency is at most `--stop-within`, default 1s. An agent that never stopped during the call counts as not handled and adds no latency sample.

**Sent clear** applies only to Twilio. It reports whether the agent sent a `clear` in reaction to the interruption, either while the caller was still speaking or within 100 ms after its audio stopped. The grace exists because an agent streaming in real time can run out of queued audio a few milliseconds before its `clear` arrives behind it. An agent that never stopped but did send a `clear` also counts.

Code: `metrics.interruption`.

## Distributions

Percentiles use the nearest-rank method: p95 is the smallest value with at least 95% of values at or below it.

With few samples, the high percentiles are simply the largest values. p99 of 50 samples is the maximum. The report warns when there are fewer than 100 first-audio samples, and every distribution shows its count `n`.

**By concurrency.** The first-audio table splits turns into up to four ranges by how many calls were active when the caller finished speaking. Latency that grows down the table means the agent slows down under load. In a run with ramp-up, the ranges hold different numbers of turns, so compare the low ranges with care.

Code: `summary/`.

## Caller pacing and health

callswarm has to keep real-time pace on every call it runs. A frame is **late** when it goes out more than 5 ms after its schedule. When the machine running callswarm is overloaded, frames go out late, and the same scheduling delays land on the timestamps of the agent's audio, inflating its measured latency.

**Health** is a verdict on whether the numbers describe the agent or the test machine:

| Health | Late frames | Meaning |
|---|---|---|
| healthy | at most 1% | The numbers describe the agent. |
| degraded | more than 1% | Tail percentiles may be inflated by the test machine. |
| unreliable | more than 5% | Latency reflects the test machine as much as the agent. Use fewer concurrent calls, or a bigger machine. |

Health flags an overloaded run but doesn't correct its numbers.

## Checks

`callswarm run` evaluates these after every run and exits 3 if any fails. A check with nothing to measure fails rather than passes. A first-audio limit on a run where the agent never replied, or a stop-rate check on a scenario without interruptions, means the run didn't test what the limit asks about.

| Flag | Default | Passes when |
|---|---|---|
| `--max-failed N` | `0` | At most N calls failed. `-1` allows any number. |
| `--fail-on LEVEL` | `unreliable` | Health is better than LEVEL (`degraded`, `unreliable`, or `never` to turn the check off). |
| `--max-first-audio STAT=DUR` | none | That statistic of time to first audio is at most DUR, such as `p95=800ms`. Repeatable. STAT is `mean`, `p50`, `p90`, `p95`, `p99` or `max`. |
| `--max-stop-latency STAT=DUR` | none | The same, for interruption stop latency. |
| `--min-reply-rate R` | off | At least share R of turns were replied to, from 0 to 1. |
| `--min-stop-rate R` | off | At least share R of interruptions were stopped within `--stop-within`. |

**A call fails** when it can't connect, when a step fails, or when it runs past `--call-timeout` (default 5 minutes). A step fails when:
- the agent didn't start speaking within `reply_timeout`;
- a reply ran past `max_reply`;
- the agent finished before an interrupt's `after` point;
- the agent hung up.

A failed call's completed steps still count in the metrics.

## Known limitations

**Timing only.** callswarm measures when the agent speaks, not what it says. It doesn't transcribe replies, judge their content, or see tool calls. An agent that answers quickly with the wrong answer passes every check.

**The voice detector hears sound, not speech:**
- Music, noise, or a beep played by the agent counts as speech.
- Agent audio quieter than -45 dBFS is missed.
- A steady sound that lasts longer than 2 seconds, such as a test tone, fades into the background estimate.

It works well on what callswarm deals with: recorded caller speech and synthesized agent speech.

**`reply_gap` is a guess about the agent.** An agent that pauses longer than `reply_gap` within one reply looks as if it finished, and the next step talks over the rest of it. That shows up as an extra turn or a missed reply. See [quickstart.md](quickstart.md#write-a-scenario).

**The line is idealized.**
- Twilio: callswarm connects straight to the agent's WebSocket, so a real phone call's extra delays aren't included. Those are the telephone network, Twilio's media servers and the phone's jitter buffer.
- LiveKit: the audio does pass through a LiveKit server, adding about 25 ms locally, more over the internet.

Treat callswarm's numbers as the agent's own latency, a lower bound on what a caller would experience.

**Caller audio is telephone quality.** Clips are sent as 8 kHz audio on every transport. That's realistic for phone agents, and slightly pessimistic for speech recognition in agents that normally get wideband microphone audio.

**Scripted callers don't listen.** Each caller plays the same clips in the same order whatever the agent says. Interruptions are timed from when the agent's speech starts, not from what it is saying.

**The test machine is part of the measurement.** Health flags an overloaded run but can't remove the overload from its numbers. For tail latency you intend to publish, run on a machine that stays healthy at your concurrency, and report the health with the numbers.

**LiveKit support has only been tested against the built-in fake agent.** It hasn't yet been run against a LiveKit Agents worker or LiveKit Cloud; see [livekit.md](livekit.md#limits).

**Not included:**
- phone network (SIP or PSTN) testing;
- synthetic caller voices;
- judging conversation quality with an LLM;
- WebRTC transports other than LiveKit;
- a hosted service.
