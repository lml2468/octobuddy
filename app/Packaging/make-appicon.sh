#!/bin/zsh
# Generates app/Packaging/AppIcon.icns from a programmatic master image:
# a graphite rounded-rect with a blue chat-bubbles glyph (XClaw mark).
# Re-run if the mark changes. Requires macOS (AppKit + sips + iconutil).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d)"
master="$work/icon_1024.png"

swift - "$master" <<'SWIFT'
import AppKit

let size: CGFloat = 1024
let outPath = CommandLine.arguments[1]

func tintedSymbol(_ name: String, pt: CGFloat, color: NSColor) -> NSImage? {
    let cfg = NSImage.SymbolConfiguration(pointSize: pt, weight: .semibold)
    guard let base = NSImage(systemSymbolName: name, accessibilityDescription: nil)?
        .withSymbolConfiguration(cfg) else { return nil }
    let s = base.size
    let out = NSImage(size: s)
    out.lockFocus()
    base.draw(in: NSRect(origin: .zero, size: s))
    color.set()
    NSRect(origin: .zero, size: s).fill(using: .sourceAtop)
    out.unlockFocus()
    return out
}

let img = NSImage(size: NSSize(width: size, height: size))
img.lockFocus()
let rect = NSRect(x: 0, y: 0, width: size, height: size)
// macOS masks app icons to a squircle; a rounded rect reads well at all sizes.
let bg = NSBezierPath(roundedRect: rect, xRadius: size * 0.225, yRadius: size * 0.225)
let grad = NSGradient(starting: NSColor(srgbRed: 0.20, green: 0.22, blue: 0.27, alpha: 1),
                      ending: NSColor(srgbRed: 0.10, green: 0.11, blue: 0.14, alpha: 1))!
grad.draw(in: bg, angle: -90)

if let glyph = tintedSymbol("bubble.left.and.bubble.right.fill",
                            pt: size * 0.5,
                            color: NSColor(srgbRed: 0.30, green: 0.66, blue: 1.0, alpha: 1)) {
    let gs = glyph.size
    let origin = NSPoint(x: (size - gs.width) / 2, y: (size - gs.height) / 2)
    glyph.draw(in: NSRect(origin: origin, size: gs), from: .zero, operation: .sourceOver, fraction: 1)
}
img.unlockFocus()

guard let tiff = img.tiffRepresentation,
      let rep = NSBitmapImageRep(data: tiff),
      let png = rep.representation(using: .png, properties: [:]) else {
    FileHandle.standardError.write(Data("render failed\n".utf8)); exit(1)
}
try! png.write(to: URL(fileURLWithPath: outPath))
SWIFT

# Build the .iconset at all required sizes, then pack into .icns.
iconset="$work/AppIcon.iconset"
mkdir -p "$iconset"
for s in 16 32 128 256 512; do
  sips -z $s $s "$master" --out "$iconset/icon_${s}x${s}.png" >/dev/null
  d=$((s*2)); sips -z $d $d "$master" --out "$iconset/icon_${s}x${s}@2x.png" >/dev/null
done
iconutil -c icns "$iconset" -o "$here/AppIcon.icns"
echo "wrote $here/AppIcon.icns"
rm -rf "$work"
