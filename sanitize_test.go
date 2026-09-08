package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"
	"time"
)

// Every test here is one line from the list in SECURITY.md. The same corpus is
// meant to run against any port of this service - if a rule cannot be expressed as a test, it
// is not a rule, it is a hope.

func pngChunk(name string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	body := append([]byte(name), data...)
	out = append(out, body...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(body))
}

// samplePNG returns a valid PNG with a tEXt and an eXIf chunk spliced in after IHDR, and the
// bytes of a ZIP archive appended after IEND.
func samplePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	clean := buf.Bytes()

	// IHDR is always the first chunk: 8 bytes of signature, then 8 + 13 + 4.
	const afterIHDR = 8 + 12 + 13
	out := append([]byte(nil), clean[:afterIHDR]...)
	out = append(out, pngChunk("tEXt", []byte("Comment\x00hello"))...)
	out = append(out, pngChunk("eXIf", []byte("II*\x00GPS 51.5074 -0.1278"))...)
	out = append(out, clean[afterIHDR:]...)
	out = append(out, []byte("PK\x03\x04TRAILING ARCHIVE PAYLOAD")...)
	return out
}

func sampleJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	clean := buf.Bytes()

	// Splice an APP1/Exif segment in right after SOI, and append trailing bytes.
	exif := []byte("Exif\x00\x00II*\x00GPS 51.5074 -0.1278")
	segment := append([]byte{0xFF, 0xE1}, byte((len(exif)+2)>>8), byte((len(exif)+2)&0xFF))
	segment = append(segment, exif...)

	out := append([]byte(nil), clean[:2]...)
	out = append(out, segment...)
	out = append(out, clean[2:]...)
	return append(out, []byte("TRAILING PAYLOAD")...)
}

// sampleGIF returns a three-frame animation carrying a comment extension and a trailing blob.
func sampleGIF(t *testing.T, frames int) []byte {
	t.Helper()
	g := &gif.GIF{LoopCount: 0}
	for range frames {
		img := image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White})
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	clean := buf.Bytes()

	// A comment extension goes in just before the trailer, and a payload just after it.
	comment := []byte{0x21, 0xFE, 5, 'h', 'e', 'l', 'l', 'o', 0x00}
	out := append([]byte(nil), clean[:len(clean)-1]...)
	out = append(out, comment...)
	out = append(out, 0x3B)
	return append(out, []byte("TRAILING PAYLOAD")...)
}

// sampleWebP hand-builds a lossless WebP carrying EXIF and XMP chunks. Nothing here decodes,
// so the VP8L payload only has to be well-formed as far as its 5-byte header.
func sampleWebP(w, h int) []byte {
	riffChunk := func(name string, payload []byte) []byte {
		out := append([]byte(name), binary.LittleEndian.AppendUint32(nil, uint32(len(payload)))...)
		out = append(out, payload...)
		if len(payload)%2 == 1 {
			out = append(out, 0)
		}
		return out
	}

	bits := uint32(w-1) | uint32(h-1)<<14
	vp8l := append([]byte{0x2F}, binary.LittleEndian.AppendUint32(nil, bits)...)
	vp8l = append(vp8l, 0x00, 0x00, 0x00, 0x00)

	// VP8X with the EXIF (0x08) and XMP (0x04) flags set, as a real file with those chunks has.
	vp8x := make([]byte, 10)
	vp8x[0] = 0x08 | 0x04
	vp8x[4], vp8x[5], vp8x[6] = byte((w-1)&0xFF), byte(((w-1)>>8)&0xFF), byte(((w-1)>>16)&0xFF)
	vp8x[7], vp8x[8], vp8x[9] = byte((h-1)&0xFF), byte(((h-1)>>8)&0xFF), byte(((h-1)>>16)&0xFF)

	body := riffChunk("VP8X", vp8x)
	body = append(body, riffChunk("EXIF", []byte("GPS 51.5074 -0.1278"))...)
	body = append(body, riffChunk("XMP ", []byte("<x:xmpmeta/>"))...)
	body = append(body, riffChunk("VP8L", vp8l)...)

	out := append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(4+len(body)))...)
	out = append(out, "WEBP"...)
	return append(out, body...)
}

func TestRejectsTypesOutsideTheAllowlist(t *testing.T) {
	cases := map[string][]byte{
		// No SVG: it carries JavaScript.
		"SVG":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"PDF":  []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n"),
		"HTML": []byte("<!doctype html><script>alert(1)</script>"),
		"ZIP":  []byte("PK\x03\x04\x14\x00\x00\x00"),
		"ELF":  {0x7F, 'E', 'L', 'F', 2, 1, 1, 0},
		// A QuickTime .mov carries ftyp too, with a "qt  " brand.
		"MOV": append([]byte{0, 0, 0, 0x14}, []byte("ftypqt  \x00\x00\x02\x00qt  ")...),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Sanitize(body); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// Matroska shares the EBML signature with WebM, so the signature alone is not enough.
func TestRejectsMatroskaPosingAsWebM(t *testing.T) {
	mkv := append([]byte{0x1A, 0x45, 0xDF, 0xA3}, []byte("\x42\x82\x88matroska")...)
	mkv = append(mkv, make([]byte, 64)...)
	if _, _, err := Sanitize(mkv); err == nil {
		t.Fatal("a Matroska file was accepted as WebM")
	}
}

func TestPNGDropsMetadataAndTrailingBytes(t *testing.T) {
	out, kind, err := Sanitize(samplePNG(t, 8, 8))
	if err != nil {
		t.Fatal(err)
	}
	if kind.Ext != "png" {
		t.Fatalf("extension = %q, want png", kind.Ext)
	}
	for _, needle := range []string{"tEXt", "eXIf", "GPS 51.5074", "TRAILING ARCHIVE PAYLOAD"} {
		if bytes.Contains(out, []byte(needle)) {
			t.Errorf("%q survived", needle)
		}
	}
	if !bytes.HasSuffix(out, []byte("IEND\xae\x42\x60\x82")) {
		t.Error("output does not end at IEND")
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("the stripped PNG no longer decodes: %v", err)
	}
}

func TestJPEGDropsEXIFAndTrailingBytes(t *testing.T) {
	out, kind, err := Sanitize(sampleJPEG(t))
	if err != nil {
		t.Fatal(err)
	}
	if kind.Ext != "jpg" {
		t.Fatalf("extension = %q, want jpg", kind.Ext)
	}
	for _, needle := range []string{"Exif", "GPS 51.5074", "TRAILING PAYLOAD"} {
		if bytes.Contains(out, []byte(needle)) {
			t.Errorf("%q survived", needle)
		}
	}
	if !bytes.HasSuffix(out, []byte{0xFF, 0xD9}) {
		t.Error("output does not end at EOI")
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("the stripped JPEG no longer decodes: %v", err)
	}
}

// Pasting a screen recording into a PR is the whole point, so every frame has to survive.
func TestGIFKeepsEveryFrameAndDropsComments(t *testing.T) {
	const frames = 3
	out, kind, err := Sanitize(sampleGIF(t, frames))
	if err != nil {
		t.Fatal(err)
	}
	if kind.Ext != "gif" {
		t.Fatalf("extension = %q, want gif", kind.Ext)
	}
	if bytes.Contains(out, []byte("hello")) {
		t.Error("the comment extension survived")
	}
	if bytes.Contains(out, []byte("TRAILING PAYLOAD")) {
		t.Error("trailing bytes survived")
	}

	decoded, err := gif.DecodeAll(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("the stripped GIF no longer decodes: %v", err)
	}
	if len(decoded.Image) != frames {
		t.Fatalf("frames = %d, want %d", len(decoded.Image), frames)
	}
	// NETSCAPE2.0 is an application extension, and dropping it would stop the loop.
	if !bytes.Contains(out, []byte("NETSCAPE2.0")) {
		t.Error("the loop extension was dropped, so the animation would play once")
	}
}

func TestWebPDropsEXIFAndClearsTheFlags(t *testing.T) {
	out, kind, err := Sanitize(sampleWebP(8, 8))
	if err != nil {
		t.Fatal(err)
	}
	if kind.Ext != "webp" {
		t.Fatalf("extension = %q, want webp", kind.Ext)
	}
	if bytes.Contains(out, []byte("EXIF")) || bytes.Contains(out, []byte("GPS 51.5074")) {
		t.Error("the EXIF chunk survived")
	}
	if bytes.Contains(out, []byte("XMP ")) {
		t.Error("the XMP chunk survived")
	}
	if !bytes.Contains(out, []byte("VP8L")) {
		t.Fatal("the image data was dropped")
	}
	// The VP8X flags byte sits 8 bytes into the chunk, which starts at offset 12.
	if flags := out[12+8]; flags&(0x08|0x04) != 0 {
		t.Errorf("VP8X flags = %#x, still advertising EXIF or XMP", flags)
	}
	// The RIFF size field has to match what is left after the drops.
	if got, want := binary.LittleEndian.Uint32(out[4:8]), uint32(len(out)-8); got != want {
		t.Errorf("RIFF size = %d, want %d", got, want)
	}
}

// A few hundred KB of PNG can declare a canvas of billions of pixels.
func TestRejectsOversizedCanvas(t *testing.T) {
	body := samplePNG(t, 8, 8)
	// Overwrite the IHDR width and height with 40000 x 40000 = 1.6 gigapixels.
	binary.BigEndian.PutUint32(body[16:20], 40000)
	binary.BigEndian.PutUint32(body[20:24], 40000)

	if _, _, err := Sanitize(body); err == nil {
		t.Fatal("a 1.6 gigapixel canvas was accepted")
	}
}

func TestExtensionComesFromTheBytesNotTheFilename(t *testing.T) {
	_, kind, err := Sanitize(sampleGIF(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	if kind.Ext != "gif" || kind.ContentType != "image/gif" {
		t.Fatalf("got %q/%q, want gif/image/gif", kind.Ext, kind.ContentType)
	}
}

func TestVideoPassesThroughUnchanged(t *testing.T) {
	mp4 := append([]byte{0, 0, 0, 0x18}, []byte("ftypisom\x00\x00\x02\x00isomiso2mp41")...)
	mp4 = append(mp4, make([]byte, 32)...)

	out, kind, err := Sanitize(mp4)
	if err != nil {
		t.Fatal(err)
	}
	if !kind.Video || kind.Ext != "mp4" {
		t.Fatalf("got %+v, want an mp4 video", kind)
	}
	if !bytes.Equal(out, mp4) {
		t.Error("video was modified; it is documented as stored exactly as it arrived")
	}
}

func TestTruncatedInputIsRejectedNotPanicking(t *testing.T) {
	full := samplePNG(t, 8, 8)
	for n := 1; n < len(full); n += 7 {
		if _, _, err := Sanitize(full[:n]); err == nil && n < len(full)-30 {
			t.Fatalf("a %d-byte prefix was accepted", n)
		}
	}
}

const month = 30 * 24 * time.Hour

func TestRetentionIsCappedNotRefused(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", "720h0m0s"}, // empty means the instance value
		{"30m", "30m0s"},
		{"6h", "6h0m0s"},
		{"7d", "168h0m0s"},
		{"365d", "720h0m0s"}, // capped at the instance retention, not refused
		{"1", "1m0s"},        // below the floor, raised to it
		{"0", "720h0m0s"},    // 0 means no expiry of its own: the instance value
	}
	for _, c := range cases {
		got, err := RetentionFrom(c.in, month)
		if err != nil {
			t.Fatalf("RetentionFrom(%q): %v", c.in, err)
		}
		if got.String() != c.want {
			t.Errorf("RetentionFrom(%q) = %s, want %s", c.in, got, c.want)
		}
	}
	if _, err := RetentionFrom("soon", month); err == nil {
		t.Error("a nonsense expires_in was accepted")
	}
	// The cap is the instance's, not the compiled-in default.
	if got, _ := RetentionFrom("", 2*time.Hour); got != 2*time.Hour {
		t.Errorf("empty expires_in = %s, want the instance retention", got)
	}
	if got, _ := RetentionFrom("7d", 2*time.Hour); got != 2*time.Hour {
		t.Errorf("7d against a 2h instance = %s, want 2h", got)
	}
	// A forever instance: empty stays forever, an explicit value is honoured uncapped.
	if got, _ := RetentionFrom("", 0); got != 0 {
		t.Errorf("empty expires_in on a forever instance = %s, want 0", got)
	}
	if got, _ := RetentionFrom("365d", 0); got != 365*24*time.Hour {
		t.Errorf("365d on a forever instance = %s, want 365 days", got)
	}
	if got, _ := RetentionFrom("0", 0); got != 0 {
		t.Errorf("0 on a forever instance = %s, want 0 (never)", got)
	}
	// An amount that would overflow int64 nanoseconds is refused, not wrapped into a
	// negative that the floor would then raise to one minute.
	for _, in := range []string{"10000000000", "200000d", "3000000h"} {
		if _, err := RetentionFrom(in, month); err == nil {
			t.Errorf("RetentionFrom(%q) accepted an overflowing amount", in)
		}
	}
}

func TestHumanDurationIsExact(t *testing.T) {
	for in, want := range map[time.Duration]string{
		0: "never", 30 * 24 * time.Hour: "30 days", 24 * time.Hour: "1 day", 36 * time.Hour: "36 hours",
		time.Hour: "1 hour", 90 * time.Minute: "90 minutes", time.Minute: "1 minute", 90 * time.Second: "90 seconds",
	} {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestMarkdownPerType(t *testing.T) {
	const u = "https://prunto.test/blobs/k.ext"
	if got := (Upload{ContentType: "image/png"}).Markdown(u); got != "![]("+u+")" {
		t.Errorf("image markdown = %q", got)
	}
	// GitHub strips an external <video> tag, so a video is a link, never a tag.
	if got := (Upload{ContentType: "video/mp4"}).Markdown(u); got != "[Watch the video]("+u+")" {
		t.Errorf("video markdown = %q", got)
	}
}

func TestRetentionEnv(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	// Pinned so a developer's own exported values cannot fail a test about RETENTION.
	t.Setenv("PRUNTO_BASE_URL", "http://prunto.test")
	t.Setenv("TRUST_PROXY", "none")
	for in, want := range map[string]time.Duration{"": 0, "never": 0, " Never ": 0, "0": 0, "7d": 7 * 24 * time.Hour, "90m": 90 * time.Minute, "3600": time.Hour} {
		t.Setenv("RETENTION", in)
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("RETENTION=%q: %v", in, err)
		}
		if cfg.Retention != want {
			t.Errorf("RETENTION=%q = %s, want %s", in, cfg.Retention, want)
		}
	}
	for _, in := range []string{"soon", "30s", "-1d", "1099511627776d", "99999999999999999999"} {
		t.Setenv("RETENTION", in)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("RETENTION=%q was accepted", in)
		}
	}
}

// A progressive JPEG carries several scans. Treating the first one as the end of the file
// would silently truncate every progressive image, which is most of what a "save for web"
// export produces.
func TestProgressiveJPEGKeepsEveryScan(t *testing.T) {
	seg := func(marker byte, payload []byte) []byte {
		out := []byte{0xFF, marker, byte((len(payload) + 2) >> 8), byte((len(payload) + 2) & 0xFF)}
		return append(out, payload...)
	}
	// Entropy-coded data, including a stuffed FF00 that must not read as a marker.
	scanOne := []byte{0x11, 0x22, 0xFF, 0x00, 0x33}
	scanTwo := []byte{0x44, 0x55, 0xFF, 0x00, 0x66}

	var b []byte
	b = append(b, 0xFF, 0xD8)
	// SOF2 (progressive): precision, height, width, one component.
	b = append(b, seg(0xC2, []byte{8, 0, 16, 0, 16, 1, 1, 0x11, 0})...)
	b = append(b, seg(0xE1, []byte("Exif\x00\x00GPS 51.5074 -0.1278"))...)
	b = append(b, seg(0xC4, []byte{0x00, 0x01})...)
	b = append(b, seg(0xDA, []byte{1, 1, 0x00, 0, 63, 0})...)
	b = append(b, scanOne...)
	b = append(b, seg(0xC4, []byte{0x01, 0x02})...)
	b = append(b, seg(0xDA, []byte{1, 1, 0x00, 1, 63, 0})...)
	b = append(b, scanTwo...)
	b = append(b, 0xFF, 0xD9)
	b = append(b, []byte("TRAILING PAYLOAD")...)

	out, kind, err := Sanitize(b)
	if err != nil {
		t.Fatal(err)
	}
	if kind.Ext != "jpg" {
		t.Fatalf("extension = %q, want jpg", kind.Ext)
	}
	if !bytes.Contains(out, scanOne) {
		t.Error("the first scan was dropped")
	}
	if !bytes.Contains(out, scanTwo) {
		t.Error("the second scan was dropped - a progressive image would be truncated")
	}
	if bytes.Contains(out, []byte("Exif")) || bytes.Contains(out, []byte("GPS 51.5074")) {
		t.Error("the EXIF segment survived")
	}
	if bytes.Contains(out, []byte("TRAILING PAYLOAD")) {
		t.Error("trailing bytes survived")
	}
	if !bytes.HasSuffix(out, []byte{0xFF, 0xD9}) {
		t.Error("output does not end at EOI")
	}
}

// PNG dimensions are uint32, so a maximal canvas overflows a 64-bit int to a negative number.
// Multiplying to check the cap let exactly that value through.
func TestOversizedCanvasCannotOverflowTheGuard(t *testing.T) {
	body := samplePNG(t, 8, 8)
	binary.BigEndian.PutUint32(body[16:20], 4294967295)
	binary.BigEndian.PutUint32(body[20:24], 4294967295)

	if _, _, err := Sanitize(body); err == nil {
		t.Fatal("a 4294967295x4294967295 canvas was accepted")
	}
}
