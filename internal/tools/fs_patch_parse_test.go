package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePatch_AddUpdateDeleteMove(t *testing.T) {
	src := "*** Begin Patch\n" +
		"*** Add File: new.txt\n" +
		"hello\n" +
		"world\n" +
		"*** Update File: app.go\n" +
		" line1\n" +
		"-func A() {}\n" +
		"+func A() { return }\n" +
		" line3\n" +
		"*** Delete File: old.txt\n" +
		"*** Move File: src/a.go\n" +
		"*** To: src/b.go\n" +
		"*** End Patch"
	ops, err := parsePatch(src)
	require.NoError(t, err)
	require.Len(t, ops, 4)

	assert.Equal(t, opAdd, ops[0].kind)
	assert.Equal(t, "new.txt", ops[0].path)
	assert.Equal(t, "hello\nworld", ops[0].addBody)

	assert.Equal(t, opUpdate, ops[1].kind)
	assert.Equal(t, "app.go", ops[1].path)
	assert.Equal(t, "line1\nfunc A() {}\nline3", ops[1].updOld)
	assert.Equal(t, "line1\nfunc A() { return }\nline3", ops[1].updNew)

	assert.Equal(t, opDelete, ops[2].kind)
	assert.Equal(t, "old.txt", ops[2].path)

	assert.Equal(t, opMove, ops[3].kind)
	assert.Equal(t, "src/a.go", ops[3].from)
	assert.Equal(t, "src/b.go", ops[3].path)
}

func TestParsePatch_UpdatePureInsert(t *testing.T) {
	// Pure insertion (no removal): old=context, new=context+add.
	src := "*** Begin Patch\n*** Update File: f.go\n line1\n+inserted\n*** End Patch"
	ops, err := parsePatch(src)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	assert.Equal(t, "line1", ops[0].updOld)
	assert.Equal(t, "line1\ninserted", ops[0].updNew)
}

func TestParsePatch_UpdateBlankContextLine(t *testing.T) {
	// A blank line inside an Update body is a context line with empty text.
	src := "*** Begin Patch\n*** Update File: f.go\n a\n\n-b\n+c\n*** End Patch"
	ops, err := parsePatch(src)
	require.NoError(t, err)
	assert.Equal(t, "a\n\nb", ops[0].updOld, "blank line preserved as context")
	assert.Equal(t, "a\n\nc", ops[0].updNew)
}

func TestParsePatch_Errors(t *testing.T) {
	tests := []struct {
		name  string
		patch string
	}{
		{"missing envelope", "*** Add File: x\nhi\n*** End Patch"},
		{"missing end", "*** Begin Patch\n*** Add File: x\nhi\n"},
		{"empty add path", "*** Begin Patch\n*** Add File: \nhi\n*** End Patch"},
		{"move missing To", "*** Begin Patch\n*** Move File: a\n*** End Patch"},
		{"move empty dest", "*** Begin Patch\n*** Move File: a\n*** To: \n*** End Patch"},
		{"bad update line", "*** Begin Patch\n*** Update File: f\nbadline\n*** End Patch"},
		{"empty update body", "*** Begin Patch\n*** Update File: f\n*** End Patch"},
		{"stray line", "*** Begin Patch\nhello\n*** End Patch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePatch(tc.patch)
			assert.Error(t, err)
		})
	}
}

func TestParsePatch_EmptyHasNoOpsButIsValid(t *testing.T) {
	ops, err := parsePatch("*** Begin Patch\n*** End Patch")
	require.NoError(t, err)
	assert.Empty(t, ops)
}
