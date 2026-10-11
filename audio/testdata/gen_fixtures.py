"""Regenerate the audio test fixtures.

The fixtures are produced by independent implementations (Python's wave module
and CPython's audioop G.711 codec) so the Go code is checked against something
other than itself.

    pip install audioop-lts   # Python 3.13+ only; audioop is built in before that
    python3 gen_fixtures.py
"""

import math
import struct
import wave

try:
    import audioop
except ImportError as e:  # pragma: no cover
    raise SystemExit("install audioop-lts on Python 3.13+") from e


def write_wav(path, rate, channels, frames):
    with wave.open(path, "wb") as w:
        w.setnchannels(channels)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(frames)


# 0.5 s, 440 Hz sine at amplitude 10000, 16 kHz mono.
rate = 16000
samples = [round(10000 * math.sin(2 * math.pi * 440 * n / rate)) for n in range(rate // 2)]
write_wav("tone_16k_mono.wav", rate, 1, struct.pack(f"<{len(samples)}h", *samples))

# 100 stereo frames at 44.1 kHz: left = 1000, right = -3000 (mono average -1000).
write_wav("stereo_44k.wav", 44100, 2, struct.pack("<hh", 1000, -3000) * 100)

# μ-law encode reference: every int16 value from -32768 to 32767, in order.
all_samples = struct.pack("<65536h", *range(-32768, 32768))
with open("mulaw_encode_ref.bin", "wb") as f:
    f.write(audioop.lin2ulaw(all_samples, 2))

# μ-law decode reference: bytes 0..255 decoded to little-endian int16.
with open("mulaw_decode_ref.bin", "wb") as f:
    f.write(audioop.ulaw2lin(bytes(range(256)), 2))