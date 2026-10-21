#!/usr/bin/env python3
"""Draw the application icons from assets/app-icon.png.

The app icon is a glowing network globe on transparency. Keep that
transparency in every desktop and tray variant. Run this again whenever the
artwork changes, and commit what it writes. It needs Pillow: pip install pillow.

  apps/sisyphus/build/icon.icns     macOS: the app, the Dock, the disk image
  apps/sisyphus/build/icon.ico      Windows: the app, the installer, shortcuts
  apps/sisyphus/build/icons/*.png   Linux: the menu entry, in each size
  apps/sisyphusd/winres/icon.png    Windows: the daemon's own executable
  apps/sisyphus/build/tray/*.png    the tray icon in each node state
"""
from pathlib import Path

from PIL import Image, ImageDraw

ROOT = Path(__file__).resolve().parent.parent
SOURCE = ROOT / "assets/app-icon.png"
BIG = 1024


def app_icon(padding: float) -> Image.Image:
    """Center the cropped artwork on a transparent square with a small margin."""
    artwork = Image.open(SOURCE).convert("RGBA")
    # Ignore faint edge pixels in artwork exported against transparency; a
    # single alpha value across the canvas would otherwise defeat cropping.
    bounds = artwork.getchannel("A").point(lambda value: 255 if value > 8 else 0).getbbox()
    if bounds is None:
        raise ValueError(f"{SOURCE} has no visible artwork")
    artwork = artwork.crop(bounds)
    icon = Image.new("RGBA", (BIG, BIG), (0, 0, 0, 0))
    room = BIG * (1 - 2 * padding)
    scale = room / max(artwork.size)
    artwork = artwork.resize((round(artwork.width * scale), round(artwork.height * scale)), Image.LANCZOS)
    icon.alpha_composite(artwork, ((BIG - artwork.width) // 2, (BIG - artwork.height) // 2))
    return icon


def main() -> None:
    desktop = ROOT / "apps/sisyphus/build"
    (desktop / "icons").mkdir(parents=True, exist_ok=True)
    # Leave a little more transparent breathing room around the macOS icon.
    mac = app_icon(padding=0.08)
    mac.save(desktop / "icon.icns", sizes=[(n, n) for n in (16, 32, 64, 128, 256, 512, 1024)])
    full = app_icon(padding=0.04)
    full.save(desktop / "icon.ico", sizes=[(n, n) for n in (16, 24, 32, 48, 64, 128, 256)])
    for n in (16, 32, 48, 64, 128, 256, 512):
        full.resize((n, n), Image.LANCZOS).save(desktop / "icons" / f"{n}x{n}.png", optimize=True)
    daemon = ROOT / "apps/sisyphusd/winres"
    daemon.mkdir(parents=True, exist_ok=True)
    full.resize((256, 256), Image.LANCZOS).save(daemon / "icon.png", optimize=True)
    tray_icons(full, desktop / "tray")


def mark(draw: ImageDraw.ImageDraw, state: str, unit: float, ink) -> None:
    """The badge's mark, in the bottom-right corner of a picture 16 units
    wide: a hollow ring while the node connects, a slash while it is offline.
    Its shape says the state, so it reads without colour."""
    cx = cy = 11.5 * unit
    width = round(1.3 * unit)
    if state == "connecting":
        r = 2.9 * unit
        draw.ellipse((cx - r, cy - r, cx + r, cy + r), outline=ink, width=width)
    elif state == "offline":
        d = 2.3 * unit
        draw.line((cx - d, cy + d, cx + d, cy - d), fill=ink, width=width)
        for x, y in ((cx - d, cy + d), (cx + d, cy - d)):  # round ends
            draw.ellipse((x - width / 2, y - width / 2, x + width / 2, y + width / 2), fill=ink)


def tray_icons(full: Image.Image, folder: Path) -> None:
    """The tray icon in each node state, at 16 pixels and twice that for
    sharper screens. Windows and Linux retain the artwork's transparency;
    the state mark clears only its own small area. macOS tints a monochrome
    template to suit the menu bar, preserving the globe's bright network lines."""
    folder.mkdir(parents=True, exist_ok=True)
    for state in ("connected", "connecting", "offline"):
        for n, suffix in ((16, ""), (32, "@2x")):
            big = n * 4  # draw large and shrink, for smooth edges
            unit = big / 16
            colour = full.resize((big, big), Image.LANCZOS)
            draw = ImageDraw.Draw(colour)
            if state != "connected":
                r = 4.5 * unit
                draw.ellipse((11.5 * unit - r, 11.5 * unit - r, 11.5 * unit + r, 11.5 * unit + r), fill=(0, 0, 0, 0))
                mark(draw, state, unit, (242, 244, 248, 255))
            colour.resize((n, n), Image.LANCZOS).save(folder / f"{state}{suffix}.png", optimize=True)

            # Keep the orb's network lines visible in macOS's monochrome
            # template while dropping its dark fill and blue glow. Bright
            # details become the template alpha that the menu bar tints.
            gray = colour.convert("RGB").convert("L")
            shape = gray.point(lambda value: 255 if value >= 72 else 0)
            draw = ImageDraw.Draw(shape)
            if state != "connected":
                r = 5 * unit
                draw.ellipse((11.5 * unit - r, 11.5 * unit - r, 11.5 * unit + r, 11.5 * unit + r), fill=0)
                mark(draw, state, unit, 255)
            template = Image.new("RGBA", (big, big), (0, 0, 0, 0))
            template.putalpha(shape)
            template.resize((n, n), Image.LANCZOS).save(folder / f"{state}Template{suffix}.png", optimize=True)


if __name__ == "__main__":
    main()
