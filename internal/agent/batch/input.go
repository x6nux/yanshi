package batch

import (
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
)

// ParseCSV 把 CSV 文本解析为稳定 index 的 Row 切片。首行为 header；后续每行
// 必须与 header 列数一致。空 header cell、重复 header、空 body 都被拒绝。
func ParseCSV(input string) ([]Row, error) {
	reader := csv.NewReader(strings.NewReader(input))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse CSV: %w", err)
	}
	if len(records) < 2 || len(records[0]) == 0 {
		return nil, errors.New("CSV must contain a header and at least one row")
	}
	seen := make(map[string]struct{}, len(records[0]))
	for _, header := range records[0] {
		if header == "" {
			return nil, errors.New("CSV header cannot be empty")
		}
		if _, ok := seen[header]; ok {
			return nil, fmt.Errorf("duplicate CSV header %q", header)
		}
		seen[header] = struct{}{}
	}
	rows := make([]Row, 0, len(records)-1)
	for index, record := range records[1:] {
		if len(record) != len(records[0]) {
			return nil, fmt.Errorf("CSV row %d has %d fields, want %d", index, len(record), len(records[0]))
		}
		values := make(map[string]string, len(record))
		for column, header := range records[0] {
			values[header] = record[column]
		}
		rows = append(rows, Row{Index: index, Values: values})
	}
	return rows, nil
}

// ParseStructured 把 []map[string]string 解析为稳定 index 的 Row 切片。每行
// 必须非空、键非空；调用方选择提供 CSV 或 structured，二选一。
func ParseStructured(values []map[string]string) ([]Row, error) {
	if len(values) == 0 {
		return nil, errors.New("structured rows cannot be empty")
	}
	rows := make([]Row, len(values))
	for index, value := range values {
		if len(value) == 0 {
			return nil, fmt.Errorf("structured row %d is empty", index)
		}
		copyValue := make(map[string]string, len(value))
		for key, item := range value {
			if key == "" {
				return nil, fmt.Errorf("structured row %d has an empty key", index)
			}
			copyValue[key] = item
		}
		rows[index] = Row{Index: index, Values: copyValue}
	}
	return rows, nil
}
