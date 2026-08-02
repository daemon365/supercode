package attachment

import (
	"encoding/base64"
	"testing"
)

func TestFromBytesLoadsPNG(t *testing.T) {
	content, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Wl2nWQAAAAASUVORK5CYII=")
	image, err := FromBytes(content, "high")
	if err != nil {
		t.Fatal(err)
	}
	if image.MIMEType != "image/png" || image.Data == "" {
		t.Fatalf("image = %#v", image)
	}
}
