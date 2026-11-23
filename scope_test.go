package bt

import (
	"encoding/json"
	"testing"
)

func breadcrumbsOf(t *testing.T, atts map[string]string) []map[string]any {
	t.Helper()
	raw, ok := atts["attachment_bt-breadcrumbs-0"]
	if !ok {
		return nil
	}
	var crumbs []map[string]any
	if err := json.Unmarshal([]byte(raw), &crumbs); err != nil {
		t.Fatalf("breadcrumb part is not a JSON array: %v", err)
	}
	return crumbs
}
