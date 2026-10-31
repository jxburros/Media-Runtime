package contracts

import (
	"archive/zip"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

// Signature verifies an output's committed bytes against its MIME type and the worker's output
// record without decoding media: the container's structure and the facts the record states.
type Signature func(data io.ReaderAt, size int64, record *jsonv.Object) bool

// Signatures are the owned format checks, one per verifiable MIME type. PNG is checked from
// 0.5.0, the frame archive and the optional encoded video from 0.9.0, WAV from 0.10.0.
var Signatures = map[string]Signature{
	"image/png":       pngSignature,
	"application/zip": frameArchiveSignature,
	"video/mp4":       mp4Signature,
	"audio/wav":       wavSignature,
}

func readAt(data io.ReaderAt, offset, n int64) ([]byte, bool) {
	b := make([]byte, n)
	read, err := data.ReadAt(b, offset)
	return b, int64(read) == n && (err == nil || err == io.EOF)
}

// pngHeader is a PNG's signature and IHDR dimensions.
func pngHeader(head []byte) (float64, float64, bool) {
	if len(head) < 24 || string(head[:8]) != "\x89PNG\r\n\x1a\n" || string(head[12:16]) != "IHDR" {
		return 0, 0, false
	}
	return float64(binary.BigEndian.Uint32(head[16:20])), float64(binary.BigEndian.Uint32(head[20:24])), true
}

func pngSignature(data io.ReaderAt, size int64, record *jsonv.Object) bool {
	head, ok := readAt(data, 0, 24)
	if !ok {
		return false
	}
	width, height, ok := pngHeader(head)
	w, _ := record.Num("width")
	h, _ := record.Num("height")
	return ok && width == w && height == h
}

// MaxFrameManifest bounds frames.json; the protocol's manifest is four small integers.
const MaxFrameManifest = 4096

// frameArchiveSignature checks an image-to-video frame archive: a ZIP local-file signature at
// offset 0, a readable central directory, a frames.json manifest of exactly fps, width, height
// and frames (positive integers) equal to the output record, exactly the members 000001.png …
// one per frame beside it, each a PNG of the declared size, and the record's duration equal to
// frames / fps. Members are not decompressed beyond their PNG headers.
func frameArchiveSignature(data io.ReaderAt, size int64, record *jsonv.Object) bool {
	if head, ok := readAt(data, 0, 4); !ok || string(head) != "PK\x03\x04" {
		return false
	}
	archive, err := zip.NewReader(data, size)
	if err != nil {
		return false
	}
	members := map[string]*zip.File{}
	for _, f := range archive.File {
		if members[f.Name] != nil {
			return false
		}
		members[f.Name] = f
	}
	manifest := members["frames.json"]
	if manifest == nil || manifest.UncompressedSize64 > MaxFrameManifest {
		return false
	}
	facts, ok := frameManifest(manifest)
	if !ok {
		return false
	}
	for _, k := range []string{"frames", "fps", "width", "height"} {
		if v, ok := record.Num(k); !ok || v != facts[k] {
			return false
		}
	}
	if d, ok := record.Num("duration_seconds"); !ok || d != facts["frames"]/facts["fps"] {
		return false
	}
	if float64(len(members)) != facts["frames"]+1 {
		return false
	}
	for k := 1; k <= int(facts["frames"]); k++ {
		frame := members[fmt.Sprintf("%06d.png", k)]
		if frame == nil {
			return false
		}
		stream, err := frame.Open()
		if err != nil {
			return false
		}
		head := make([]byte, 24)
		_, err = io.ReadFull(stream, head)
		stream.Close()
		width, height, ok := pngHeader(head)
		if err != nil || !ok || width != facts["width"] || height != facts["height"] {
			return false
		}
	}
	return true
}

// frameManifest reads frames.json strictly: an object of exactly fps, width, height and frames.
func frameManifest(f *zip.File) (map[string]float64, bool) {
	stream, err := f.Open()
	if err != nil {
		return nil, false
	}
	defer stream.Close()
	raw, err := io.ReadAll(io.LimitReader(stream, MaxFrameManifest+1))
	if err != nil || len(raw) > MaxFrameManifest {
		return nil, false
	}
	parsed, err := jsonv.Parse(raw)
	manifest, isObject := parsed.(*jsonv.Object)
	if err != nil || !isObject || manifest.Len() != 4 {
		return nil, false
	}
	facts := map[string]float64{}
	for _, k := range []string{"fps", "width", "height", "frames"} {
		v, ok := manifest.Num(k)
		if !ok || v < 1 || v != math.Trunc(v) || v > math.MaxUint32 {
			return nil, false
		}
		facts[k] = v
	}
	return facts, true
}

// mp4Signature checks an ISO base media file: a first box of type ftyp at offset 4 whose size
// (32-bit, or the 64-bit large size when it is 1) fits the file.
func mp4Signature(data io.ReaderAt, size int64, record *jsonv.Object) bool {
	head, ok := readAt(data, 0, 16)
	if !ok || string(head[4:8]) != "ftyp" {
		return false
	}
	box := uint64(binary.BigEndian.Uint32(head[:4]))
	if box == 1 {
		box = binary.BigEndian.Uint64(head[8:16])
	}
	return box >= 8 && box <= uint64(size)
}

// wavSignature checks the canonical WAV the protocol specifies: a 44-byte RIFF/WAVE header whose
// RIFF size is the file size less eight, a 16-byte "fmt " chunk of 16-bit PCM with a consistent
// block alignment and byte rate, then one "data" chunk holding a whole, non-zero number of
// sample frames up to the end of the file. The record's sample rate, channels, samples and
// duration (samples / sample_rate) must be the header's.
func wavSignature(data io.ReaderAt, size int64, record *jsonv.Object) bool {
	head, ok := readAt(data, 0, 44)
	if !ok {
		return false
	}
	le16 := func(i int) uint64 { return uint64(binary.LittleEndian.Uint16(head[i:])) }
	le32 := func(i int) uint64 { return uint64(binary.LittleEndian.Uint32(head[i:])) }
	if string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" || string(head[12:16]) != "fmt " || string(head[36:40]) != "data" {
		return false
	}
	format, channels, rate, byteRate, align, bits := le16(20), le16(22), le32(24), le32(28), le16(32), le16(34)
	dataSize := le32(40)
	if le32(16) != 16 || format != 1 || bits != 16 || channels < 1 || rate < 1 || align != channels*2 || byteRate != rate*align {
		return false
	}
	if le32(4) != uint64(size)-8 || dataSize == 0 || dataSize%align != 0 || uint64(size) != 44+dataSize {
		return false
	}
	samples := dataSize / align
	facts := map[string]float64{"sample_rate": float64(rate), "channels": float64(channels), "samples": float64(samples)}
	for k, want := range facts {
		if v, ok := record.Num(k); !ok || v != want {
			return false
		}
	}
	d, ok := record.Num("duration_seconds")
	return ok && d == float64(samples)/float64(rate)
}
