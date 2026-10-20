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

"""Illustrated virtual-role operations, not a recording of a live cluster."""
import os

import numpy as np
from manim import (
    Arrow, Dot, FadeIn, FadeOut, Indicate, Line, MoveAlongPath, MoveToTarget,
    RoundedRectangle, Scene, VGroup, VMobject, config, linear,
)

from manim_style import apply_text_defaults, readable_text

apply_text_defaults()
config.frame_width = 20
config.frame_height = 12.5
config.background_color = "#FFFFFF"

INK, MUTED, EDGE = "#202B3A", "#526174", "#DCE3EC"
SHORT, LONG = "#326CE5", "#8047BA"
PALE_SHORT, PALE_LONG = "#F0F5FF", "#F6F0FC"
GREEN, RED, AMBER = "#237A46", "#B02B35", "#875600"


def text(value, x, y, size=32, color=INK, bold=False, width=18.5, code=False):
    result = readable_text(value, font_size=size, color=color,
                           weight="BOLD" if bold else "NORMAL",
                           font="Menlo" if code else "Arial")
    if result.width > width:
        result.scale(width / result.width)
    return result.move_to([x, y, 0])


def box(w, h, x, y, fill="#F4F6F9", stroke=EDGE, radius=.15):
    return RoundedRectangle(width=w, height=h, corner_radius=radius,
                            fill_color=fill, fill_opacity=1,
                            stroke_color=stroke, stroke_width=1.5).move_to([x, y, 0])


def tile(group_id, role, pos):
    color = (SHORT, LONG)[role]
    return VGroup(box(.8, .8, *pos[:2], color, color, .08),
                  text(str(group_id), *pos[:2], size=31, color="#FFFFFF", bold=True))


def slot(role, index):
    return np.array([-7.5 + index * 1.35 + role * 9, -.85, 0])


class TypographyCheck(Scene):
    def construct(self):
        for y, line in zip((2, .5, -1, -2.5), (
            "Pending allowance: 1", "Pods open readiness",
            "maxSurge: 10%   maxUnavailable: 10%", "0-8192   8193-131072",
        )):
            self.add(text(line, 0, y, size=32, code=y == -2.5))


class VirtualRoles(Scene):
    def construct(self):
        self.heading = None
        self.banner = None
        self.counts = None
        self.tiles = {}
        if os.environ.get("VIRTUAL_STILL") in ("pair-shared", "pair-split"):
            self.build_fixed_pair()
            if os.environ["VIRTUAL_STILL"] == "pair-split":
                self.show_split_pair()
            self.check_bounds()
            return
        self.title("Virtual roles: separate pools, shared runtime")
        self.build_parent()
        self.set_groups([0, 1, 2, 3], [4, 5], animate=False)
        self.say("Two virtual roles inside Decode",
                 "Each pool has its own replica target. Both use the same Pod templates.")
        if os.environ.get("VIRTUAL_STILL") == "poster":
            self.check_bounds()
            return
        self.wait(7)

        self.title("One shared llm-d-router can serve both pools")
        self.say("Route by prompt length — with an explicit filter",
                 "The pool names are token ranges; they do not change the model's limits.")
        self.request(0, "3,000 prompt tokens", .8)
        self.request(1, "20,000 prompt tokens", .8)
        self.wait(4)

        self.title("Grow one pool without growing the other")
        self.say("Short: 4 → 5     Long: 2 → 2",
                 "The new group is routable once Ready and assigned. Parent total: 6 → 7.")
        new = tile(6, 0, slot(0, 4))
        new[0].set_fill("#FFFFFF").set_stroke(SHORT, width=3)
        new[1].set_color(SHORT)
        next_counts = self.group_counts(5, 2)
        self.play(FadeIn(new, shift=np.array([0, .25, 0])),
                  FadeOut(self.counts), FadeIn(next_counts), run_time=.7)
        self.counts = next_counts
        self.wait(2)
        self.play(new[0].animate.set_fill(SHORT), new[1].animate.set_color("#FFFFFF"), run_time=.6)
        self.tiles[6] = new
        self.set_groups([0, 1, 2, 3, 6], [4, 5])
        self.wait(5)

        self.title("Rebalance pools without restarting groups")
        self.say("Short: 5 → 3     Long: 2 → 4",
                 "Groups 3 and 6 change their subrole labels. Parent total stays at 7.")
        self.play(Indicate(self.tiles[3], color=LONG), Indicate(self.tiles[6], color=LONG), run_time=1)
        self.set_groups([0, 1, 2], [3, 4, 5, 6])
        self.wait(7)

        self.title("A virtual role can scale to zero")
        self.say("Short: 3 → 0     Long: 4 → 4",
                 "Ordinal keeps groups 0–3: it reassigns 0–2, then removes 4–6.")
        self.wait(3)
        # Routing membership changes before the physical parent shrinks.
        # Keep outgoing Long groups visible until retained assignments exist.
        outgoing = [self.tiles.pop(i) for i in (4, 5, 6)]
        retiring = text("Long groups to retire", -2, -2.36, size=25, color=MUTED)
        animations = [FadeOut(self.runtime_caption), FadeIn(retiring)]
        for i, group in enumerate(outgoing):
            group.generate_target()
            group.target.scale(.65).move_to([1 + i * 1.05, -2.36, 0])
            animations.append(MoveToTarget(group))
        self.play(*animations, run_time=1)
        self.set_groups([], [0, 1, 2, 3])
        self.play(*[group[0].animate.set_fill(MUTED).set_stroke(MUTED) for group in outgoing],
                  run_time=.5)
        self.wait(2)
        self.play(*[FadeOut(group, shift=np.array([0, -.25, 0])) for group in outgoing],
                  FadeOut(retiring), FadeIn(self.runtime_caption), run_time=.8)
        empty = text("No Short capacity", -4.5, -.85, size=29, color=MUTED)
        self.play(FadeIn(empty))
        self.wait(6)

        self.play(*[FadeOut(m) for m in list(self.mobjects)], run_time=.7)
        self.heading = self.banner = None
        self.build_fixed_pair()
        self.wait(7)
        self.show_split_pair()
        self.wait(9)
        self.title("B opens when both required subroles have a Ready endpoint")
        ready = self.pair_status(4.65, -.2, True)
        gate = text("Subrole gate: open", 4.65, -1.82, size=28, color=GREEN, bold=True)
        self.play(FadeOut(self.pair_new_status), FadeIn(ready),
                  FadeOut(self.pair_new_gate), FadeIn(gate), run_time=.5)
        self.say("New requests may now choose B",
                 "Requests already pinned to A keep A across both routers.")
        self.wait(6)

        self.play(*[FadeOut(m) for m in list(self.mobjects)], run_time=.7)
        self.heading = self.banner = None
        self.build_caveat()
        self.wait(7)
        self.say("Here the configuration checks the parent roles only",
                 "B passes because some Decode is Ready — not because Long is Ready.")
        self.wait(6)
        self.title("A long request can still land on revision B")
        arrow = Arrow([0, 3.5, 0], [4.65, 2.45, 0], color=RED,
                      buff=.12, stroke_width=4, tip_length=.22)
        chosen = text("B selected", 3.2, 3.35, size=28, color=RED, bold=True)
        self.play(FadeIn(arrow), FadeIn(chosen))
        self.say("Then the Long-only filter finds no endpoint → HTTP 503",
                 "A could serve the request, but the router does not try another revision.")
        self.wait(8)
        self.title("The remaining gap: different subroles for different requests")
        self.say("A fixed pair works with today's gate; dynamic choice needs more",
                 "The parent-role gate does not choose coverage from the request's prompt length.")
        self.wait(8)
        self.check_bounds()

    def title(self, value):
        next_heading = text(value, 0, 5.48, size=42, bold=True)
        if self.heading is None:
            self.add(next_heading)
        else:
            self.play(FadeOut(self.heading), FadeIn(next_heading), run_time=.35)
        self.heading = next_heading

    def say(self, first, second):
        new = VGroup(box(18.5, 1.65, 0, -4.25, stroke="#F4F6F9"),
                     text(first, 0, -3.92, size=32, bold=True, width=17.7),
                     text(second, 0, -4.6, size=27, width=17.7))
        if self.banner is None:
            self.add(new)
        else:
            self.play(FadeOut(self.banner), FadeIn(new), run_time=.35)
        self.banner = new

    def build_parent(self):
        self.add(box(10, 1.1, 0, 3.85),
                 text("Shared llm-d-router", 0, 3.98, size=34, bold=True),
                 text("Decode profile + prompt-length filter", 0, 3.57, size=22, color=MUTED))
        self.paths = []
        for role, x in enumerate((-4.5, 4.5)):
            points = [[0, 3.28, 0], [0, 2.98, 0], [x, 2.98, 0], [x, 2.08, 0]]
            path = VMobject().set_points_as_corners(points)
            self.paths.append(path)
            self.add(Line(points[1], points[2], color=(SHORT, LONG)[role], stroke_width=2),
                     Arrow(points[2], points[3], color=(SHORT, LONG)[role],
                           buff=0, stroke_width=2, tip_length=.16))
        self.add(Line([0, 3.28, 0], [0, 2.98, 0], color=INK, stroke_width=2))
        self.add(box(18.5, 4.8, 0, -.38, fill="#FFFFFF"),
                 text("Parent role: Decode  ·  one LeaderWorkerSet", 0, 1.58, size=30, bold=True))
        for role, x in enumerate((-4.5, 4.5)):
            self.add(box(8.55, 3, x, -.35, (PALE_SHORT, PALE_LONG)[role],
                         (PALE_SHORT, PALE_LONG)[role]),
                     text(("Short prompts", "Long prompts")[role], x, .78, size=32,
                          color=(SHORT, LONG)[role], bold=True),
                     text(("0-8192", "8193-131072")[role], x, .18, size=25, code=True))
        self.runtime_caption = text("Same image, resources and model settings in both pools", 0, -2.36, size=27)
        self.add(self.runtime_caption,
                 text("One square = one group  ·  Number = Ordinal group index", 0, -2.98,
                      size=24, color=MUTED),
                 text("Illustrated operations  ·  one slice / one revision  ·  healthy Ordinal groups",
                      0, -5.65, size=21, color=MUTED))

    def set_groups(self, short, long, animate=True):
        assert not set(short) & set(long)
        wanted = set(short + long)
        changes = []
        for group_id in set(self.tiles) - wanted:
            changes.append(FadeOut(self.tiles.pop(group_id), shift=np.array([0, -.5, 0])))
        for role, groups in enumerate((short, long)):
            for index, group_id in enumerate(groups):
                pos = slot(role, index)
                current = self.tiles.get(group_id)
                if current is None:
                    current = self.tiles[group_id] = tile(group_id, role, pos)
                    if animate:
                        changes.append(FadeIn(current))
                    else:
                        self.add(current)
                elif animate:
                    # Moving the group preserves its identity; only membership color changes.
                    current.generate_target()
                    current.target.move_to(pos)
                    current.target[0].set_fill((SHORT, LONG)[role]).set_stroke((SHORT, LONG)[role])
                    changes.append(MoveToTarget(current))
        new_counts = self.group_counts(len(short), len(long))
        if self.counts is not None:
            changes.append(FadeOut(self.counts))
        if animate:
            changes.append(FadeIn(new_counts))
            self.play(*changes, run_time=1.6)
        else:
            self.add(new_counts)
        self.counts = new_counts

    def group_counts(self, short, long):
        return VGroup(text(f"Target: {short} groups", -4.5, -1.52, size=26, color=SHORT, bold=True),
                      text(f"Target: {long} groups", 4.5, -1.52, size=26, color=LONG, bold=True))

    def request(self, role, value, hold):
        caption = text(value, 0, 4.8, size=27,
                       color=(SHORT, LONG)[role], bold=True, width=8.2)
        dot = Dot(self.paths[role].get_start(), radius=.095, color=(SHORT, LONG)[role])
        self.play(FadeIn(caption), FadeIn(dot), run_time=.3)
        self.play(MoveAlongPath(dot, self.paths[role]), run_time=1.5, rate_func=linear)
        self.wait(hold)
        self.play(FadeOut(caption), FadeOut(dot), run_time=.3)

    def pair_status(self, x, y, ready):
        return VGroup(box(2.25, .59, x + 2.55, y,
                          "#EDF7F0" if ready else "#FFF0F0", stroke="#FFFFFF"),
                      text("Ready" if ready else "Starting", x + 2.55, y,
                           size=26, bold=True, color=GREEN if ready else RED))

    def build_fixed_pair(self):
        self.title("A fixed serving pair works with the existing gate")
        self.pair_router = VGroup(
            box(10.5, 1.15, 0, 3.98),
            text("One shared llm-d-router", 0, 4.15, size=32, bold=True),
            text("Prefill / batch + Decode / standard profiles", 0, 3.7, size=24),
            Arrow([0, 3.36, 0], [-4.65, 2.41, 0], color=GREEN,
                  buff=.07, stroke_width=2.5, tip_length=.16))
        self.add(self.pair_router)
        for side, x in enumerate((-4.65, 4.65)):
            self.add(box(8.7, 4.8, x, 0, fill="#FFFFFF"),
                     text(f"Revision {'AB'[side]}", x, 1.85, size=35, bold=True))
            for row, name in enumerate(("Prefill / batch", "Decode / standard")):
                ready = side == 0 or row == 0
                y = .8 - row
                status = self.pair_status(x, y, ready)
                self.add(text(name, x - 1.13, y, size=26, width=4.8), status)
                if not ready:
                    self.pair_new_status = status
            gate = text(f"Subrole gate: {'open' if side == 0 else 'closed'}", x, -1.82,
                        size=28, color=GREEN if side == 0 else RED, bold=True)
            self.add(gate)
            if side == 1:
                self.pair_new_gate = gate
        self.add(text("Read the subrole label  ·  requiredRoles: [batch, standard]",
                      0, -2.94, size=25, width=18),
                 text("A fixed pair for every request  ·  names unique across the set  ·  illustrated readiness",
                      0, -5.65, size=21, color=MUTED))
        self.say("Gate on batch + standard, not just prefill + decode",
                 "B stays closed until each required subrole has a Ready serving endpoint.")

    def show_split_pair(self):
        self.title("Or two llm-d-routers: one for each virtual role")
        split = VGroup()
        for x, name, detail, width in (
            (-6.15, "Prefill llm-d-router", "prefill / batch", 6),
            (0, "llm-d coordinator", "forwards revision A", 5),
            (6.15, "Decode llm-d-router", "decode / standard", 6),
        ):
            split.add(box(width, 1.2, x, 3.98),
                      text(name, x, 4.15, size=27, bold=True, width=width - .4),
                      text(detail, x, 3.67, size=23, width=width - .4))
        for start, end in ((-3.13, -2.52), (2.52, 3.13)):
            split.add(Arrow([start, 3.98, 0], [end, 3.98, 0], color=GREEN,
                            buff=0, stroke_width=2.5, tip_length=.13))
        split.add(text("x-llm-d-disagg-revision: A", 0, 2.95, size=24, code=True))
        self.play(FadeOut(self.pair_router), FadeIn(split), run_time=.5)
        self.pair_router = split
        self.say("Install the same subrole gate in both routers",
                 "Each watches both pools. The coordinator carries Prefill's revision into Decode.")

    def build_caveat(self):
        self.title("Different case: choosing Short or Long for each request")
        self.add(box(10.5, 1, 0, 3.98),
                 text("llm-d revision gate", 0, 4.05, size=33, bold=True),
                 text("Runs before the subrole filter", 0, 3.65, size=23, color=MUTED))
        for side, x in enumerate((-4.65, 4.65)):
            self.add(box(8.7, 5.25, x, -.18, fill="#FFFFFF"),
                     text(f"Revision {'AB'[side]}", x, 1.9, size=35, bold=True))
            for row, name in enumerate(("Prefill", "Decode / Short", "Decode / Long")):
                ready = side == 0 or row != 2
                y = .92 - row * 1.0
                self.add(text(name, x - 1.17, y, size=27, width=4.3),
                         box(2.25, .59, x + 2.55, y,
                             "#EDF7F0" if ready else "#FFF0F0", stroke="#FFFFFF"),
                         text("Ready" if ready else "Absent", x + 2.55, y,
                              size=26, bold=True, color=GREEN if ready else RED))
            self.add(text("Parent-role gate: open", x, -2.1, size=28, color=AMBER, bold=True))
        self.say("B has Ready Prefill and Ready Short Decode",
                 "But only A has a Ready Long pool. A long request needs that pool.")
        self.add(text("Current router limitation  ·  source-verified example, not a live trace",
                      0, -5.65, size=21, color=MUTED))

    def check_bounds(self):
        for item in self.mobjects:
            assert item.get_left()[0] >= -9.85 and item.get_right()[0] <= 9.85
            assert item.get_bottom()[1] >= -6.1 and item.get_top()[1] <= 6.1
