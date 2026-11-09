// Generates the app icon and the menu bar template from the logo.
// Usage: swift make-icon.swift logo.png icon.png menubar.png
import AppKit

let args = CommandLine.arguments
let logo = NSImage(contentsOfFile: args[1])!
var proposed = NSRect(origin: .zero, size: logo.size)
let source = logo.cgImage(forProposedRect: &proposed, context: nil, hints: nil)!

func save(_ ctx: CGContext, _ path: String) {
    let rep = NSBitmapImageRep(cgImage: ctx.makeImage()!)
    try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: path))
}

func context(_ size: Int) -> CGContext {
    CGContext(data: nil, width: size, height: size, bitsPerComponent: 8, bytesPerRow: size * 4,
              space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
}

// App icon: the logo on a white macOS-grid tile.
let icon = context(1024)
let tile = CGRect(x: 100, y: 100, width: 824, height: 824)
icon.addPath(CGPath(roundedRect: tile, cornerWidth: 185, cornerHeight: 185, transform: nil))
icon.clip()
icon.setFillColor(.white)
icon.fill(tile)
icon.draw(source, in: tile)
save(icon, args[2])

// Menu bar: a black-on-transparent template, so the system can tint it. The
// pastel colors are mapped to opacity by their distance from white.
let width = source.width, height = source.height
var pixels = [UInt8](repeating: 0, count: width * height * 4)
pixels.withUnsafeMutableBytes { buf in
    let c = CGContext(data: buf.baseAddress, width: width, height: height, bitsPerComponent: 8, bytesPerRow: width * 4,
                      space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
    c.setFillColor(.white)
    c.fill(CGRect(x: 0, y: 0, width: width, height: height))
    c.draw(source, in: CGRect(x: 0, y: 0, width: width, height: height))
}
var minX = width, minY = height, maxX = 0, maxY = 0
for y in 0..<height {
    for x in 0..<width {
        let i = (y * width + x) * 4
        let distance = 255 - Int(min(pixels[i], pixels[i + 1], pixels[i + 2]))
        let alpha = UInt8(max(0, min(255, (distance - 14) * 255 / 40)))
        pixels[i] = 0; pixels[i + 1] = 0; pixels[i + 2] = 0; pixels[i + 3] = alpha
        if alpha > 128 {
            minX = min(minX, x); maxX = max(maxX, x); minY = min(minY, y); maxY = max(maxY, y)
        }
    }
}
let mask = pixels.withUnsafeMutableBytes { buf -> CGImage in
    CGContext(data: buf.baseAddress, width: width, height: height, bitsPerComponent: 8, bytesPerRow: width * 4,
              space: CGColorSpace(name: CGColorSpace.sRGB)!, bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!.makeImage()!
}
let side = max(maxX - minX, maxY - minY) + 1
let crop = mask.cropping(to: CGRect(x: minX - (side - (maxX - minX)) / 2, y: minY - (side - (maxY - minY)) / 2, width: side, height: side))!
let bar = context(44)
bar.interpolationQuality = .high
bar.draw(crop, in: CGRect(x: 0, y: 0, width: 44, height: 44))
save(bar, args[3])
