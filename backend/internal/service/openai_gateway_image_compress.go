package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Codex 在 API 模式下只是「看」图（review / 本地 tool call 拼接），图片不是最终交付物，
// 所以可以在网关层把 base64 input_image 压到 review 足够的分辨率，把 65MB 级别的大 body
// 压到几 MB，从而绕过上游 502（body too large）与重试循环。
//
// 策略：只处理 type == "input_image" 的 data:base64 URI；http(s) 外链与
// function_call_output 不动（那是模型自身 tool 调用的上下文）。压缩遵循 fail-open：
// 任一步骤失败都保留原值，绝不破坏请求。
const (
	// openAIImageCompressEnabled 总开关。改 false 即可整体关闭（无需改调用点）。
	openAIImageCompressEnabled = true
	// openAIImageCompressMaxDim 长边上限（像素），超过才缩放。
	openAIImageCompressMaxDim = 1024
	// openAIImageCompressQuality JPEG 编码质量。
	openAIImageCompressQuality = 80
	// openAIImageCompressMinBytes 解码后小于该字节数且未超长边的图不动，避免无谓重编码。
	openAIImageCompressMinBytes = 256 << 10
)

// compressBase64InputImagesInOpenAIBody 对 /v1/responses 请求体中的 base64 input_image
// 做有损压缩，返回 (新 body, 是否变更, error)。结构与 sanitizeEmptyBase64InputImagesInOpenAIBody
// 完全对齐，仅把「删空图」换成「压图」。
func compressBase64InputImagesInOpenAIBody(body []byte) ([]byte, bool, error) {
	if !openAIImageCompressEnabled {
		return body, false, nil
	}
	if !openAIBodyMayContainImageData(body) {
		return body, false, nil
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return body, false, fmt.Errorf("compress request body: %w", err)
	}
	if !compressBase64InputImagesInOpenAIInput(reqBody["input"]) {
		return body, false, nil
	}
	normalized, err := marshalOpenAIUpstreamJSON(reqBody)
	if err != nil {
		return body, false, fmt.Errorf("serialize compressed request body: %w", err)
	}
	slog.Debug("openai_input_image_compressed",
		"body_bytes_before", len(body),
		"body_bytes_after", len(normalized),
		"saved_bytes", len(body)-len(normalized),
	)
	return normalized, true, nil
}

// compressBase64InputImagesInOpenAIInput 处理 input 数组里的图片字段，返回是否有改动。
//   - content parts（用户贴的图）：压缩字节，同时修复畸形 data URI。
//   - function_call_output 的 output（Codex 工具产出的图）：只修复/清洗，不压缩字节
//     —— 在该处压缩图片会触发上游 400。
func compressBase64InputImagesInOpenAIInput(input any) bool {
	items, ok := input.([]any)
	if !ok {
		return false
	}
	changedAny := false
	for _, item := range items {
		itemMap, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if parts, ok := itemMap["content"].([]any); ok {
			kept := make([]any, 0, len(parts))
			droppedAny := false
			changedAnyPart := false
			for _, part := range parts {
				c, drop := compressBase64InputImagePart(part)
				if c {
					changedAnyPart = true
				}
				if drop {
					droppedAny = true
					continue
				}
				kept = append(kept, part)
			}
			if droppedAny {
				itemMap["content"] = kept
				changedAnyPart = true
			}
			if changedAnyPart {
				changedAny = true
			}
		}
		// Codex 会把图片放进 function_call_output 的 output 数组（形如
		// {"type":"input_image","image_url":"data:..."}），上游同样严格校验这里的
		// image_url。压缩该处的图片字节会触发上游 400，因此这里只做「修复/清洗」：
		// 修复畸形 data URI、删除无法修复或 MIME 非图片的项（文本/外链不受影响）。
		if output, ok := itemMap["output"]; ok {
			rewritten, changed, drop := compressToolOutputImages(output)
			if changed {
				if drop {
					delete(itemMap, "output")
				} else {
					itemMap["output"] = rewritten
				}
				changedAny = true
			}
		}
	}
	return changedAny
}

// compressBase64InputImagePart 就地处理单个 content part 里的图片 data URI。
// 返回 (changed, drop)：drop 为 true 表示该 part 已无法安全送上游，调用方必须把
// **整个 part** 删掉 —— 只清空 image_url 会留下既无 file_id 又无 image_url 的
// 空壳，上游会报 "Missing mutually exclusive parameters: 'file_id' or 'image_url'"。
// 兼容 input_image / image_url 的 image_url 字段与 input_file 的 file_data 字段。
func compressBase64InputImagePart(part any) (bool, bool) {
	partMap, ok := part.(map[string]any)
	if !ok {
		return false, false
	}
	switch strings.TrimSpace(firstNonEmptyString(partMap["type"])) {
	case "input_image", "image_url":
		return compressInputImageField(partMap, "image_url")
	case "input_file":
		return compressInputImageField(partMap, "file_data")
	}
	return false, false
}

// isOpenAIToolOutputItem 判断 input 里的某个 item 是不是工具输出项。
// 与 apicompat.isResponsesToolOutputItem 保持一致；图片藏在 output 字段里。
func isOpenAIToolOutputItem(item map[string]any) bool {
	switch strings.TrimSpace(firstNonEmptyString(item["type"])) {
	case "function_call_output", "custom_tool_call_output",
		"tool_search_output", "tool_search_call_output", "mcp_tool_call_output":
		return true
	}
	return false
}

// compressToolOutputImages 递归处理工具输出里的图片。
// output 可能是：裸 data:image URI 字符串、JSON 字符串、或嵌套数组/对象里的
// input_image / image_url 节点。这里**只修复/清洗，不压缩图片字节** —— 在该处
// 压缩字节会触发上游 400。
// 返回 (value, changed, drop)：drop 为 true 表示该值已无法安全送上游，调用方须删除它。
// 注意此处不做字节压缩，因此 `changed` 只代表「畸形值被修复或被删除」。
func compressToolOutputImages(value any) (any, bool, bool) {
	switch v := value.(type) {
	case string:
		if normalized, ok := tryNormalizeStandaloneImageDataURI(v); ok {
			return normalized, normalized != v, normalized == ""
		}
		decoded, ok := decodeJSONAny([]byte(v))
		if !ok {
			return value, false, false
		}
		rewritten, changed, drop := compressToolOutputImages(decoded)
		if drop || !changed {
			return value, changed, drop
		}
		encoded, err := marshalOpenAIUpstreamJSON(rewritten)
		if err != nil {
			return value, false, false
		}
		return string(encoded), true, false
	case []any:
		kept := make([]any, 0, len(v))
		changed := false
		for i := range v {
			rewritten, c, drop := compressToolOutputImages(v[i])
			if c {
				changed = true
			}
			if drop {
				changed = true
				continue
			}
			kept = append(kept, rewritten)
		}
		if !changed {
			return v, false, false
		}
		return kept, true, false
	case map[string]any:
		// 先按 parts 处理（image_url / file_data）：这里的删除语义是删掉**整个节点**。
		if _, drop := compressBase64InputImagePart(v); drop {
			return nil, true, true
		}
		changed := false
		for k := range v {
			rewritten, c, drop := compressToolOutputImages(v[k])
			if c {
				changed = true
			}
			if drop {
				delete(v, k)
				changed = true
				continue
			}
			v[k] = rewritten
		}
		return v, changed, false
	default:
		return value, false, false
	}
}

// tryNormalizeStandaloneImageDataURI 判断字符串是否是**图片 data URI**，是则返回
// 规范化结果（无法修复时返回空串表示应删除）。非图片 data URI 与其他字符串都返回
// ok=false，表示不归这里处理，避免误删普通文本。
func tryNormalizeStandaloneImageDataURI(s string) (string, bool) {
	if !strings.HasPrefix(s, "data:") {
		return "", false
	}
	header := s
	if comma := strings.IndexByte(s, ','); comma >= 0 {
		header = s[:comma]
	}
	if !strings.Contains(header, "image/") {
		return "", false
	}
	return normalizeImageDataURI(s)
}

// decodeJSONAny 把字符串按 JSON 解码为 any，仅当是合法 JSON 对象/数组时返回 true。
// 数字用 UseNumber 保留精度，避免大整数被 float64 截断。
func decodeJSONAny(raw []byte) (any, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil, false
	}
	if !json.Valid(trimmed) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

// normalizeImageDataURI 修复/清洗一个图片 data URI，使其满足 OpenAI Responses
// 对 image_url / file_data 的严格校验：必须是 data:<image mime>;base64,<payload>。
//
// 上游会以 400「Expected a base64-encoded data URL with an image MIME type ...
// but got a value without the ',' separator」拒绝畸形值，而 Codex 客户端确实会
// 发出这类值（已在 ops_error_logs 中确认 input[6].content[5].image_url、
// input[33].output[2].image_url 两种路径）。这些值此前会被原样送上游。
//
// 返回值语义：
//   - (uri, false)：原样保留（合法，或不是 data URI，如 http(s)/file_id）
//   - (repaired, true)：已修复，应写回原字段
//   - ("", true)：无法修复或 MIME 非图片，应删除该部分
func normalizeImageDataURI(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "data:") {
		return raw, false // http(s) 外链、file_id 等不归这里管
	}
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		// 形如 "data:image/png;base64"：丢了 ',' 分隔符。补一个即可复原。
		if strings.HasSuffix(raw, ";base64") || strings.HasSuffix(raw, ";base64;") {
			return strings.TrimSuffix(strings.TrimSuffix(raw, ";"), "base64") + "base64,", true
		}
		slog.Warn("openai_image_datauri_unrepairable", "reason", "missing_comma", "head", truncateImageURILog(raw))
		return "", true
	}
	header := raw[:comma]
	payload := raw[comma+1:]
	if !strings.Contains(header, ";base64") {
		slog.Warn("openai_image_datauri_unrepairable", "reason", "no_base64_marker", "header", truncateImageURILog(header))
		return "", true
	}
	if strings.Contains(header, "image/") {
		return raw, false
	}
	// 非图片 MIME（如 data:application/pdf;base64,...）不能出现在 image_url 上。
	slog.Warn("openai_image_datauri_unrepairable", "reason", "not_image_mime", "header", truncateImageURILog(header))
	_ = payload
	return "", true
}

// truncateImageURILog 截断日志字段，避免把 base64 主体写进日志。
func truncateImageURILog(s string) string {
	const maxLen = 60
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}

// compressInputImageField 处理 part 里某个承载 data URI 的字段（image_url / file_data）。
// 字段值可能是字符串 "data:..."，也可能是 {"url": "data:..."} 对象形式。
// 返回 (changed, drop)：drop 为 true 表示调用方必须删除**整个 part** —— 只清空
// image_url 会留下既无 file_id 又无 image_url 的空壳，上游会报
// "Missing mutually exclusive parameters: 'file_id' or 'image_url'"。
func compressInputImageField(partMap map[string]any, field string) (bool, bool) {
	var dataURI string
	switch v := partMap[field].(type) {
	case string:
		dataURI = v
	case map[string]any:
		raw, _ := v["url"].(string)
		dataURI = raw
	default:
		return false, false
	}
	if dataURI == "" {
		return false, false
	}
	original := dataURI

	// 先修复/清洗：无法修复或 MIME 非图片时删除整个 part，避免整请求被上游 400 拒绝。
	normalized, remove := normalizeImageDataURI(dataURI)
	if remove {
		slog.Warn("openai_image_part_dropped", "field", field, "head", truncateImageURILog(original))
		return true, true
	}
	dataURI = normalized

	compressed, ok := compressBase64DataURI(dataURI)
	if !ok {
		// 不压缩（http(s)/file_id/已足够小等）；仅当修复改动了值才写回。
		if dataURI != original {
			writeBackImageField(partMap, field, dataURI)
			slog.Warn("openai_image_datauri_repaired", "field", field, "head", truncateImageURILog(original))
			return true, false
		}
		return false, false
	}

	// 对象形式把压缩结果写回 url 字段，字符串形式直接替换。
	writeBackImageField(partMap, field, compressed)
	return true, false
}

// writeBackImageField 把处理结果写回字段，兼容字符串与 {"url": ...} 对象两种形态。
func writeBackImageField(partMap map[string]any, field, value string) {
	switch v := partMap[field].(type) {
	case string:
		partMap[field] = value
	case map[string]any:
		v["url"] = value
	default:
		partMap[field] = value
	}
}

// compressBase64DataURI 解析 data:...;base64,... 并重编码为 data:image/jpeg;base64,...
func compressBase64DataURI(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "data:") {
		return raw, false // 只处理 data URI，http(s) 外链不动
	}
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		return raw, false
	}
	header := raw[:comma]
	payload := raw[comma+1:]
	if !strings.Contains(header, "base64") {
		return raw, false
	}
	if !strings.Contains(header, "image/") {
		return raw, false // 只压图片；PDF/文本等 file_data 不动
	}

	trimmed := strings.TrimSpace(payload)
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		// 无 padding 的 RawStdEncoding 兜底
		decoded, err = base64.RawStdEncoding.DecodeString(trimmed)
		if err != nil {
			return raw, false
		}
	}
	if len(decoded) == 0 {
		return raw, false
	}

	compressed, ok := compressDecodedImage(decoded)
	if !ok {
		return raw, false
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(compressed), true
}

// openAIImageCompressDumpDir TEMP 诊断：把「原图 + 压缩后 JPEG」落盘到该目录，供人工 review
// 压缩质量。设为空字符串即关闭。文件名用原图 sha256 前 8 字节做 stem，天然去重（同一张图
// 反复重发只覆盖同一文件，不会撑爆磁盘）。
const openAIImageCompressDumpDir = "/app/data/imgcompress_debug"

// dumpOpenAICompressedImage 把压缩前后的字节写盘，供 review。任何失败都静默忽略（fail-open）。
func dumpOpenAICompressedImage(original, compressed []byte) {
	if openAIImageCompressDumpDir == "" {
		return
	}
	if err := os.MkdirAll(openAIImageCompressDumpDir, 0o755); err != nil {
		return
	}
	sum := sha256.Sum256(original)
	stem := hex.EncodeToString(sum[:8])
	compPath := filepath.Join(openAIImageCompressDumpDir, stem+"_compressed.jpg")
	if _, err := os.Stat(compPath); err == nil {
		return // 已 dump 过同一张图（历史重发会反复命中），跳过避免刷屏
	}
	origPath := filepath.Join(openAIImageCompressDumpDir, stem+"_orig."+sniffImageExt(original))
	_ = os.WriteFile(origPath, original, 0o644)
	_ = os.WriteFile(compPath, compressed, 0o644)
	slog.Info("openai_image_compress_dumped",
		"stem", stem, "orig_bytes", len(original), "compressed_bytes", len(compressed))
}

// sniffImageExt 根据魔数返回图片扩展名，无法识别时返回 bin。
func sniffImageExt(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpg"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte{'R', 'I', 'F', 'F'}) && bytes.Equal(b[8:12], []byte{'W', 'E', 'B', 'P'}):
		return "webp"
	case len(b) >= 6 && bytes.Equal(b[:4], []byte{'G', 'I', 'F', '8'}):
		return "gif"
	default:
		return "bin"
	}
}

// compressDecodedImage 解码 → 若超长边则按比例缩放 → 白底铺平透明 → JPEG 重编码。
// 返回 (新字节, 是否应替换)。任何一步失败都返回 false 保留原图（fail-open）。
func compressDecodedImage(decoded []byte) ([]byte, bool) {
	src, _, err := image.Decode(bytes.NewReader(decoded))
	if err != nil {
		return nil, false
	}
	b := src.Bounds()
	if b.Empty() {
		return nil, false
	}

	longSide := max(b.Dx(), b.Dy())
	if longSide <= openAIImageCompressMaxDim && len(decoded) <= openAIImageCompressMinBytes {
		return nil, false // 已经够小，跳过
	}

	width, height := b.Dx(), b.Dy()
	if longSide > openAIImageCompressMaxDim {
		scale := float64(openAIImageCompressMaxDim) / float64(longSide)
		width = max(1, int(float64(width)*scale))
		height = max(1, int(float64(height)*scale))
	}

	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	stddraw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, stddraw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, stddraw.Over, nil)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: openAIImageCompressQuality}); err != nil {
		return nil, false
	}
	// 兜底：若重编码反而更大（极端情况），保留原图。
	if buf.Len() >= len(decoded) {
		return nil, false
	}
	compressed := buf.Bytes()
	dumpOpenAICompressedImage(decoded, compressed)
	return compressed, true
}

// openAIBodyMayContainImageData 快速预检：body 是否可能含图片 data URI。
// 覆盖 input_image / input_file / file_data / image_url / data:image 等 token。
func openAIBodyMayContainImageData(body []byte) bool {
	for _, tok := range []string{"input_image", "input_file", "file_data", "image_url", "data:image"} {
		if bytes.Contains(body, []byte(tok)) {
			return true
		}
	}
	return bytes.Contains(body, []byte("\\u"))
}
