"""Generates the caller's clips for the benchmark scenario with Gemini TTS.

Run once from this directory with GEMINI_API_KEY set:

    python gen_clips.py

Writes one 16-bit mono WAV per line in CLIPS. Commit the WAVs: the benchmark
should replay the same audio every time, and its scenario digest changes if
they are regenerated. Uses only the standard library.
"""

import argparse
import base64
import json
import os
import struct
import sys
import urllib.error
import urllib.request
import wave

ENDPOINT = "https://generativelanguage.googleapis.com/v1beta/interactions"
RATE = 24000  # Gemini TTS output when no other rate is requested

CLIPS = {
    "hours.wav": "Hi, are you open on Saturday?",
    "cakes.wav": "Can you tell me about the cakes you make for birthdays?",
    "interrupt.wav": "Sorry, actually, do you deliver?",
    "bye.wav": "Okay, thank you. Goodbye!",
}


def synthesize(text, model, voice, key):
    body = {
        "model": model,
        "input": [{"type": "user_input", "content": [{"type": "text", "text": text}]}],
        "response_format": {"type": "audio"},
        "generation_config": {"speech_config": [{"voice": voice}]},
    }
    req = urllib.request.Request(
        ENDPOINT,
        data=json.dumps(body).encode(),
        headers={"x-goog-api-key": key, "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            reply = json.load(resp)
    except urllib.error.HTTPError as e:
        sys.exit(f"Gemini TTS returned {e.code}: {e.read().decode(errors='replace')}")
    audio = [
        c["data"]
        for step in reply.get("steps", [])
        if step.get("type") == "model_output"
        for c in step.get("content", [])
        if c.get("type") == "audio"
    ]
    if not audio:
        sys.exit(f"no audio in the response: {json.dumps(reply)[:500]}")
    return base64.b64decode(audio[-1])


def pcm_of(data):
    """Returns (pcm, rate, channels) from a WAV, or raw 24 kHz mono PCM."""
    if data[:4] != b"RIFF" or data[8:12] != b"WAVE":
        return data, RATE, 1
    rate, channels, bits, pcm = RATE, 1, 16, b""
    pos = 12
    while pos + 8 <= len(data):
        cid, size = data[pos : pos + 4], struct.unpack("<I", data[pos + 4 : pos + 8])[0]
        body = data[pos + 8 : pos + 8 + size]  # a streaming header's size overruns; take the rest
        if cid == b"fmt ":
            _, channels, rate, _, _, bits = struct.unpack("<HHIIHH", body[:16])
        elif cid == b"data":
            pcm = body
            break
        pos += 8 + size + (size & 1)
    if bits != 16:
        sys.exit(f"expected 16-bit audio, got {bits}-bit")
    return pcm, rate, channels


def main():
    parser = argparse.ArgumentParser(description="Generate benchmark caller clips with Gemini TTS.")
    parser.add_argument("--model", default="gemini-3.8-flash-tts")
    parser.add_argument("--voice", default="Kore")
    args = parser.parse_args()
    key = os.getenv("GEMINI_API_KEY")
    if not key:
        sys.exit("GEMINI_API_KEY is not set")

    for name, text in CLIPS.items():
        pcm, rate, channels = pcm_of(synthesize(text, args.model, args.voice, key))
        pcm = pcm[: len(pcm) - len(pcm) % (2 * channels)]
        with wave.open(name, "wb") as w:
            w.setnchannels(channels)
            w.setsampwidth(2)
            w.setframerate(rate)
            w.writeframes(pcm)
        print(f"{name}: {len(pcm) / (2 * channels * rate):.2f}s at {rate} Hz  \"{text}\"")


if __name__ == "__main__":
    main()