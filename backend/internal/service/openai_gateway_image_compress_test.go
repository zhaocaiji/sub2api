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
			got, remove := normalizeImageDataURI(tc.uri, false)
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
			out, _, err := compressBase64InputImagesInOpenAIBody(raw, true)
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

// TestCorruptImagePartIsDropped 验证「无法解码的图片会被整段丢弃」。
// 背景：Codex Windows 沙箱会把 >~1.5MB 的图片截断（openai/codex#46815），上游收到损坏 PNG
// 会返回 400 Corrupt deflate stream。这张坏图进入会话历史后整段对话永久失败。
// 网关丢弃该 part 后，请求可以成功（只少一张图），对话不再卡死。
func TestCorruptImagePartIsDropped(t *testing.T) {
	// 合法 1x1 PNG
	validPNG, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	truncated := validPNG[:60] // 截断 → deflate 流损坏

	cases := []struct {
		name     string
		uri      string
		wantKept bool
	}{
		{"truncated png dropped", "data:image/png;base64," + base64.StdEncoding.EncodeToString(truncated), false},
		{"garbage bytes dropped", "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0xde, 0xad}), false},
		{"invalid base64 dropped", "data:image/png;base64,!!!not-base64!!!", false},
		{"valid small png kept", "data:image/png;base64," + base64.StdEncoding.EncodeToString(validPNG), true},
		{"http link kept", "https://example.com/a.png", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{
				"model": "gpt-5.6-sol",
				"input": []any{
					map[string]any{
						"type": "message",
						"role": "user",
						"content": []any{
							map[string]any{"type": "input_text", "text": "look"},
							map[string]any{"type": "input_image", "image_url": tc.uri},
						},
					},
				},
			}
			raw, _ := json.Marshal(body)
			out, _, err := compressBase64InputImagesInOpenAIBody(raw, true)
			if err != nil {
				t.Fatalf("compress: %v", err)
			}
			var parsed map[string]any
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatalf("输出不是合法 JSON: %v", err)
			}
			items, _ := parsed["input"].([]any)
			im, _ := items[0].(map[string]any)
			parts, _ := im["content"].([]any)

			kept := false
			noticed := false
			for _, p := range parts {
				pm, _ := p.(map[string]any)
				switch strings.TrimSpace(pm["type"].(string)) {
				case "input_image":
					if _, ok := pm["image_url"].(string); ok {
						kept = true
					}
				case "input_text":
					if txt, _ := pm["text"].(string); strings.Contains(txt, "[gateway notice]") {
						noticed = true
					}
				}
			}
			if kept != tc.wantKept {
				t.Errorf("part kept=%v, want %v", kept, tc.wantKept)
			}
			// 丢弃时必须让模型知道（注入说明）；没丢时不应出现说明。
			if tc.wantKept == false && !noticed {
				t.Errorf("图片被丢弃但没有注入 [gateway notice] 说明，模型无法得知")
			}
			if tc.wantKept && noticed {
				t.Errorf("没有丢弃图片却注入了说明")
			}
		})
	}
}
