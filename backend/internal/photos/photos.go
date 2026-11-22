// Package photos turns a photo someone uploads into the pictures customers see. Every upload is decoded
// and drawn again as a fresh JPEG, so nothing from the original file survives: no hidden data such as the
// GPS position phones write into photos, and nothing that isn't a picture.
package photos

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // registers PNG decoding
	"net/http"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers WebP decoding
)

const (
	// MaxSize is the largest upload accepted, in bytes.
	MaxSize = 10 << 20
	// MaxPixels guards against small files that decode to huge pictures.
	MaxPixels = 40_000_000
	// Full and Thumb are the longest side, in pixels, of the two pictures kept.
	Full  = 1200
	Thumb = 320
	// MinSide: smaller pictures look broken in the app.
	MinSide = 200
)

var (
	ErrNotPhoto = errors.New("photos: not a JPEG, PNG or WebP picture")
	ErrTooLarge = errors.New("photos: picture too large")
	ErrTooSmall = errors.New("photos: picture too small")
)

// Kinds are the formats accepted, by their sniffed content type.
var kinds = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true}

// Processed is an upload drawn again at the two sizes kept.
type Processed struct {
	Full, Thumb []byte
}

// Process checks an upload by its content, not its name, and returns it as two JPEGs.
func Process(data []byte) (*Processed, error) {
	if len(data) > MaxSize {
		return nil, ErrTooLarge
	}
	if !kinds[http.DetectContentType(data)] {
		return nil, ErrNotPhoto
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, ErrNotPhoto
	}
	if cfg.Width*cfg.Height > MaxPixels {
		return nil, ErrTooLarge
	}
	if cfg.Width < MinSide || cfg.Height < MinSide {
		return nil, ErrTooSmall
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, ErrNotPhoto
	}
	full, err := encode(fit(img, Full), 85)
	if err != nil {
		return nil, err
	}
	thumb, err := encode(fit(img, Thumb), 80)
	if err != nil {
		return nil, err
	}
	return &Processed{Full: full, Thumb: thumb}, nil
}

// fit scales a picture down so its longest side is at most limit, on a white background (for transparent PNGs).
func fit(src image.Image, limit int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > limit || h > limit {
		if w >= h {
			w, h = limit, max(1, h*limit/w)
		} else {
			w, h = max(1, w*limit/h), limit
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

func encode(img image.Image, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("photos: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// NewID names a stored photo. Each upload gets a new one, so the pictures can be cached for good.
func NewID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Store keeps the pictures: any S3-compatible bucket (see kyb.S3, which this shares).
type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
}
