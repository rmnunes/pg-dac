"""Swap the small-size variant into the generated icon set.

`pnpm tauri icon` scales one source to every size; at 16-32 px the full artwork
blurs, so those sizes come from a simplified source instead. Run from ui/ after
`pnpm tauri icon src-tauri/icons/source/pg-dac.png` (`pnpm icons` does both).
Needs Pillow.
"""
from pathlib import Path

from PIL import Image

ICONS = Path(__file__).resolve().parent.parent / "src-tauri" / "icons"
SMALL_MAX = 32

main = Image.open(ICONS / "source" / "pg-dac.png").convert("RGBA")
small = Image.open(ICONS / "source" / "pg-dac-small.png").convert("RGBA")


def at(size):
    return (small if size <= SMALL_MAX else main).resize((size, size), Image.LANCZOS)


sizes = sorted(Image.open(ICONS / "icon.ico").info["sizes"])
frames = [at(w) for (w, _) in sizes]
frames[-1].save(ICONS / "icon.ico", format="ICO", sizes=sizes, append_images=frames[:-1])
at(32).save(ICONS / "32x32.png")
at(30).save(ICONS / "Square30x30Logo.png")
print("small sizes replaced:", [w for (w, _) in sizes if w <= SMALL_MAX], "+ 32x32.png, Square30x30Logo.png")
