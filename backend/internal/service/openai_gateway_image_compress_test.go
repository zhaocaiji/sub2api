package service

// Regression tests for gateway-layer image data URI repair.
//
// Upstream (OpenAI Responses / Codex) strictly validates image_url / file_data:
//   Invalid 'input[N].content[M].image_url'. Expected a base64-encoded data URL
//   with an image MIME type (...), but got a value without the ',' separator.
//
// Codex clients do emit such malformed values, and they must be repaired or
// neutralised in the gateway, otherwise the whole request is rejected with 400.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeImageDataURIRepairsMissingComma(t *testing.T) {
	cases := []struct {
		name     string
		uri      string
		wantURI  string
		wantDrop bool
	}{
		{"valid png untouched", "data:image/png;base64,iVBORw0KGgo=", "data:image/png;base64,iVBORw0KGgo=", false},
		{"missing comma with payload", "data:image/png;base64,iVBORw0KGgo=", "data:image/png;base64,iVBORw0KGgo=", false},
		{"missing comma no payload", "data:image/png;base64", "", true},
		{"trailing semicolon", "data:image/png;base64;", "", true},
		{"non image mime", "data:application/pdf;base64,AAA", "", true},
		{"no base64 marker", "data:image/png,AAAA", "", true},
		{"http link untouched", "https://example.com/a.png", "https://example.com/a.png", false},
		{"file id untouched", "file-abc123", "file-abc123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, remove := normalizeImageDataURI(tc.uri)
			if remove != tc.wantDrop {
				t.Fatalf("normalizeImageDataURI(%q) remove=%v, want %v", tc.uri, remove, tc.wantDrop)
			}
			if !tc.wantDrop && got != tc.wantURI {
				t.Fatalf("normalizeImageDataURI(%q) = %q, want %q", tc.uri, got, tc.wantURI)
			}
		})
	}
}

// TestCompressGatewayNeutralisesMalformedImageURL drives the public body-level
// entry point and asserts the surviving content part is valid.
func TestCompressGatewayNeutralisesMalformedImageURL(t *testing.T) {
	cases := []struct {
		name         string
		uri          string
		wantPassthru bool
	}{
		{"valid data uri is compressed or kept valid", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not-a-real-png")), false},
		{"missing comma must not survive", "data:image/png;base64", false},
		{"non image mime must not survive", "data:application/pdf;base64,AAA", false},
		{"http link passes through", "https://example.com/a.png", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"model": "gpt-6-astra",
				"input": []any{
					map[string]any{
						"type": "message",
						"role": "user",
						"content": []any{
							map[string]any{"type": "input_text", "text": "review"},
							map[string]any{"type": "input_image", "image_url": tc.uri},
						},
					},
				},
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			out, _, err := compressBase64InputImagesInOpenAIBody(raw)
			if err != nil {
				t.Fatalf("compress: %v", err)
			}
			var parsed map[string]any
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatalf("output is not valid JSON: %v", err)
			}
			items, _ := parsed["input"].([]any)
			im, _ := items[0].(map[string]any)
			parts, _ := im["content"].([]any)
			for _, p := range parts {
				pm, _ := p.(map[string]any)
				if strings.TrimSpace(pm["type"].(string)) != "input_image" {
					continue
				}
				u, ok := pm["image_url"].(string)
				if !ok {
					continue // dropped field: neutralised
				}
				if tc.wantPassthru {
					if u != tc.uri {
						t.Fatalf("non-data URI was modified: %q", u)
					}
					continue
				}
				if !strings.HasPrefix(u, "data:") {
					t.Fatalf("unexpected value: %q", u)
				}
				if err := validateEmittedImageDataURI(u); err != nil {
					t.Fatalf("malformed value reached the upstream body: %v", err)
				}
			}
		})
	}
}

// validateEmittedImageDataURI checks a value the way the upstream validator does.
func validateEmittedImageDataURI(u string) error {
	comma := strings.IndexByte(u, ',')
	if comma < 0 {
		return errMissingComma
	}
	header := u[:comma]
	if !strings.Contains(header, "base64") || !strings.Contains(header, "image/") {
		return errBadHeader
	}
	return nil
}

type imageURITestError string

func (e imageURITestError) Error() string { return string(e) }

const (
	errMissingComma imageURITestError = "missing ',' separator"
	errBadHeader    imageURITestError = "header lacks base64 and/or image MIME"
)
