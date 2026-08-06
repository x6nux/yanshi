package tui

import (
	"testing"

	"github.com/x6nux/yanshi/internal/proto"
	"github.com/x6nux/yanshi/internal/tools"
)

func TestAttachPathDetectsImageExtension(t *testing.T) {
	cases := map[string]bool{
		"shot.png": true, "pic.JPG": true, "a.webp": true, "anim.gif": true,
		"readme.md": false, "main.go": false, "data.json": false, "noext": false,
	}
	for path, want := range cases {
		if got := tools.IsImagePath(path); got != want {
			t.Errorf("IsImagePath(%q) = %v want %v", path, got, want)
		}
	}
}

func TestBuildUserMessageFrameIncludesImages(t *testing.T) {
	fr := buildSendFrame("look", []proto.ImageAttach{{ID: "img-1", Fmt: "png", DataB64: "AA"}}, nil)
	if len(fr.Images) != 1 || fr.Images[0].ID != "img-1" {
		t.Fatalf("frame images = %#v", fr.Images)
	}
	if fr.Text != "look" {
		t.Fatalf("text = %q", fr.Text)
	}
}

func TestBuildUserMessageFrameOmitsImagesWhenNone(t *testing.T) {
	fr := buildSendFrame("text only", nil, nil)
	if len(fr.Images) != 0 {
		t.Fatalf("text-only frame must not carry images: %#v", fr)
	}
}
