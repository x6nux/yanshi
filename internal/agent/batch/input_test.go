package batch_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/x6nux/yanshi/internal/agent/batch"
)

func TestParseCSV_HeaderAndStableIndex(t *testing.T) {
	input := "name,city\nAlice,NYC\nBob,SF\n"
	rows, err := batch.ParseCSV(input)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, 0, rows[0].Index)
	assert.Equal(t, "Alice", rows[0].Values["name"])
	assert.Equal(t, 1, rows[1].Index)
	assert.Equal(t, "SF", rows[1].Values["city"])
}

func TestParseCSV_QuotedCommaAndNewline(t *testing.T) {
	input := "desc,note\n\"line1\nline2\",\"has, comma\"\n"
	rows, err := batch.ParseCSV(input)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "line1\nline2", rows[0].Values["desc"])
	assert.Equal(t, "has, comma", rows[0].Values["note"])
}

func TestParseCSV_UTF8Preserved(t *testing.T) {
	input := "name\n张三\n李四\n"
	rows, err := batch.ParseCSV(input)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "张三", rows[0].Values["name"])
	assert.Equal(t, "李四", rows[1].Values["name"])
}

func TestParseCSV_EmptyString(t *testing.T) {
	_, err := batch.ParseCSV("")
	require.Error(t, err)
}

func TestParseCSV_HeaderOnly(t *testing.T) {
	_, err := batch.ParseCSV("name,city\n")
	require.Error(t, err)
}

func TestParseCSV_DuplicateHeader(t *testing.T) {
	_, err := batch.ParseCSV("name,name\na,b\n")
	require.Error(t, err)
}

func TestParseCSV_EmptyHeaderCell(t *testing.T) {
	_, err := batch.ParseCSV("name,\na,b\n")
	require.Error(t, err)
}

func TestParseCSV_RowFieldCountMismatch(t *testing.T) {
	_, err := batch.ParseCSV("a,b\n1,2,3\n")
	require.Error(t, err)
}

func TestParseStructured_BasicAndStableIndex(t *testing.T) {
	rows, err := batch.ParseStructured([]map[string]string{
		{"q": "a"},
		{"q": "b"},
	})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, 0, rows[0].Index)
	assert.Equal(t, 1, rows[1].Index)
}

func TestParseStructured_EmptyList(t *testing.T) {
	_, err := batch.ParseStructured(nil)
	require.Error(t, err)
}

func TestParseStructured_EmptyRow(t *testing.T) {
	_, err := batch.ParseStructured([]map[string]string{{}})
	require.Error(t, err)
}

func TestParseStructured_EmptyKey(t *testing.T) {
	_, err := batch.ParseStructured([]map[string]string{{"": "v"}})
	require.Error(t, err)
}

// Ensure strings import is used.
var _ = strings.Contains
