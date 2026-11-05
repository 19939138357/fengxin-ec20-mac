import AppKit
let output=CommandLine.arguments[1]
let size=NSSize(width:1024,height:1024)
let image=NSImage(size:size)
image.lockFocus()
let rect=NSRect(x:72,y:72,width:880,height:880)
let path=NSBezierPath(roundedRect:rect,xRadius:200,yRadius:200)
NSGradient(starting:NSColor(calibratedRed:0.28,green:0.55,blue:0.96,alpha:1),ending:NSColor(calibratedRed:0.10,green:0.25,blue:0.65,alpha:1))!.draw(in:path,angle:270)
let symbol=NSImage(systemSymbolName:"antenna.radiowaves.left.and.right",accessibilityDescription:nil)!.withSymbolConfiguration(NSImage.SymbolConfiguration(paletteColors:[.white]))!
symbol.draw(in:NSRect(x:227,y:220,width:570,height:570))
image.unlockFocus()
let rep=NSBitmapImageRep(data:image.tiffRepresentation!)!
try rep.representation(using:.png,properties:[:])!.write(to:URL(fileURLWithPath:output))
