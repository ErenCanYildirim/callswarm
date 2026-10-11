"""Writes tone.wav: six 250 ms tone bursts with 80 ms gaps at 8 kHz.

Run from this directory with: python gen_tone.py
"""

import math
import struct
import wave

RATE = 8000
FADE = int(0.01 * RATE)  # 10 ms fade in and out, so bursts don't click


def burst(ms, freq):
    n = int(RATE * ms / 1000)
    return [
        int(8000 * min(1, i / FADE, (n - 1 - i) / FADE) * math.sin(2 * math.pi * freq * i / RATE))
        for i in range(n)
    ]


samples = []
for k, freq in enumerate([440, 520, 470, 560, 500, 440]):
    if k:
        samples += [0] * int(RATE * 0.08)
    samples += burst(250, freq)

with wave.open("tone.wav", "wb") as w:
    w.setnchannels(1)
    w.setsampwidth(2)
    w.setframerate(RATE)
    w.writeframes(b"".join(struct.pack("<h", s) for s in samples))