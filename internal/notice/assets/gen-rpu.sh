#!/bin/sh
# Regenerates rpu_p5.bin and rpu_p81.bin: one static Dolby Vision RPU NAL each (header 7C 01,
# emulation prevention included), without start code. dovi_tool builds the NAL: the RPU is
# injected into a one-frame HEVC stream and the type-62 NAL is extracted from the result.
# Dev machine only: needs ffmpeg and dovi_tool (prebuilt: github.com/quietvoid/dovi_tool releases).
set -e
cd "$(dirname "$0")"
ffmpeg -y -v error -f lavfi -i color=black:size=64x64:rate=24 -frames:v 1 -c:v libx265 -pix_fmt yuv420p10le \
  -x265-params log-level=error -f hevc bl.hevc
for P in 5 8.1; do
  N=$(echo $P | tr -d .)
  printf '{"cm_version":"V40","profile":"%s","length":1,"level6":{"max_display_mastering_luminance":1000,"min_display_mastering_luminance":1,"max_content_light_level":1000,"max_frame_average_light_level":400}}' "$P" > p$N.json
  dovi_tool generate -j p$N.json -o p$N.rpu >/dev/null
  dovi_tool inject-rpu -i bl.hevc --rpu-in p$N.rpu -o dv$N.hevc >/dev/null
  python3 - dv$N.hevc rpu_p$N.bin <<'PY'
import re, sys
b = open(sys.argv[1], 'rb').read()
starts = [m.end() for m in re.finditer(b'\x00\x00\x01', b)]
nals = [b[s:e - (4 if b[e-4:e] == b'\x00\x00\x00\x01' else 3)] if e else b[s:] for s, e in zip(starts, starts[1:] + [0])]
rpu = [n for n in nals if n and (n[0] >> 1) & 0x3F == 62]
assert len(rpu) == 1, "expected one RPU NAL, found %d" % len(rpu)
open(sys.argv[2], 'wb').write(rpu[0])
print(sys.argv[2], len(rpu[0]), "bytes", rpu[0][:4].hex())
PY
  dovi_tool extract-rpu -i dv$N.hevc -o x$N.rpu >/dev/null
  dovi_tool info -i x$N.rpu --frame 0 | grep -E '"(dovi_profile|guessed_profile)"'
  rm -f p$N.json p$N.rpu dv$N.hevc x$N.rpu
done
rm -f bl.hevc
