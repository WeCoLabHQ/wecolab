import AppKit

/// The app icon's mark for the menu bar: a ring, the triangle, three sites. Drawn in code so it stays
/// crisp at every scale, and a template image (black and clear) so macOS tints it for light and dark
/// menu bars. Dimmed while the node is stopped.
func menuBarMark(running: Bool) -> NSImage {
    let image = NSImage(size: NSSize(width: 18, height: 18), flipped: false) { _ in
        let c = NSPoint(x: 9, y: 9), r: CGFloat = 6.3
        let alpha: CGFloat = running ? 1 : 0.45
        let ring = NSBezierPath(ovalIn: NSRect(x: c.x - r, y: c.y - r, width: 2 * r, height: 2 * r))
        ring.lineWidth = 1.1
        NSColor.black.withAlphaComponent(alpha * 0.5).setStroke()
        ring.stroke()
        let sites = [90.0, 210.0, 330.0].map { a in
            NSPoint(x: c.x + r * cos(a * .pi / 180), y: c.y + r * sin(a * .pi / 180))
        }
        let triangle = NSBezierPath()
        triangle.move(to: sites[0])
        triangle.line(to: sites[1])
        triangle.line(to: sites[2])
        triangle.close()
        triangle.lineWidth = 1.2
        NSColor.black.withAlphaComponent(alpha).set()
        triangle.stroke()
        for p in sites {
            NSBezierPath(ovalIn: NSRect(x: p.x - 2.2, y: p.y - 2.2, width: 4.4, height: 4.4)).fill()
        }
        // The hollow centers, cleared rather than left unfilled, so the lines do not show through.
        NSGraphicsContext.current?.compositingOperation = .clear
        for p in sites {
            NSBezierPath(ovalIn: NSRect(x: p.x - 0.9, y: p.y - 0.9, width: 1.8, height: 1.8)).fill()
        }
        NSGraphicsContext.current?.compositingOperation = .sourceOver
        return true
    }
    image.isTemplate = true
    image.accessibilityDescription = "WeCoLab"
    return image
}
