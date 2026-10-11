"""Pipecat phone agent for the callswarm benchmark.

Speaks the Twilio Media Streams protocol on ws://HOST:PORT/ws, as Twilio
would connect to it, and runs Deepgram speech-to-text, Gemini and ElevenLabs
text-to-speech with Silero VAD and Smart Turn v3 for turn taking. The Jargo
bot in ../jargo is built to match it setting for setting.

Run:  python bot.py [--host 127.0.0.1] [--port 7860]
Needs DEEPGRAM_API_KEY, GEMINI_API_KEY and ELEVENLABS_API_KEY.
"""

import argparse
import os
import sys

import uvicorn
from fastapi import FastAPI, WebSocket
from loguru import logger

from pipecat.audio.vad.silero import SileroVADAnalyzer
from pipecat.frames.frames import TTSSpeakFrame
from pipecat.pipeline.pipeline import Pipeline
from pipecat.pipeline.worker import PipelineParams, PipelineWorker
from pipecat.processors.aggregators.llm_context import LLMContext
from pipecat.processors.aggregators.llm_response_universal import (
    LLMContextAggregatorPair,
    LLMUserAggregatorParams,
)
from pipecat.runner.utils import parse_telephony_websocket
from pipecat.serializers.twilio import TwilioFrameSerializer
from pipecat.services.deepgram.stt import DeepgramSTTService
from pipecat.services.elevenlabs.tts import ElevenLabsTTSService
from pipecat.services.google.llm import GoogleLLMService
from pipecat.workers.runner import WorkerRunner
from pipecat.transports.websocket.fastapi import (
    FastAPIWebsocketParams,
    FastAPIWebsocketTransport,
)

# Shared with the Jargo bot. Change both together, or the comparison is off.
SAMPLE_RATE = 16000
STT_MODEL = "nova-3-general"
LLM_MODEL = "gemini-3.6-flash"
TTS_MODEL = "eleven_flash_v2_5"
TTS_VOICE = "21m00Tcm4TlvDq8ikWAM"
MAX_TOKENS = 1024
SYSTEM_PROMPT = (
    "You are a friendly voice assistant for a small bakery on a phone call. "
    "The bakery is open Monday to Saturday from 7am to 6pm and closed on Sunday. "
    "Keep your replies short, warm and conversational: one or two sentences."
)
GREETING = "Hello! Thanks for calling the bakery. How can I help you today?"

app = FastAPI()


def env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        sys.exit(f"{name} is not set")
    return value


@app.websocket("/ws")
async def stream(websocket: WebSocket):
    await websocket.accept()
    # Reads Twilio's "connected" and "start" messages to learn the stream SID.
    _, call_data = await parse_telephony_websocket(websocket)

    serializer = TwilioFrameSerializer(
        stream_sid=call_data.stream_id,
        call_sid=call_data.call_id,
        # Hanging up is Twilio's REST API's job, and a test caller has no
        # Twilio account. Left on, the serializer refuses to start without
        # credentials.
        params=TwilioFrameSerializer.InputParams(auto_hang_up=False),
    )
    transport = FastAPIWebsocketTransport(
        websocket=websocket,
        params=FastAPIWebsocketParams(
            audio_in_enabled=True,
            audio_out_enabled=True,
            add_wav_header=False,
            serializer=serializer,
        ),
    )

    stt = DeepgramSTTService(
        api_key=env("DEEPGRAM_API_KEY"),
        settings=DeepgramSTTService.Settings(model=STT_MODEL),
    )
    llm = GoogleLLMService(
        api_key=env("GEMINI_API_KEY"),
        settings=GoogleLLMService.Settings(
            model=LLM_MODEL, system_instruction=SYSTEM_PROMPT, max_tokens=MAX_TOKENS
        ),
    )
    tts = ElevenLabsTTSService(
        api_key=env("ELEVENLABS_API_KEY"),
        sample_rate=SAMPLE_RATE,
        settings=ElevenLabsTTSService.Settings(model=TTS_MODEL, voice=TTS_VOICE),
    )

    context = LLMContext()
    aggregators = LLMContextAggregatorPair(
        context,
        user_params=LLMUserAggregatorParams(vad_analyzer=SileroVADAnalyzer()),
    )
    pipeline = Pipeline(
        [
            transport.input(),
            stt,
            aggregators.user(),
            llm,
            tts,
            transport.output(),
            aggregators.assistant(),
        ]
    )
    worker = PipelineWorker(
        pipeline,
        params=PipelineParams(audio_in_sample_rate=SAMPLE_RATE, audio_out_sample_rate=SAMPLE_RATE),
    )

    @transport.event_handler("on_client_disconnected")
    async def on_disconnected(_transport, _websocket):
        await worker.cancel()

    await worker.queue_frame(TTSSpeakFrame(GREETING))
    logger.info(f"call {call_data.call_id} started")
    runner = WorkerRunner(handle_sigint=False)
    await runner.add_workers(worker)
    await runner.run()
    logger.info(f"call {call_data.call_id} ended")


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=7860)
    args = parser.parse_args()
    for name in ("DEEPGRAM_API_KEY", "GEMINI_API_KEY", "ELEVENLABS_API_KEY"):
        env(name)
    logger.info(f"pipecat bot listening on ws://{args.host}:{args.port}/ws")
    uvicorn.run(app, host=args.host, port=args.port, log_level="warning")


if __name__ == "__main__":
    main()