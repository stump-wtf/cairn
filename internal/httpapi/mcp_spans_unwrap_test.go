package httpapi

import (
	"encoding/json"
	"testing"
)

func TestUnwrapStringEncodedSpans(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantSame bool // true if output should equal input (no fixup)
	}{
		{
			name:     "native array unchanged",
			input:    `{"id":"test","spans":[{"span_id":"s1","category":"exec"}]}`,
			wantSame: true,
		},
		{
			name:     "string-encoded spans unwrapped",
			input:    `{"id":"test","spans":"[{\"span_id\":\"s1\",\"category\":\"exec\"}]"}`,
			wantSame: false,
		},
		{
			name:     "null spans unchanged",
			input:    `{"id":"test","spans":null}`,
			wantSame: true,
		},
		{
			name:     "missing spans unchanged",
			input:    `{"id":"test"}`,
			wantSame: true,
		},
		{
			name:     "empty object unchanged",
			input:    `{}`,
			wantSame: true,
		},
		{
			name:     "invalid JSON returned as-is",
			input:    `not json`,
			wantSame: true,
		},
		{
			name:     "string that is not valid array left as-is",
			input:    `{"id":"test","spans":"not an array"}`,
			wantSame: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unwrapStringEncodedSpans(json.RawMessage(tt.input))
			if tt.wantSame {
				if string(got) != tt.input {
					t.Errorf("unwrapStringEncodedSpans() = %s, want unchanged %s", string(got), tt.input)
				}
			} else {
				// Verify the result is valid JSON with spans as an array
				var args map[string]json.RawMessage
				if err := json.Unmarshal(got, &args); err != nil {
					t.Fatalf("result is not valid JSON: %v", err)
				}
				spansRaw, ok := args["spans"]
				if !ok {
					t.Fatal("result missing spans field")
				}
				// Verify spans is now an array (starts with '[')
				if len(spansRaw) == 0 || spansRaw[0] != '[' {
					t.Errorf("spans should be an array, got: %s", string(spansRaw))
				}
			}
		})
	}
}
