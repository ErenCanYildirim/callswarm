# Testing a Pipecat agent

Pipecat agents that take phone calls through Twilio speak the Twilio Media Streams protocol over a WebSocket, so callswarm can call them directly with no Twilio account and no phone number. This page covers what to change in the bot and what callswarm will and won't see. It was checked against Pipecat 1.12.0.

[`bench/pipecat/bot.py`](../bench/pipecat/bot.py) is a complete example.

## Point callswarm at the bot

Twilio connects to whatever URL your TwiML `<Stream>` names, usually `wss://<host>/ws`. Run the bot locally and use the same path:

```
callswarm run scenario.yaml --url ws://127.0.0.1:7860/ws
```

Values passed with `--param key=value` arrive the way Twilio's `<Parameter>` elements do: `parse_telephony_websocket` puts them in `call_data.body`.

## Turn off auto hang-up

`TwilioFrameSerializer` hangs up through Twilio's REST API when the pipeline ends, and that is on by default. With it on, the serializer needs `account_sid` and `auth_token`, and constructing it without them raises:

```
ValueError: auto_hang_up is enabled but missing required parameters: account_sid, auth_token
```

callswarm is not a Twilio call, so there is nothing to hang up. Build the serializer yourself with hang-up off:

```python
serializer = TwilioFrameSerializer(
    stream_sid=call_data.stream_id,
    call_sid=call_data.call_id,
    params=TwilioFrameSerializer.InputParams(auto_hang_up=False),
)
```

If your bot uses Pipecat's development runner (`create_transport`), the runner builds the serializer for you from the `TWILIO_ACCOUNT_SID` and `TWILIO_AUTH_TOKEN` environment variables with hang-up on, so it fails the same way when they are unset. Either build the transport yourself, as above, or set the two variables. With placeholder values the bot runs, and the hang-up attempt at the end of each call fails and is logged as an error.

## End the pipeline when the caller hangs up

Pipecat ignores Twilio's `stop` message; the pipeline only learns the call is over when the WebSocket closes. Make sure the bot cancels its pipeline then:

```python
@transport.event_handler("on_client_disconnected")
async def on_disconnected(_transport, _websocket):
    await worker.cancel()
```

Without this, each finished call's pipeline keeps running until the worker's idle timeout (300 s by default). One test call won't notice; a load test of a few hundred calls will pile up pipelines and their connections to your speech providers, and the latency you measure will include that.

## What callswarm sees

- **No marks.** Pipecat neither sends `mark` messages nor reads the ones callswarm echoes back, so reports from a Pipecat bot have no `agent_mark` events. This is expected. callswarm finds the agent's speech from the audio itself.
- **Audio paced at real time, in 40 ms chunks.** The transport sleeps between chunks to emulate a sound card, so audio arrives about as fast as it plays, in chunks twice Twilio's 20 ms. callswarm queues whatever arrives and times it by when it would play, the way Twilio does, so time to first audio and interruption timing are measured from playback, not from arrival.
- **Barge-in by `clear`.** When the bot detects the caller interrupting, it sends `clear`, and callswarm discards the queued audio. The report's interruption section counts the clears alongside how quickly the audio stopped. Pipecat also sends `clear` at the start of every caller turn, even when the bot is silent, so a timeline shows `agent_clear` events where nothing was interrupted. They change nothing in the measurements.
- **Detection of the call.** `parse_telephony_websocket` reads the first two messages and recognizes Twilio by a `start` message carrying `streamSid` and `callSid`, both of which callswarm sends. It has no timeout, so a client that connects and sends nothing holds a server task open.