package photos_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"superapp/backend/internal/photos"
)

func picture(w, h int) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.NRGBA{uint8(x), uint8(y), 120, 255})
		}
	}
	return img
}

func pngBytes(t *testing.T, img image.Image) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func size(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "jpeg" {
		t.Fatalf("not a JPEG: %v %s", err, format)
	}
	return cfg.Width, cfg.Height
}

func TestPhotosAreScaledAndReencoded(t *testing.T) {
	out, err := photos.Process(pngBytes(t, picture(1600, 900)))
	if err != nil {
		t.Fatal(err)
	}
	if w, h := size(t, out.Full); w != 1200 || h != 675 {
		t.Errorf("full: %dx%d", w, h)
	}
	if w, h := size(t, out.Thumb); w != 320 || h != 180 {
		t.Errorf("thumb: %dx%d", w, h)
	}
	// Portrait, and smaller than the full size: kept as it is, only the thumbnail shrinks.
	out, err = photos.Process(pngBytes(t, picture(400, 800)))
	if err != nil {
		t.Fatal(err)
	}
	if w, h := size(t, out.Full); w != 400 || h != 800 {
		t.Errorf("small full: %dx%d", w, h)
	}
	if w, h := size(t, out.Thumb); w != 160 || h != 320 {
		t.Errorf("portrait thumb: %dx%d", w, h)
	}
}

func TestLocationDataIsDropped(t *testing.T) {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, picture(300, 300), nil); err != nil {
		t.Fatal(err)
	}
	src := b.Bytes()
	// An APP1 "Exif" segment right after the start marker, as phone cameras write, with a GPS marker in it.
	payload := append([]byte("Exif\x00\x00GPSLatitude 9.0108N GPSLongitude 38.7613E"), make([]byte, 32)...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2))
	withExif := append(append(append([]byte{}, src[:2]...), append(seg, payload...)...), src[2:]...)
	if !bytes.Contains(withExif, []byte("GPSLatitude")) {
		t.Fatal("test photo lacks its marker")
	}
	out, err := photos.Process(withExif)
	if err != nil {
		t.Fatal(err)
	}
	for _, pic := range [][]byte{out.Full, out.Thumb} {
		if bytes.Contains(pic, []byte("GPS")) || bytes.Contains(pic, []byte("Exif")) {
			t.Fatal("location data survived")
		}
	}
}

// hugePNG is a valid PNG header claiming a 20,000 × 20,000 picture, with no pixels behind it.
func hugePNG() []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 20000)
	binary.BigEndian.PutUint32(ihdr[4:], 20000)
	ihdr[8], ihdr[9] = 8, 2 // 8-bit RGB
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], 13)
	b.Write(n[:])
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	binary.BigEndian.PutUint32(n[:], crc32.ChecksumIEEE(chunk))
	b.Write(n[:])
	return b.Bytes()
}

func TestRefusedUploads(t *testing.T) {
	for name, tc := range map[string]struct {
		data []byte
		want error
	}{
		"text":        {[]byte("hello, this is not a picture at all"), photos.ErrNotPhoto},
		"pdf":         {[]byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n"), photos.ErrNotPhoto},
		"svg":         {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), photos.ErrNotPhoto},
		"broken jpeg": {[]byte("\xff\xd8\xff\xe0 not really a jpeg"), photos.ErrNotPhoto},
		"tiny":        {pngBytes(t, picture(120, 400)), photos.ErrTooSmall},
		"pixel bomb":  {hugePNG(), photos.ErrTooLarge},
		"over 10 MB":  {make([]byte, photos.MaxSize+1), photos.ErrTooLarge},
	} {
		if _, err := photos.Process(tc.data); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if len(photos.NewID()) != 24 || photos.NewID() == photos.NewID() {
		t.Error("photo IDs")
	}
}
