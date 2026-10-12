// Package imageprep turns a picture into something a vision model can be sent.
//
// Phone screenshots are 3–8 MB and 3000 px wide, which is both more pixels than
// a model samples and more bytes than a request wants to carry as base64: the
// encoding adds a third again, so an untouched screenshot becomes a 10 MB body.
// Everything here exists to make that number small without making the picture
// unreadable — a face, a chart or a line of text has to survive.
package imageprep

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"

	// Decoders are registered for their side effects: image.Decode needs them,
	// and the formats are exactly what a phone sends.
	_ "image/gif"
	_ "image/png"
)

// The long edge a vision model actually samples. Above this the extra pixels are
// thrown away upstream, so sending them costs time and money for nothing.
const MaxEdge = 1568

// A ceiling on the encoded result. Below it the base64 of a JPEG stays inside a
// request body comfortably; above it every provider starts refusing.
const MaxBytes = 1200 * 1024

// Quality of the first attempt. It is stepped down only if the size ceiling is
// still missed after resizing.
const Quality = 82

// Prepared is the result: the bytes, the MIME type they now are, and what was
// done to get there, which is worth reporting so a caller can say why an answer
// looks like it does.
type Prepared struct {
	Data      []byte
	MediaType string
	Width     int
	Height    int
	// OriginalWidth and OriginalHeight are what arrived, so a caller can tell
	// whether the picture was shrunk at all.
	OriginalWidth  int
	OriginalHeight int
	// Resized and Reencoded are false only when the input was already a small
	// JPEG, in which case the bytes are passed through untouched.
	Resized   bool
	Reencoded bool
}

// Scale reports the factor the long edge was reduced by, 1 when untouched.
func (p Prepared) Scale() float64 {
	longest := p.OriginalWidth
	if p.OriginalHeight > longest {
		longest = p.OriginalHeight
	}
	if longest == 0 {
		return 1
	}
	now := p.Width
	if p.Height > now {
		now = p.Height
	}
	return float64(now) / float64(longest)
}

// Prepare decodes a picture and returns a JPEG no larger than MaxEdge on its
// long side and no bigger than MaxBytes.
func Prepare(raw []byte) (Prepared, error) {
	return PrepareWithin(raw, MaxBytes)
}

// PrepareWithin is Prepare with a caller-chosen byte budget, for a caller that
// has to fit somewhere tighter than a request body — a tool result returned as
// text, say, where the base64 of the result is charged by the character.
//
// The budget is met by giving up pixels before quality: a smaller picture a
// model can still read beats a sharp one it never receives. The long edge steps
// down until it fits, and only then does the quality follow.
func PrepareWithin(raw []byte, maxBytes int) (Prepared, error) {
	src, format, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return Prepared{}, fmt.Errorf("not a picture this can decode: %w", err)
	}
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width == 0 || height == 0 {
		return Prepared{}, fmt.Errorf("picture has no pixels")
	}
	out := Prepared{
		Data:           raw,
		MediaType:      "image/" + format,
		Width:          width,
		Height:         height,
		OriginalWidth:  width,
		OriginalHeight: height,
	}
	if maxBytes <= 0 {
		maxBytes = MaxBytes
	}
	if format == "jpeg" && fits(width, height) && len(raw) <= maxBytes {
		return out, nil
	}

	// Each pass is a smaller box; within a box the quality steps down. The floor
	// stops the loop from shrinking a picture into something useless just to
	// satisfy a budget that was never going to be met.
	edge := MaxEdge
	for {
		target := fitTo(width, height, edge)
		scaled := src
		if target.w != width || target.h != height {
			scaled = resize(src, target.w, target.h)
		}
		for _, quality := range []int{Quality, 70, 58, 45} {
			var buf bytes.Buffer
			if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: quality}); err != nil {
				return Prepared{}, fmt.Errorf("cannot encode picture: %w", err)
			}
			out.Data = buf.Bytes()
			out.Reencoded = true
			out.MediaType = "image/jpeg"
			out.Width, out.Height = target.w, target.h
			out.Resized = target.w != width || target.h != height
			if buf.Len() <= maxBytes {
				return out, nil
			}
		}
		if edge <= minEdge {
			// Nothing left to give: the caller gets the smallest version and can
			// decide from the size whether to use it.
			return out, nil
		}
		edge = edge * 3 / 4
	}
}

// minEdge is the point below which shrinking stops being worth it: a picture
// this small rarely survives the trip anyway, and reporting the size is more
// honest than sending a thumbnail nobody asked for.
const minEdge = 512

func fits(width, height int) bool {
	return width <= MaxEdge && height <= MaxEdge
}

type box struct{ w, h int }

// fit is the largest box with the same shape as the input that is no wider or
// taller than MaxEdge. A picture already inside the box is returned unchanged,
// so nothing is enlarged.
func fit(width, height int) box { return fitTo(width, height, MaxEdge) }

// fitTo is the largest box with the same shape as the input that is no wider or
// taller than edge. A picture already inside the box is returned unchanged, so
// nothing is ever enlarged.
func fitTo(width, height, edge int) box {
	if width <= edge && height <= edge {
		return box{width, height}
	}
	if width >= height {
		return box{edge, max(1, height*edge/width)}
	}
	return box{max(1, width*edge/height), edge}
}

// resize averages the source pixels each destination pixel covers. That is the
// area filter, and for the only direction this is used in — down — it is both
// the cheapest and the one that keeps detail: every source pixel contributes, so
// thin lines and small text do not fall between samples the way they do when a
// single source pixel is picked per destination pixel.
func resize(src image.Image, width, height int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	bounds := src.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	for y := 0; y < height; y++ {
		y0 := bounds.Min.Y + y*srcH/height
		y1 := bounds.Min.Y + (y+1)*srcH/height
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < width; x++ {
			x0 := bounds.Min.X + x*srcW/width
			x1 := bounds.Min.X + (x+1)*srcW/width
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sumR, sumG, sumB, sumA, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, b, a := src.At(sx, sy).RGBA()
					// RGBA returns 16-bit values; the shift keeps the sums small
					// enough that a large area cannot overflow the uint64.
					sumR += uint64(r >> 8)
					sumG += uint64(g >> 8)
					sumB += uint64(b >> 8)
					sumA += uint64(a >> 8)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.Set(x, y, color.RGBA{
				R: uint8(sumR / n),
				G: uint8(sumG / n),
				B: uint8(sumB / n),
				A: uint8(sumA / n),
			})
		}
	}
	return dst
}
