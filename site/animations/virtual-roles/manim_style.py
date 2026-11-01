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

"""Local typography defaults; no dependency on another film project."""
from manim import MarkupText, Text


def apply_text_defaults():
    missing = {"Arial", "Menlo"} - set(Text.font_list())
    if missing:
        raise RuntimeError(f"Required fonts are missing: {sorted(missing)}")
    Text.set_default(font="Arial", disable_ligatures=True)
    MarkupText.set_default(font="Arial", disable_ligatures=True)


def readable_text(value, *, font_size=36, **style):
    style.setdefault("font", "Arial")
    style.setdefault("disable_ligatures", True)
    return Text(str(value), font_size=font_size * 4, **style).scale(.25)
