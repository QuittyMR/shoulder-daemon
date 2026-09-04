#!/usr/bin/env python3
"""Generate docs/assets/social-preview.png: the hero with the tagline under its title.

1280x640 is what GitHub's social preview and LinkedIn's link card both take
uncropped. The hero already carries the title in gold monospace at the right;
the tagline is set beneath it in the same face and a slightly dimmer gold.
"""

from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / "docs/assets/hero.webp"
OUT = ROOT / "docs/assets/social-preview.png"
FONT = "/usr/share/fonts/liberation-mono-fonts/LiberationMono-Regular.ttf"

W, H = 1280, 640
TAGLINE = ("Context advisor", "for coding agents.")
GOLD = (168, 147, 100)
SIZE = 30
LINE = 40

im = Image.open(SRC).convert("RGB").resize((W, H), Image.LANCZOS)
draw = ImageDraw.Draw(im)
font = ImageFont.truetype(FONT, SIZE)

# The title's left edge and the bottom of its second line, measured on the
# source and scaled: it sits at x=1162, y=388..529 of 1774x887.
scale = W / 1774
x = round(1162 * scale)
y = round(529 * scale) + 34
for line in TAGLINE:
    draw.text((x, y), line, font=font, fill=GOLD)
    y += LINE

im.save(OUT, optimize=True)
print(OUT, im.size)
