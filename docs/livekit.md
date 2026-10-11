# LiveKit

callswarm can call an agent through a LiveKit room instead of a Twilio Media Streams WebSocket. Each call creates its own room, joins it as an ordinary participant, publishes the caller's audio as a microphone track and listens to the agent's audio track. Scenarios, metrics, checks and reports work the same as over Twilio.

```
export LIVEKIT_URL=wss://your-project.livekit.cloud
export LIVEKIT_API_KEY=...
export LIVEKIT_API_SECRET=...
callswarm run scenario.yaml --transport livekit --calls 20 --concurrency 5 --out report.json
```

`--url` overrides `LIVEKIT_URL`, and `--api-key` and `--api-secret` override the key variables. Prefer the environment for the secret, since command lines show up in process lists and CI logs.

## Getting the agent into the room

Each call creates a room named `<prefix><run id>-<call number>`, such as `callswarm-3fa9c2d1-7`. The prefix is `callswarm-` by default and set with `--room-prefix`.

- **Automatic dispatch:** an agent worker without an agent name is sent to every new room, so it joins on its own. Nothing else is needed.
- **Explicit dispatch:** an agent that registers with an agent name only joins rooms it is dispatched to. Pass `--agent-name <name>`, and callswarm puts the dispatch in each caller's access token. `--agent-metadata` is passed to the agent's job.

`--param key=value` sets attributes on the caller's participant, which an agent can read the way it would read a SIP caller's attributes.

A call starts only when the agent has answered: when its audio track has arrived and it has subscribed to the caller's. Until then the caller would be talking to an empty room. `--answer-timeout` (default 30s) bounds the wait, and a call whose agent never shows up fails with "no agent answered". The wait counts toward `--call-timeout`, not toward any latency.

The first audio track another participant publishes is taken to be the agent's. Don't run callswarm in rooms where other participants publish audio.

## How measurements differ from Twilio

- **Caller audio is narrowband.** Scenario clips are 8 kHz, as for every transport, and are sent as 8 kHz Opus. That is fine for timing, but an agent's speech recognition will do slightly worse than with a wideband microphone, roughly as it does on a phone call.
- **No clear or marks.** WebRTC has neither, so an agent stops talking by simply stopping sending. Interruption stop latency is measured the same way, but the report has no "sent clear" count.
- **Agent audio plays as it arrives.** Received audio is taken to start playing on arrival, queued behind audio still playing, as a WebRTC client's jitter buffer roughly does. Over Twilio, agents often send audio faster than real time, so this matters less there.
- **One more network hop.** Audio goes through the LiveKit server and Opus in both directions. Against the built-in fake agent on a local server, that adds about 25 ms to time to first audio (525 ms measured for an agent that answers in exactly 500 ms) and about 5 ms to interruption stop latency. Over LiveKit Cloud, add the round trip to the nearest region.

Noise cancellation in the agent, such as LiveKit's enhanced noise cancellation, can treat test tones as noise and remove them. Use recorded speech in scenarios meant for real agents.

## Trying it locally

Run a LiveKit server in development mode, which uses the key `devkey` and secret `secret`:

```
docker run --rm -p 7880:7880 -p 7881:7881 -p 7882:7882/udp livekit/livekit-server:v1.13.9 --dev --bind 0.0.0.0 --node-ip 127.0.0.1
```

Then, in a second terminal, start the fake agent, which joins every new room whose name starts with `callswarm-`:

```
export LIVEKIT_URL=ws://127.0.0.1:7880 LIVEKIT_API_KEY=devkey LIVEKIT_API_SECRET=secret
callswarm fake-agent --transport livekit
```

And in a third, with the same variables set, run a scenario:

```
callswarm run examples/smoke/scenario.yaml --transport livekit --calls 4 --concurrency 2
```

The fake agent answers 500 ms after the caller stops and stops within about 60 ms when interrupted, so the report should show about 525 ms and 65 ms.

## Limits

- Tested against the built-in fake agent on a local LiveKit server, not yet against a LiveKit Agents worker or LiveKit Cloud. Reports of either are welcome.
- Every call is a real participant session. On LiveKit Cloud, load runs use participant minutes like any other traffic.
- Rooms are left, not deleted, when a call ends; the server closes them once they are empty.
