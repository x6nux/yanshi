package tools

import (
	"testing"
)

func TestDetectFmt(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"image.png", "png"},
		{"subdir/photo.PNG", "png"},
		{"animation.gif", "gif"},
		{"image.GIF", "gif"},
		{"webp/file.webp", "webp"},
		{"photo.WEBP", "webp"},
		{"photo.jpg", "jpeg"},
		{"photo.jpeg", "jpeg"},
		{"photo.JPEG", "jpeg"},
		{"noextension", "jpeg"},
		{"file.txt", "jpeg"},
		{"", "jpeg"},
	}
	for _, tc := range tests {
		got := detectFmt(tc.path)
		if got != tc.want {
			t.Errorf("detectFmt(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestMimeForFmt(t *testing.T) {
	tests := []struct {
		fmt  string
		want string
	}{
		{"png", "image/png"},
		{"gif", "image/gif"},
		{"webp", "image/webp"},
		{"jpeg", "image/jpeg"},
		{"jpg", "image/jpeg"}, // default case
		{"unknown", "image/jpeg"},
		{"", "image/jpeg"},
	}
	for _, tc := range tests {
		got := mimeForFmt(tc.fmt)
		if got != tc.want {
			t.Errorf("mimeForFmt(%q) = %q, want %q", tc.fmt, got, tc.want)
		}
	}
}
