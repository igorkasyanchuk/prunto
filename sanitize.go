package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// Sanitising without a decoder.
//
// The type is decided by magic bytes, never by the filename or the client's declared content
// type. What survives is then rebuilt at the container level: metadata chunks are dropped and
// everything past the format's end marker is truncated, so EXIF (and the GPS in it) and any
// appended payload never reach the bucket.
//
// ponytail: no libvips, no re-encode. Decoding attacker-controlled bytes in order to protect
// against attacker-controlled bytes means running the same C codecs a browser would - only in
// this process, beside the database and bucket credentials, and without the browser's sandbox.
// Byte-walking has none of that surface, needs no cgo, and cannot be handed a decompression
// bomb. What it does not do is neutralise a payload hidden inside the compressed pixel stream:
// nosniff, the sniffed content type and the separate CDN origin are what stand between that
// and a browser, which is the same bet already made for video. Swap in a real re-encode here
// if that stops being enough - Sanitize is the only thing that would change.

// MaxPixels bounds the declared canvas. Nothing here decodes, so this is not protecting a
// decoder of ours; it keeps absurd canvases out of the bucket and out of viewers.
const MaxPixels = 50_000_000

// ErrRejected carries a message meant for the uploader.
type ErrRejected struct{ msg string }

func (e ErrRejected) Error() string { return e.msg }

func reject(format string, args ...any) error {
	return ErrRejected{fmt.Sprintf(format, args...)}
}

// Kind is an entry in the allowlist. The extension comes from the sniffed type, so the
// client's filename never decides what the object is called.
type Kind struct {
	ContentType string
	Ext         string
	Video       bool
}

// Allowed is the entire allowlist, keyed by what we sniff out of the bytes.
//
// No SVG: it carries JavaScript. No PDF: a phishing and malware carrier with an entirely
// separate threat model. Two video containers and no more - MP4 and WebM are what a browser
// plays and what GitHub renders from a <video> tag, so a third only widens the surface.
var Allowed = []Kind{
	{"image/png", "png", false},
	{"image/jpeg", "jpg", false},
	{"image/gif", "gif", false},
	{"image/webp", "webp", false},
	{"video/mp4", "mp4", true},
	{"video/webm", "webm", true},
}

var (
	pngSig  = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	ebmlSig = []byte{0x1A, 0x45, 0xDF, 0xA3}
)

// Sniff identifies the bytes by their leading signature alone.
func Sniff(b []byte) (Kind, bool) {
	find := func(ct string) Kind {
		for _, k := range Allowed {
			if k.ContentType == ct {
				return k
			}
		}
		panic("unknown content type in Allowed: " + ct)
	}

	switch {
	case bytes.HasPrefix(b, pngSig):
		return find("image/png"), true
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return find("image/jpeg"), true
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return find("image/gif"), true
	case len(b) > 12 && bytes.HasPrefix(b, []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return find("image/webp"), true
	case isMP4(b):
		return find("video/mp4"), true
	case isWebM(b):
		return find("video/webm"), true
	}
	return Kind{}, false
}

// isMP4 checks for an ftyp box whose major brand or compatible brands say MP4. A QuickTime
// .mov also carries ftyp, with a "qt  " brand, and is refused: it buys nothing a re-mux to
// MP4 does not, and every extra container is more surface.
func isMP4(b []byte) bool {
	if len(b) < 12 || !bytes.Equal(b[4:8], []byte("ftyp")) {
		return false
	}
	size := int(binary.BigEndian.Uint32(b[0:4]))
	if size < 12 || size > len(b) {
		size = len(b)
	}
	// Major brand plus the compatible-brands list, each four bytes from offset 8.
	for i := 8; i+4 <= size; i += 4 {
		switch string(b[i : i+4]) {
		case "isom", "iso2", "iso4", "iso5", "iso6", "mp41", "mp42", "avc1", "dash", "M4V ":
			return true
		}
	}
	return false
}

// isWebM checks the EBML header's DocType. Matroska shares the EBML signature, so the
// signature alone would let a .mkv through wearing a .webm extension.
func isWebM(b []byte) bool {
	if !bytes.HasPrefix(b, ebmlSig) {
		return false
	}
	// The DocType string sits in the EBML header, well inside the first few dozen bytes.
	head := b
	if len(head) > 256 {
		head = head[:256]
	}
	return bytes.Contains(head, []byte("webm"))
}

// Sanitize sniffs the bytes, refuses anything outside the allowlist, and returns the stored
// form: metadata dropped, trailing bytes truncated. Video is returned as it arrived.
func Sanitize(b []byte) ([]byte, Kind, error) {
	kind, ok := Sniff(b)
	if !ok {
		return nil, Kind{}, reject("This file type is not allowed")
	}

	if kind.Video {
		// ponytail: containers pass through unchanged. Demuxing MP4 and WebM to rebuild them
		// is a second parser each for bytes no browser will execute; the sniffed content type,
		// nosniff and the separate CDN origin are the controls that matter here. Pipe uploads
		// through ffmpeg if that stops being enough.
		return b, kind, nil
	}

	out, w, h, err := stripImage(kind, b)
	if err != nil {
		return nil, Kind{}, err
	}
	if w <= 0 || h <= 0 {
		return nil, Kind{}, reject("This file does not look like a valid %s", kind.Ext)
	}
	if w*h > MaxPixels {
		return nil, Kind{}, reject("Images over %d megapixels are not allowed", MaxPixels/1_000_000)
	}
	return out, kind, nil
}

func stripImage(kind Kind, b []byte) (out []byte, w, h int, err error) {
	switch kind.ContentType {
	case "image/png":
		return stripPNG(b)
	case "image/jpeg":
		return stripJPEG(b)
	case "image/gif":
		return stripGIF(b)
	case "image/webp":
		return stripWebP(b)
	}
	return nil, 0, 0, reject("This file type is not allowed")
}

var errTruncated = errors.New("truncated")

// pngKeep is the chunk allowlist: the critical chunks, the ancillary ones that change what is
// rendered, and the APNG animation chunks. Everything else - tEXt, iTXt, zTXt, eXIf, iCCP,
// and anything unrecognised - is dropped. Chunks are copied byte for byte, so their CRCs stay
// valid and nothing has to be recomputed.
var pngKeep = map[string]bool{
	"IHDR": true, "PLTE": true, "IDAT": true, "IEND": true, "tRNS": true,
	"acTL": true, "fcTL": true, "fdAT": true,
	"gAMA": true, "cHRM": true, "sRGB": true,
}

func stripPNG(b []byte) ([]byte, int, int, error) {
	if len(b) < 8+25 {
		return nil, 0, 0, reject("This PNG is truncated")
	}
	out := make([]byte, 0, len(b))
	out = append(out, pngSig...)

	var w, h int
	i := 8
	for {
		if i+8 > len(b) {
			return nil, 0, 0, reject("This PNG is truncated")
		}
		length := int(binary.BigEndian.Uint32(b[i : i+4]))
		if length < 0 || i+12+length > len(b) {
			return nil, 0, 0, reject("This PNG is truncated")
		}
		name := string(b[i+4 : i+8])
		chunk := b[i : i+12+length]

		if name == "IHDR" {
			if length < 8 {
				return nil, 0, 0, reject("This PNG is malformed")
			}
			w = int(binary.BigEndian.Uint32(b[i+8 : i+12]))
			h = int(binary.BigEndian.Uint32(b[i+12 : i+16]))
			// Bail out before allocating anything on an absurd canvas.
			if w > 0 && h > 0 && w*h > MaxPixels {
				return nil, w, h, nil
			}
		}
		if pngKeep[name] {
			out = append(out, chunk...)
		}
		i += 12 + length

		if name == "IEND" {
			// Everything past IEND is trailing payload. It stops here.
			return out, w, h, nil
		}
	}
}

func stripJPEG(b []byte) ([]byte, int, int, error) {
	if len(b) < 4 {
		return nil, 0, 0, reject("This JPEG is truncated")
	}
	out := make([]byte, 0, len(b))
	out = append(out, 0xFF, 0xD8)

	var w, h int
	i := 2
	for {
		// Skip any fill bytes between segments.
		for i < len(b) && b[i] == 0xFF && i+1 < len(b) && b[i+1] == 0xFF {
			i++
		}
		if i+1 >= len(b) {
			return nil, 0, 0, reject("This JPEG is truncated")
		}
		if b[i] != 0xFF {
			return nil, 0, 0, reject("This JPEG is malformed")
		}
		marker := b[i+1]

		switch {
		case marker == 0xD9: // EOI - anything after it is trailing payload
			out = append(out, 0xFF, 0xD9)
			return out, w, h, nil
		case marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7): // standalone
			out = append(out, 0xFF, marker)
			i += 2
			continue
		}

		if i+4 > len(b) {
			return nil, 0, 0, reject("This JPEG is truncated")
		}
		length := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		if length < 2 || i+2+length > len(b) {
			return nil, 0, 0, reject("This JPEG is truncated")
		}
		payload := b[i+4 : i+2+length]

		// SOFn carries the dimensions. C4/C8/CC are DHT/JPG/DAC, not frame headers.
		if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
			if len(payload) >= 5 {
				h = int(binary.BigEndian.Uint16(payload[1:3]))
				w = int(binary.BigEndian.Uint16(payload[3:5]))
				if w > 0 && h > 0 && w*h > MaxPixels {
					return nil, w, h, nil
				}
			}
		}

		// APPn holds EXIF, XMP, ICC and every other metadata blob; COM holds comments.
		drop := (marker >= 0xE0 && marker <= 0xEF) || marker == 0xFE
		if !drop {
			out = append(out, b[i:i+2+length]...)
		}
		i += 2 + length

		if marker == 0xDA { // SOS - entropy-coded data follows, outside the segment structure
			end, err := jpegScanEnd(b, i)
			if err != nil {
				return nil, 0, 0, reject("This JPEG is truncated")
			}
			out = append(out, b[i:end]...)
			// A progressive JPEG has several scans, so this is not necessarily the end of the
			// file - go back round and let the loop find the next marker. Treating the first
			// scan as final would silently truncate every progressive image.
			i = end
		}
	}
}

// jpegScanEnd returns the offset of the EOI marker that closes the entropy-coded scan. Inside
// that data a literal 0xFF is stuffed as FF00, and FFD0-FFD7 are restart markers, so the first
// FFD9 that is neither is the real end of image.
func jpegScanEnd(b []byte, i int) (int, error) {
	for ; i+1 < len(b); i++ {
		if b[i] != 0xFF {
			continue
		}
		next := b[i+1]
		if next == 0x00 || next == 0xFF || (next >= 0xD0 && next <= 0xD7) {
			continue
		}
		// Either EOI or the start of the next segment; both end this scan.
		return i, nil
	}
	return 0, errTruncated
}

func stripGIF(b []byte) ([]byte, int, int, error) {
	if len(b) < 13 {
		return nil, 0, 0, reject("This GIF is truncated")
	}
	w := int(binary.LittleEndian.Uint16(b[6:8]))
	h := int(binary.LittleEndian.Uint16(b[8:10]))
	if w > 0 && h > 0 && w*h > MaxPixels {
		return nil, w, h, nil
	}

	out := make([]byte, 0, len(b))
	i := 13
	packed := b[10]
	if packed&0x80 != 0 { // global colour table
		i += 3 * (1 << ((packed & 0x07) + 1))
	}
	if i > len(b) {
		return nil, 0, 0, reject("This GIF is truncated")
	}
	out = append(out, b[:i]...)

	for {
		if i >= len(b) {
			return nil, 0, 0, reject("This GIF is truncated")
		}
		switch b[i] {
		case 0x3B: // trailer - anything after it is trailing payload
			out = append(out, 0x3B)
			return out, w, h, nil

		case 0x21: // extension
			if i+2 > len(b) {
				return nil, 0, 0, reject("This GIF is truncated")
			}
			label := b[i+1]
			end, err := gifSubBlocksEnd(b, i+2)
			if err != nil {
				return nil, 0, 0, reject("This GIF is truncated")
			}
			// Keep graphic control (timing and transparency) and the NETSCAPE2.0 application
			// extension, which is what makes an animation loop. Comment (FE), plain text (01)
			// and every other application extension - XMP and ICC ride in here - are dropped.
			keep := label == 0xF9 ||
				(label == 0xFF && i+3+11 <= len(b) && bytes.Equal(b[i+3:i+14], []byte("NETSCAPE2.0")))
			if keep {
				out = append(out, b[i:end]...)
			}
			i = end

		case 0x2C: // image descriptor
			if i+10 > len(b) {
				return nil, 0, 0, reject("This GIF is truncated")
			}
			j := i + 10
			lp := b[i+9]
			if lp&0x80 != 0 { // local colour table
				j += 3 * (1 << ((lp & 0x07) + 1))
			}
			if j >= len(b) {
				return nil, 0, 0, reject("This GIF is truncated")
			}
			j++ // LZW minimum code size
			end, err := gifSubBlocksEnd(b, j)
			if err != nil {
				return nil, 0, 0, reject("This GIF is truncated")
			}
			out = append(out, b[i:end]...)
			i = end

		default:
			return nil, 0, 0, reject("This GIF is malformed")
		}
	}
}

// gifSubBlocksEnd walks a chain of length-prefixed sub-blocks and returns the offset just past
// its terminating zero byte.
func gifSubBlocksEnd(b []byte, i int) (int, error) {
	for {
		if i >= len(b) {
			return 0, errTruncated
		}
		n := int(b[i])
		if n == 0 {
			return i + 1, nil
		}
		i += 1 + n
	}
}

// webpKeep is the chunk allowlist. EXIF, XMP and ICCP are dropped; everything that carries
// pixels, alpha or animation stays.
var webpKeep = map[string]bool{
	"VP8 ": true, "VP8L": true, "VP8X": true, "ALPH": true, "ANIM": true, "ANMF": true,
}

func stripWebP(b []byte) ([]byte, int, int, error) {
	if len(b) < 12 {
		return nil, 0, 0, reject("This WebP is truncated")
	}
	riffSize := int(binary.LittleEndian.Uint32(b[4:8]))
	end := 8 + riffSize
	if riffSize <= 0 || end > len(b) {
		end = len(b) // tolerate a wrong size field; the chunk walk is the real bound
	}

	body := make([]byte, 0, len(b))
	var w, h int
	i := 12
	for i+8 <= end {
		name := string(b[i : i+4])
		size := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		if size < 0 || i+8+size > end {
			break
		}
		payload := b[i+8 : i+8+size]
		padded := size + size%2

		switch name {
		case "VP8X":
			if len(payload) >= 10 {
				// Clear the ICC (0x20), EXIF (0x08) and XMP (0x04) flags, since those chunks
				// are about to be dropped and a viewer should not go looking for them.
				chunk := append([]byte(nil), b[i:i+8+size]...)
				chunk[8] &^= 0x20 | 0x08 | 0x04
				w = int(payload[4]) | int(payload[5])<<8 | int(payload[6])<<16
				h = int(payload[7]) | int(payload[8])<<8 | int(payload[9])<<16
				w, h = w+1, h+1
				body = append(body, chunk...)
				if size%2 == 1 {
					body = append(body, 0)
				}
			}
		case "VP8 ":
			if len(payload) >= 10 && payload[3] == 0x9D && payload[4] == 0x01 && payload[5] == 0x2A {
				if w == 0 {
					w = int(binary.LittleEndian.Uint16(payload[6:8]) & 0x3FFF)
					h = int(binary.LittleEndian.Uint16(payload[8:10]) & 0x3FFF)
				}
			}
			body = appendChunk(body, b[i:i+8+size], size)
		case "VP8L":
			if len(payload) >= 5 && payload[0] == 0x2F {
				bits := binary.LittleEndian.Uint32(payload[1:5])
				if w == 0 {
					w = int(bits&0x3FFF) + 1
					h = int((bits>>14)&0x3FFF) + 1
				}
			}
			body = appendChunk(body, b[i:i+8+size], size)
		default:
			if webpKeep[name] {
				body = appendChunk(body, b[i:i+8+size], size)
			}
		}
		i += 8 + padded
	}

	if len(body) == 0 {
		return nil, 0, 0, reject("This WebP has no image data")
	}
	if w > 0 && h > 0 && w*h > MaxPixels {
		return nil, w, h, nil
	}

	out := make([]byte, 0, 12+len(body))
	out = append(out, 'R', 'I', 'F', 'F')
	out = binary.LittleEndian.AppendUint32(out, uint32(4+len(body))) // "WEBP" plus the chunks
	out = append(out, 'W', 'E', 'B', 'P')
	out = append(out, body...)
	return out, w, h, nil
}

func appendChunk(dst, chunk []byte, size int) []byte {
	dst = append(dst, chunk...)
	if size%2 == 1 {
		dst = append(dst, 0) // RIFF chunks are padded to an even length
	}
	return dst
}
