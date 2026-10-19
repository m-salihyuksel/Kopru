package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const thumbShortSide = 320

type thumbCache struct {
	mu    sync.Mutex
	m     map[string][]byte
	order []string
	size  int
	limit int
	sem   chan struct{}
}

func newThumbCache(limit int) *thumbCache {
	return &thumbCache{m: map[string][]byte{}, limit: limit, sem: make(chan struct{}, 2)}
}

func (c *thumbCache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.m[key]
	return b, ok
}

func (c *thumbCache) put(key string, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; ok {
		return
	}
	c.m[key] = b
	c.order = append(c.order, key)
	c.size += len(b)
	for c.size > c.limit && len(c.order) > 0 {
		old := c.order[0]
		c.order = c.order[1:]
		c.size -= len(c.m[old])
		delete(c.m, old)
	}
}

// makeThumb, küçük bir JPEG önizleme üretir ve iPhone fotoğraflarının döndürme bilgisini (EXIF) uygular.
func makeThumb(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	ext := strings.ToLower(filepath.Ext(path))
	orient := 1
	if ext == ".jpg" || ext == ".jpeg" {
		head := make([]byte, 256<<10)
		n, _ := io.ReadFull(f, head)
		orient = jpegOrientation(head[:n])
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
	}
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, err
	}
	if cfg.Width*cfg.Height > 120_000_000 {
		return nil, errors.New("görsel çok büyük")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	small := orientImage(downscale(img, thumbShortSide), orient)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, small, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func downscale(src image.Image, short int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	s := float64(short) / float64(min(w, h))
	if s > 1 {
		s = 1
	}
	dw := max(1, int(float64(w)*s+0.5))
	dh := max(1, int(float64(h)*s+0.5))
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	n := int(math.Ceil(1 / s))
	n = min(max(n, 1), 4)
	cnt := uint32(n * n)
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			var r, g, bl, al uint32
			for j := 0; j < n; j++ {
				sy := b.Min.Y + min(h-1, int((float64(y)+(float64(j)+0.5)/float64(n))/s))
				for i := 0; i < n; i++ {
					sx := b.Min.X + min(w-1, int((float64(x)+(float64(i)+0.5)/float64(n))/s))
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					r += cr
					g += cg
					bl += cb
					al += ca
				}
			}
			r, g, bl, al = r/cnt, g/cnt, bl/cnt, al/cnt
			white := 0xffff - al // saydam alanları beyaza oturt
			o := dst.PixOffset(x, y)
			dst.Pix[o+0] = uint8((r + white) >> 8)
			dst.Pix[o+1] = uint8((g + white) >> 8)
			dst.Pix[o+2] = uint8((bl + white) >> 8)
			dst.Pix[o+3] = 0xff
		}
	}
	return dst
}

func orientImage(src *image.RGBA, o int) *image.RGBA {
	if o <= 1 || o > 8 {
		return src
	}
	w, h := src.Rect.Dx(), src.Rect.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			so := src.PixOffset(x, y)
			do := dst.PixOffset(dx, dy)
			copy(dst.Pix[do:do+4], src.Pix[so:so+4])
		}
	}
	return dst
}

func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		if marker == 0xFF {
			i++
			continue
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			return 1
		}
		segLen := int(b[i+2])<<8 | int(b[i+3])
		if segLen < 2 {
			return 1
		}
		end := i + 2 + segLen
		if marker == 0xE1 && end <= len(b) {
			seg := b[i+4 : end]
			if len(seg) >= 6 && string(seg[:6]) == "Exif\x00\x00" {
				return tiffOrientation(seg[6:])
			}
		}
		i = end
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			v := int(bo.Uint16(t[e+8:]))
			if v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}
