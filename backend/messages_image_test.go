package main

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"rykvo.local/auth/internal/mms"
)

func TestMessageImageFormatsAndInclusiveLimit(t *testing.T) {
	canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
	canvas.Set(0, 0, color.RGBA{255, 0, 0, 255})
	palette := color.Palette{color.Black, color.White}
	frame := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	for _, kind := range []string{"jpeg", "png", "gif"} {
		t.Run(kind, func(t *testing.T) {
			var data bytes.Buffer
			var err error
			switch kind {
			case "jpeg":
				err = jpeg.Encode(&data, canvas, nil)
			case "png":
				err = png.Encode(&data, canvas)
			case "gif":
				err = gif.EncodeAll(&data, &gif.GIF{Image: []*image.Paletted{frame, frame}, Delay: []int{10, 20}, LoopCount: 0})
			}
			if err != nil {
				t.Fatal(err)
			}
			original := append([]byte(nil), data.Bytes()...)
			encode := func(kind string, data []byte) string {
				return "data:image/" + kind + ";base64," + base64.StdEncoding.EncodeToString(data)
			}
			part, err := messageImage(encode(kind, original))
			if err != nil || part.Type != "image/"+kind || !bytes.Equal(part.Data, original) {
				t.Fatal("image changed", err)
			}
			wire, err := mms.SendRequest("image-format-test", "+12025550123", "图片说明", part)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := mms.Parse(wire)
			if err != nil || len(parsed.Parts) != 2 || string(parsed.Parts[0].Data) != "图片说明" || !bytes.Equal(parsed.Parts[1].Data, original) {
				t.Fatal("MMS altered attachment or text", err)
			}
			full := append(append([]byte(nil), original...), make([]byte, 1024*1024-len(original))...)
			if _, err = messageImage(encode(kind, full)); err != nil {
				t.Fatal("exact 1 MiB rejected", err)
			}
			if _, err = messageImage(encode(kind, append(full, 0))); err == nil || err.Error() != "MMS_TOO_LARGE" {
				t.Fatal("size boundary", err)
			}
			other := "png"
			if kind == other {
				other = "jpeg"
			}
			if _, err = messageImage(encode(other, original)); err == nil {
				t.Fatal("MIME mismatch accepted")
			}
		})
	}
}

func TestMessageImageRejectsUnsupportedAndInvalidData(t *testing.T) {
	for _, raw := range []string{"data:image/webp;base64,UklGRg==", "data:image/svg+xml;base64,PHN2Zz4=", "data:video/mp4;base64,YQ==", "data:image/png;base64,", "data:image/jpeg;base64,!!!", "data:image/gif;base64,R0lGODlh"} {
		if _, err := messageImage(raw); err == nil {
			t.Fatal(raw)
		}
	}
	if part, err := messageImage(""); part != nil || err != nil {
		t.Fatal(part, err)
	}
	if _, err := mms.SendRequest("image-format-test", "+12025550123", "", &mms.Part{Type: "image/webp", Data: []byte("RIFF")}); err == nil {
		t.Fatal("WebP still allowed by outbound codec")
	}
}
