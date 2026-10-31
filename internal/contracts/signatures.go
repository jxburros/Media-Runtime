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
// 0.5.0; the frame archive and the optional encoded video from 0.9.0.
var Signatures = map[string]Signature{
	"image/png":       pngSignature,
	"application/zip": frameArchiveSignature,
	"video/mp4":       mp4Signature,
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
