package imageprobe

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
)

func png(w, h uint32) []byte {
	buf := make([]byte, 33)
	copy(buf, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	binary.BigEndian.PutUint32(buf[8:12], 13)
	copy(buf[12:16], "IHDR")
	binary.BigEndian.PutUint32(buf[16:20], w)
	binary.BigEndian.PutUint32(buf[20:24], h)
	buf[24] = 8
	buf[25] = 6
	return buf
}

func TestPixelAreaPNG(t *testing.T) {
	if got := PixelArea(png(5504, 3072)); got != 5504*3072 {
		t.Fatalf("PNG area = %d, want %d", got, 5504*3072)
	}
	if got := PixelAreaFromBase64(base64.StdEncoding.EncodeToString(png(1376, 768))); got != 1376*768 {
		t.Fatalf("base64 PNG area = %d, want %d", got, 1376*768)
	}
}

func TestPixelAreaJPEG(t *testing.T) {
	// SOI + APP0(16 字节段) + SOF0
	buf := []byte{0xFF, 0xD8}
	app0 := []byte{0xFF, 0xE0, 0x00, 0x10}
	app0 = append(app0, make([]byte, 14)...)
	buf = append(buf, app0...)
	sof := []byte{0xFF, 0xC0, 0x00, 0x11, 0x08, 0x0C, 0x00, 0x15, 0x80, 0x03}
	buf = append(buf, sof...)
	buf = append(buf, make([]byte, 20)...)
	if got := PixelArea(buf); got != 5504*3072 {
		t.Fatalf("JPEG area = %d, want %d", got, 5504*3072)
	}
}

func TestPixelAreaUnknownAndTiny(t *testing.T) {
	if got := PixelArea([]byte("not an image")); got != 0 {
		t.Fatalf("unknown format area = %d, want 0", got)
	}
	if got := PixelArea(nil); got != 0 {
		t.Fatalf("nil area = %d, want 0", got)
	}
	if got := PixelAreaFromBase64(""); got != 0 {
		t.Fatalf("empty base64 area = %d, want 0", got)
	}
	if got := PixelAreaFromBase64("!!!not-base64!!!"); got != 0 {
		t.Fatalf("bad base64 area = %d, want 0", got)
	}
}
