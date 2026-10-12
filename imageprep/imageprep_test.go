package imageprep

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// bigJPEG is a picture larger than the limit, with a recognisable pattern so a
// resize can be checked rather than merely not crashing.
func bigJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			switch {
			case x < width/2 && y < height/2:
				img.Set(x, y, color.RGBA{220, 30, 30, 255})
			case x >= width/2 && y < height/2:
				img.Set(x, y, color.RGBA{30, 220, 30, 255})
			case x < width/2:
				img.Set(x, y, color.RGBA{30, 30, 220, 255})
			default:
				img.Set(x, y, color.RGBA{240, 240, 240, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestPrepareShrinksToTheLongEdge(t *testing.T) {
	raw := bigJPEG(t, 3000, 2000)
	got, err := Prepare(raw)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !got.Resized {
		t.Error("a 3000px picture should have been resized")
	}
	if got.Width != MaxEdge {
		t.Errorf("width = %d, want the long edge at %d", got.Width, MaxEdge)
	}
	// The shape has to be kept, or the model sees a distorted face.
	if want := 2000 * MaxEdge / 3000; got.Height != want {
		t.Errorf("height = %d, want %d (aspect ratio must survive)", got.Height, want)
	}
	if len(got.Data) > MaxBytes {
		t.Errorf("%d bytes is over the ceiling of %d", len(got.Data), MaxBytes)
	}
	if got.MediaType != "image/jpeg" {
		t.Errorf("media type = %q, want image/jpeg", got.MediaType)
	}
	if _, err := jpeg.Decode(bytes.NewReader(got.Data)); err != nil {
		t.Errorf("the result must still be a decodable JPEG: %v", err)
	}
}

func TestPrepareLeavesASmallJPEGAlone(t *testing.T) {
	raw := bigJPEG(t, 640, 480)
	got, err := Prepare(raw)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got.Resized || got.Reencoded {
		t.Error("re-encoding a JPEG that already fits only loses detail")
	}
	if !bytes.Equal(got.Data, raw) {
		t.Error("the bytes should be passed through untouched")
	}
}

func TestPrepareAcceptsPNGAndConvertsIt(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 800, 600))
	for i := 0; i < 800*600; i++ {
		img.Set(i%800, i/800, color.RGBA{uint8(i % 255), 40, 200, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	got, err := Prepare(buf.Bytes())
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// A PNG of a photograph is many times the size of the same JPEG, and a
	// screenshot is a photograph as far as a request body is concerned.
	if got.MediaType != "image/jpeg" {
		t.Errorf("media type = %q, want the PNG converted to image/jpeg", got.MediaType)
	}
	if got.Reencoded != true {
		t.Error("a PNG has to be re-encoded to become a JPEG")
	}
}

func TestPrepareDoesNotEnlarge(t *testing.T) {
	raw := bigJPEG(t, 200, 100)
	got, err := Prepare(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Width != 200 || got.Height != 100 {
		t.Errorf("got %dx%d, want the original 200x100", got.Width, got.Height)
	}
}

func TestPrepareRefusesSomethingThatIsNotAPicture(t *testing.T) {
	if _, err := Prepare([]byte("this is a text file, not a picture")); err == nil {
		t.Error("a text file should be refused rather than half-decoded")
	}
}

func TestFitKeepsTheShapeAndTheLongEdge(t *testing.T) {
	cases := []struct {
		w, h         int
		wantW, wantH int
	}{
		{3000, 2000, MaxEdge, 1045},
		{2000, 3000, 1045, MaxEdge},
		{1568, 1568, 1568, 1568},
		{100, 50, 100, 50},
		{4000, 40, MaxEdge, 15},
	}
	for _, c := range cases {
		got := fit(c.w, c.h)
		if got.w != c.wantW || got.h != c.wantH {
			t.Errorf("fit(%d,%d) = %dx%d, want %dx%d", c.w, c.h, got.w, got.h, c.wantW, c.wantH)
		}
	}
}

// The area filter has to average, not sample: a two-pixel checkerboard reduced
// by half must come out grey, and a single-pixel pick would come out black or
// white depending on which pixel was chosen.
func TestResizeAveragesRatherThanSamples(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	src.Set(0, 0, color.RGBA{0, 0, 0, 255})
	src.Set(1, 0, color.RGBA{255, 255, 255, 255})
	src.Set(0, 1, color.RGBA{255, 255, 255, 255})
	src.Set(1, 1, color.RGBA{0, 0, 0, 255})
	got := resize(src, 1, 1)
	r, g, b, _ := got.At(0, 0).RGBA()
	if r>>8 < 100 || r>>8 > 155 {
		t.Errorf("red = %d, want about 128: an average, not a sample", r>>8)
	}
	if r != g || g != b {
		t.Errorf("a grey average should be neutral, got %d,%d,%d", r>>8, g>>8, b>>8)
	}
}
