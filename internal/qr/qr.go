package qr

import (
	qrcode "github.com/skip2/go-qrcode"
)

const qrPNGSize = 256

// GenerateQR returns a 256×256 PNG encoding of link.
func GenerateQR(link string) ([]byte, error) {
	return qrcode.Encode(link, qrcode.Medium, qrPNGSize)
}
