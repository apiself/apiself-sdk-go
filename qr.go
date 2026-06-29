package sdk

import (
	"encoding/base64"

	qrcode "github.com/skip2/go-qrcode"
)

// QRCodePNG encodes text as a QR-code PNG at the given pixel size with medium
// error correction. Shared helper so any box can render a QR (e.g. auth 2FA
// enrollment, a download link) without depending on the scancodes box at
// runtime or vendoring its own QR library. For a richer user-facing QR product
// (formats, styling, batch) use the scancodes box; this is the lightweight
// in-box primitive.
//
// size <= 0 defaults to 256px.
func QRCodePNG(text string, size int) ([]byte, error) {
	if size <= 0 {
		size = 256
	}
	return qrcode.Encode(text, qrcode.Medium, size)
}

// QRCodeDataURI is QRCodePNG wrapped as a `data:image/png;base64,...` URI ready
// to drop into an <img src>. Returns "" on error so callers can fall back to a
// manual/textual representation without branching on the error.
func QRCodeDataURI(text string, size int) string {
	png, err := QRCodePNG(text, size)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}
