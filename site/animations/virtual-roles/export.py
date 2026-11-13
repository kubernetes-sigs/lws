# Copyright 2026 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Optimize the rendered MP4, extract its poster, and decode-check every frame."""
import argparse
from pathlib import Path
import subprocess

import av
import imageio_ffmpeg

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("movie", type=Path)
args = parser.parse_args()
site = Path(__file__).resolve().parents[2]
video = site / "static/videos/disaggregatedset-virtual-roles.mp4"
poster = site / "static/images/disaggregatedset-virtual-roles-poster.png"
video.parent.mkdir(parents=True, exist_ok=True)
poster.parent.mkdir(parents=True, exist_ok=True)
subprocess.run([
    imageio_ffmpeg.get_ffmpeg_exe(), "-y", "-i", str(args.movie),
    "-c:v", "libx264", "-crf", "20", "-preset", "slow", "-pix_fmt", "yuv420p",
    "-an", "-movflags", "+faststart", str(video),
], check=True)
with av.open(str(video)) as container:
    stream = container.streams.video[0]
    assert stream.codec_context.name == "h264"
    assert stream.codec_context.format.name == "yuv420p"
    frames = 0
    for frame in container.decode(video=0):
        if frames == 0:
            frame.to_image().save(poster)
        frames += 1
    assert frames > 1500, "Incomplete render"
    print(f"Validated {frames} frames, {stream.width}x{stream.height}, {stream.average_rate} fps")
    print(f"Video: {video} ({video.stat().st_size:,} bytes)")
