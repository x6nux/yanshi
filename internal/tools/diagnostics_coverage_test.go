package tools

import (
	"context"
	"testing"
)

func TestDefaultFileLister(t *testing.T) {
	var l defaultFileLister
	files := l.recentFiles(context.Background(), "/some/root")
	if files != nil {
		t.Fatalf("defaultFileLister should return nil, got %v", files)
	}
}

func TestRecentFilesOverride(t *testing.T) {
	// Save and restore override.
	saved := diagFileListerOverride
	defer func() { diagFileListerOverride = saved }()

	myFiles := []string{"file1.go", "file2.go"}
	diagFileListerOverride = mockFileLister{files: myFiles}

	got := diagFileListerOverride.recentFiles(context.Background(), "/root")
	if len(got) != 2 || got[0] != "file1.go" || got[1] != "file2.go" {
		t.Fatalf("unexpected files: %v", got)
	}
}

type mockFileLister struct {
	files []string
}

func (m mockFileLister) recentFiles(_ context.Context, _ string) []string {
	return m.files
}
