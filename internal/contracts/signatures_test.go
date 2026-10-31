package contracts

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/jxburros/media-runtime/internal/jsonv"
)

// png24 is the first 24 bytes of a PNG: its signature and IHDR dimensions.
func png24(width, height int) []byte {
	b := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR")
	return append(b, byte(width>>24), byte(width>>16), byte(width>>8), byte(width), byte(height>>24), byte(height>>16), byte(height>>8), byte(height))
}

type member struct {
	name string
	data []byte
}

func archive(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, m := range members {
		f, err := w.CreateHeader(&zip.FileHeader{Name: m.name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(m.data)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func frames(n int, manifest string) []member {
	out := []member{{"frames.json", []byte(manifest)}}
	for k := 1; k <= n; k++ {
		out = append(out, member{fmt.Sprintf("%06d.png", k), png24(64, 64)})
	}
	return out
}

func check(mime string, data []byte, record *jsonv.Object) bool {
	return Signatures[mime](bytes.NewReader(data), int64(len(data)), record)
}

func TestFrameArchiveSignature(t *testing.T) {
	record := func() *jsonv.Object {
		return jsonv.O("output_id", "frames", "width", 64.0, "height", 64.0, "frames", 2.0, "fps", 7.0, "duration_seconds", 2.0/7)
	}
	manifest := `{"fps": 7, "frames": 2, "height": 64, "width": 64}`
	good := archive(t, frames(2, manifest)...)
	if !check("application/zip", good, record()) {
		t.Fatal("a valid frame archive was refused")
	}
	cases := map[string][]byte{
		"truncated central directory": good[:len(good)-10],
		"no frames.json":              archive(t, frames(2, manifest)[1:]...),
		"a missing frame":             archive(t, frames(1, manifest)...),
		"an extra member":             archive(t, append(frames(2, manifest), member{"notes.txt", []byte("x")})...),
		"an unknown manifest field":   archive(t, frames(2, `{"fps":7,"frames":2,"height":64,"width":64,"codec":"x"}`)...),
		"a fractional rate":           archive(t, frames(2, `{"fps":7.5,"frames":2,"height":64,"width":64}`)...),
		"a frame of another size":     archive(t, member{"frames.json", []byte(manifest)}, member{"000001.png", png24(64, 64)}, member{"000002.png", png24(32, 64)}),
		"not a zip":                   append([]byte("PK\x05\x06"), good[4:]...),
	}
	for name, data := range cases {
		if check("application/zip", data, record()) {
			t.Fatalf("%s was accepted", name)
		}
	}
	for field, value := range map[string]float64{"frames": 3, "fps": 8, "width": 32, "duration_seconds": 0.25} {
		if check("application/zip", good, record().Set(field, value)) {
			t.Fatalf("a record with %s %v was accepted", field, value)
		}
	}
}

func TestMP4AndPNGSignatures(t *testing.T) {
	mp4 := append([]byte("\x00\x00\x00\x14ftypisom\x00\x00\x02\x00isom"), []byte("\x00\x00\x00\x08mdat")...)
	if !check("video/mp4", mp4, jsonv.NewObject()) {
		t.Fatal("a valid ftyp box was refused")
	}
	for name, data := range map[string][]byte{
		"no ftyp":        append([]byte("\x00\x00\x00\x14moovisom\x00\x00\x02\x00isom"), mp4[20:]...),
		"oversized box":  append([]byte("\x00\x00\x01\x00ftypisom\x00\x00\x02\x00isom"), mp4[20:]...),
		"too short":      mp4[:12],
		"undersized box": append([]byte("\x00\x00\x00\x04ftypisom\x00\x00\x02\x00isom"), mp4[20:]...),
	} {
		if check("video/mp4", data, jsonv.NewObject()) {
			t.Fatalf("%s was accepted", name)
		}
	}
	if !check("image/png", png24(64, 32), jsonv.O("width", 64.0, "height", 32.0)) || check("image/png", png24(64, 32), jsonv.O("width", 64.0, "height", 64.0)) {
		t.Fatal("PNG dimensions are not checked")
	}
}

func TestVideoRecipesAndBindings(t *testing.T) {
	parse := func(text string) error {
		raw, err := jsonv.Parse([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		_, err = Validate(RecipeSchema, raw)
		return err
	}
	for _, r := range DefaultRecipes() {
		if r.Str("recipe_id") == "animate" && (r.Str("pipeline") != "image-to-video" || r.Str("model_family") != "svd") {
			t.Fatal(jsonv.MustMarshal(r))
		}
	}
	// A schema-1 video recipe takes an image and no prompt, and has no guidance or adapters.
	for body, field := range map[string]string{
		`{"schema_version":1,"recipe_id":"a","revision":1,"pipeline":"image-to-video","model_family":"svd","inputs":{"image":{"type":"image","required":true}},"defaults":{"frames":14,"fps":6}}`:                         "",
		`{"schema_version":1,"recipe_id":"a","revision":1,"pipeline":"image-to-video","model_family":"svd","inputs":{"image":{"type":"image","required":true},"prompt":{"type":"string","required":true}},"defaults":{}}`: "inputs",
		`{"schema_version":1,"recipe_id":"a","revision":1,"pipeline":"image-to-video","model_family":"svd","inputs":{"image":{"type":"image","required":true}},"defaults":{"guidance":3}}`:                                "inputs",
		`{"schema_version":1,"recipe_id":"a","revision":1,"pipeline":"image-to-video","model_family":"sdxl-base","inputs":{"image":{"type":"image","required":true}},"defaults":{}}`:                                      "inputs",
		`{"schema_version":1,"recipe_id":"a","revision":1,"pipeline":"text-to-image","model_family":"sdxl-base","inputs":{"prompt":{"type":"string","required":true}},"defaults":{"frames":14}}`:                          "inputs",
	} {
		err := parse(body)
		if field == "" && err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if api, ok := err.(*ApiError); field != "" && (!ok || api.Field != field) {
			t.Fatalf("%s: %v", body, err)
		}
	}
	// The poster is a PNG and may feed a later image input; the archive may not.
	linear := `{"schema_version":2,"recipe_id":"l","revision":1,"inputs":{"image":{"type":"image"}},"operations":[
	  {"op_id":"animate","task":"image-to-video","model_family":"svd","inputs":{"image":{"from":"input:image"}},"defaults":{"steps":4}},
	  {"op_id":"enlarge","task":"upscale","model_family":"esrgan","inputs":{"image":{"from":"op:animate/%s"}}}]}`
	if err := parse(fmt.Sprintf(linear, "poster")); err != nil {
		t.Fatal(err)
	}
	if err := parse(fmt.Sprintf(linear, "frames")); err == nil || err.(*ApiError).Field != "operations.1.inputs.image.from" {
		t.Fatalf("an archive bound to an image input: %v", err)
	}
}

// wav16 is a canonical 16-bit PCM WAV of silence.
func wav16(rate, channels, samples int) []byte {
	data := samples * channels * 2
	h := make([]byte, 44)
	copy(h, "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+data))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(h[32:], uint16(channels*2))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(data))
	return append(h, make([]byte, data)...)
}

func TestWAVSignature(t *testing.T) {
	record := func() *jsonv.Object {
		return jsonv.O("output_id", "audio", "sample_rate", 8000.0, "channels", 2.0, "samples", 8192.0, "duration_seconds", 1.024)
	}
	good := wav16(8000, 2, 8192)
	if !check("audio/wav", good, record()) {
		t.Fatal("a canonical WAV was refused")
	}
	mutate := func(at int, put func(b []byte)) []byte {
		b := append([]byte(nil), good...)
		put(b[at:])
		return b
	}
	cases := map[string][]byte{
		"truncated":      good[:len(good)-2],
		"trailing bytes": append(append([]byte(nil), good...), 0, 0, 0, 0),
		"not RIFF":       mutate(0, func(b []byte) { copy(b, "RIFX") }),
		"not PCM":        mutate(20, func(b []byte) { binary.LittleEndian.PutUint16(b, 3) }),
		"24-bit":         mutate(34, func(b []byte) { binary.LittleEndian.PutUint16(b, 24) }),
		"bad byte rate":  mutate(28, func(b []byte) { binary.LittleEndian.PutUint32(b, 1) }),
		"no data chunk":  mutate(36, func(b []byte) { copy(b, "LIST") }),
		"empty":          wav16(8000, 2, 0),
		"short header":   good[:40],
	}
	for name, data := range cases {
		if check("audio/wav", data, record()) {
			t.Fatalf("%s was accepted", name)
		}
	}
	for field, value := range map[string]float64{"sample_rate": 16000, "channels": 1, "samples": 8191, "duration_seconds": 1} {
		if check("audio/wav", good, record().Set(field, value)) {
			t.Fatalf("a record with %s %v was accepted", field, value)
		}
	}
}

func TestAudioRecipes(t *testing.T) {
	parse := func(text string) error {
		raw, err := jsonv.Parse([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		_, err = Validate(RecipeSchema, raw)
		return err
	}
	found := false
	for _, r := range DefaultRecipes() {
		if r.Str("recipe_id") == "soundscape" {
			found = r.Str("pipeline") == "text-to-audio" && r.Str("model_family") == "stable-audio-open"
		}
	}
	if !found {
		t.Fatal("no soundscape recipe")
	}
	for body, ok := range map[string]bool{
		`{"schema_version":1,"recipe_id":"a","revision":1,"pipeline":"text-to-audio","model_family":"stable-audio-open","inputs":{"prompt":{"type":"string","required":true}},"defaults":{"steps":200,"duration_seconds":30,"guidance":5}}`: true,
		`{"schema_version":1,"recipe_id":"a","revision":1,"pipeline":"text-to-audio","model_family":"stable-audio-open","inputs":{"prompt":{"type":"string","required":true}},"defaults":{"width":64}}`:                                     false,
		`{"schema_version":1,"recipe_id":"a","revision":1,"pipeline":"text-to-audio","model_family":"stable-audio-open","inputs":{"prompt":{"type":"string","required":true},"image":{"type":"image","required":true}},"defaults":{}}`:      false,
		`{"schema_version":1,"recipe_id":"a","revision":1,"pipeline":"text-to-audio","model_family":"stable-audio-open","inputs":{"prompt":{"type":"string","required":true}},"defaults":{"steps":201}}`:                                    false,
		// The wider audio step range does not widen the image tasks'.
		`{"schema_version":1,"recipe_id":"a","revision":1,"pipeline":"text-to-image","model_family":"sdxl-base","inputs":{"prompt":{"type":"string","required":true}},"defaults":{"steps":175}}`:          false,
		`{"schema_version":1,"recipe_id":"a","revision":1,"pipeline":"text-to-image","model_family":"sdxl-base","inputs":{"prompt":{"type":"string","required":true}},"defaults":{"duration_seconds":5}}`: false,
	} {
		if err := parse(body); (err == nil) != ok {
			t.Fatalf("%s: %v", body, err)
		}
	}
	// An audio output can be published but never bound to an image input.
	linear := `{"schema_version":2,"recipe_id":"l","revision":1,"inputs":{"prompt":{"type":"string"}},"operations":[
	  {"op_id":"sound","task":"text-to-audio","model_family":"stable-audio-open","inputs":{"prompt":{"from":"input:prompt"}},"defaults":{"steps":4,"duration_seconds":2}},
	  {"op_id":"enlarge","task":"upscale","model_family":"esrgan","inputs":{"image":{"from":"op:sound/audio"}}}]}`
	if err := parse(linear); err == nil {
		t.Fatal("an audio output was bound to an image input")
	}
}
