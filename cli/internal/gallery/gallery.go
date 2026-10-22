package gallery

import "embed"

//go:embed gallery.html
var HTML []byte

// Assets is the home-screen app's manifest and icons. The PNGs are rendered from icon-app.svg, which
// pads the glyph inside the maskable safe zone and fills the square so iOS rounds it itself:
//
//	for px in 180 192 512; do rsvg-convert -w $px -h $px icon-app.svg -o icon-$px.png; done
//
//go:embed manifest.webmanifest icon.svg icon-180.png icon-192.png icon-512.png
var Assets embed.FS
