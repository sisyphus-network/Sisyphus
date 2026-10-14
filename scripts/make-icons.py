#!/usr/bin/env python3
"""Draw the application icons from assets/logo.png.

The logo is a glowing figure on nothing, which disappears on a light
taskbar, so each icon sets it on a dark rounded plate the colour of the
app's window. Run this again whenever the logo changes, and commit what it
writes. It needs Pillow: pip install pillow.

  apps/sisyphus/build/icon.icns     macOS: the app, the Dock, the disk image
  apps/sisyphus/build/icon.ico      Windows: the app, the installer, shortcuts
  apps/sisyphus/build/icons/*.png   Linux: the menu entry, in each size
  apps/sisyphusd/winres/icon.png    Windows: the daemon's own executable
"""
from pathlib import Path

from PIL import Image, ImageDraw

ROOT = Path(__file__).resolve().parent.parent
PLATE = (16, 20, 26, 255)  # the window's background, #10141a
BIG = 1024


def plate(inset: float, radius: float, padding: float) -> Image.Image:
    """The logo on a rounded square. inset is the gap between the square
    and the edge of the picture, radius its corners and padding the gap
    between the square's edge and the logo, each as a share of the picture."""
    logo = Image.open(ROOT / "assets/logo.png").convert("RGBA")
    logo = logo.crop(logo.getbbox())
    # Draw at four times the size and shrink, for smooth corners.
    size = BIG * 4
    mask = Image.new("L", (size, size), 0)
    edge = round(size * inset)
    ImageDraw.Draw(mask).rounded_rectangle((edge, edge, size - edge - 1, size - edge - 1), round(size * radius), fill=255)
    icon = Image.new("RGBA", (BIG, BIG), (0, 0, 0, 0))
    icon.paste(Image.new("RGBA", (BIG, BIG), PLATE), mask=mask.resize((BIG, BIG), Image.LANCZOS))
    room = BIG * (1 - 2 * inset - 2 * padding)
    scale = room / max(logo.size)
    logo = logo.resize((round(logo.width * scale), round(logo.height * scale)), Image.LANCZOS)
    icon.alpha_composite(logo, ((BIG - logo.width) // 2, (BIG - logo.height) // 2))
    return icon


def main() -> None:
    desktop = ROOT / "apps/sisyphus/build"
    (desktop / "icons").mkdir(parents=True, exist_ok=True)
    # macOS draws no plate of its own: its icons leave a margin around a
    # square with wide corners, so that they sit level with the others.
    mac = plate(inset=0.098, radius=0.18, padding=0.07)
    mac.save(desktop / "icon.icns", sizes=[(n, n) for n in (16, 32, 64, 128, 256, 512, 1024)])
    # Windows and Linux icons fill the picture.
    full = plate(inset=0.0, radius=0.2, padding=0.06)
    full.save(desktop / "icon.ico", sizes=[(n, n) for n in (16, 24, 32, 48, 64, 128, 256)])
    for n in (16, 32, 48, 64, 128, 256, 512):
        full.resize((n, n), Image.LANCZOS).save(desktop / "icons" / f"{n}x{n}.png", optimize=True)
    daemon = ROOT / "apps/sisyphusd/winres"
    daemon.mkdir(parents=True, exist_ok=True)
    full.resize((256, 256), Image.LANCZOS).save(daemon / "icon.png", optimize=True)


if __name__ == "__main__":
    main()
