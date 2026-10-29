package service

// 验证「工具输出（function_call_output）里的图片」也会被压缩。
// 背景：2026-10-29 出现 413（请求体 >256MB），排查发现会话里单张工具输出图片可达 100-357MB。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"strings"
	"testing"
)

// buildNoisyPNG 生成 w*h 的随机噪声 PNG（体积大，用于触发压缩）。
func buildNoisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(42))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(rnd.Intn(256)),
				G: uint8(rnd.Intn(256)),
				B: uint8(rnd.Intn(256)),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func TestToolOutputImagesAreCompressed(t *testing.T) {
	pngBytes := buildNoisyPNG(t, 900, 900)
	origB64Len := len(base64.StdEncoding.EncodeToString(pngBytes))
	t.Logf("原图: %d 字节, base64 %d 字节", len(pngBytes), origB64Len)

	body := map[string]any{
		"model": "gpt-6.1-sol",
		"input": []any{
			map[string]any{
				"type":    "function_call_output",
				"call_id": "call_1",
				"output": []any{
					map[string]any{
						"type":      "input_image",
						"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes),
					},
				},
			},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	out, changed, err := compressBase64InputImagesInOpenAIBody(raw, true)
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if !changed {
		t.Fatalf("工具输出里的图没有被压缩（changed=false）")
	}
	if len(out) >= len(raw) {
		t.Fatalf("压缩后反而更大: %d -> %d", len(raw), len(out))
	}

	// 解析出来确认仍是合法 JPEG data URI
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	items := parsed["input"].([]any)
	item := items[0].(map[string]any)
	output := item["output"].([]any)
	part := output[0].(map[string]any)
	uri, _ := part["image_url"].(string)
	if !strings.HasPrefix(uri, "data:image/jpeg;base64,") {
		t.Fatalf("压缩后不是 JPEG data URI: %s", uri[:min(60, len(uri))])
	}
	t.Logf("压缩后 base64 %d 字节（原 %d，缩减到 %.1f%%）",
		len(uri), origB64Len, float64(len(uri))/float64(origB64Len)*100)
}
