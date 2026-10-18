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
//
// dropCorrupt=true 时，无法解码的图片 part 会被丢弃（见 shouldDropUndecodableImages）。
func compressBase64InputImagesInOpenAIBody(body []byte, dropCorrupt bool) ([]byte, bool, error) {
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
	if !compressBase64InputImagesInOpenAIInput(reqBody["input"], dropCorrupt) {
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
//
// dropCorrupt=true 时，无法解码的图片会被丢弃（仅对真正的 OpenAI/Codex 上游启用，
// 见 shouldDropUndecodableImages；CN 供应商保持原样透传）。
func compressBase64InputImagesInOpenAIInput(input any, dropCorrupt bool) bool {
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
				c, drop := compressBase64InputImagePart(part, dropCorrupt)
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
			rewritten, changed, drop := compressToolOutputImages(output, dropCorrupt)
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
func compressBase64InputImagePart(part any, dropCorrupt bool) (bool, bool) {
	partMap, ok := part.(map[string]any)
	if !ok {
		return false, false
	}
	switch strings.TrimSpace(firstNonEmptyString(partMap["type"])) {
	case "input_image", "image_url":
		return compressInputImageField(partMap, "image_url", dropCorrupt)
	case "input_file":
		return compressInputImageField(partMap, "file_data", dropCorrupt)
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
func compressToolOutputImages(value any, dropCorrupt bool) (any, bool, bool) {
	switch v := value.(type) {
	case string:
		if normalized, ok := tryNormalizeStandaloneImageDataURI(v); ok {
			return normalized, normalized != v, normalized == ""
		}
		decoded, ok := decodeJSONAny([]byte(v))
		if !ok {
			return value, false, false
		}
		rewritten, changed, drop := compressToolOutputImages(decoded, dropCorrupt)
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
			rewritten, c, drop := compressToolOutputImages(v[i], dropCorrupt)
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
		if _, drop := compressBase64InputImagePart(v, dropCorrupt); drop {
			return nil, true, true
		}
		changed := false
		for k := range v {
			rewritten, c, drop := compressToolOutputImages(v[k], dropCorrupt)
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
	return normalizeImageDataURI(s, false)
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
// allowNonImageMIME=true 用于 input_file.file_data：那里允许 PDF/文本等 data URI，
// 不应因 MIME 不是 image/* 而删除。image_url 位置必须传 false。
//
// 返回值语义：
//   - (uri, false)：原样保留（合法，或不是 data URI，如 http(s)/file_id）
//   - (repaired, true)：已修复，应写回原字段
//   - ("", true)：无法修复或 MIME 不允许，应删除该部分
func normalizeImageDataURI(raw string, allowNonImageMIME bool) (string, bool) {
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
	if !strings.Contains(header, ";base64") {
		slog.Warn("openai_image_datauri_unrepairable", "reason", "no_base64_marker", "header", truncateImageURILog(header))
		return "", true
	}
	if strings.Contains(header, "image/") || allowNonImageMIME {
		return raw, false
	}
	// 非图片 MIME（如 data:application/pdf;base64,...）不能出现在 image_url 上。
	slog.Warn("openai_image_datauri_unrepairable", "reason", "not_image_mime", "header", truncateImageURILog(header))
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
func compressInputImageField(partMap map[string]any, field string, dropCorrupt bool) (bool, bool) {
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

	// 先修复/清洗：无法修复时删除整个 part，避免整请求被上游 400 拒绝。
	// file_data 允许非图片 MIME（PDF/文本），image_url 不允许。
	normalized, remove := normalizeImageDataURI(dataURI, field == "file_data")
	if remove {
		slog.Warn("openai_image_part_dropped", "field", field, "head", truncateImageURILog(original))
		return true, true
	}
	dataURI = normalized

	compressed, status := compressBase64DataURI(dataURI)
	switch status {
	case imageURICorrupt:
		// 图片字节无法解码（截断/损坏/格式不支持）。送上游必然 400，且会让整个请求失败；
		// 仅在明确启用时丢弃（CN 供应商保持原样透传）。
		if openAIDropUndecodableImages && dropCorrupt {
			slog.Warn("openai_image_part_dropped_corrupt",
				"field", field, "head", truncateImageURILog(original), "bytes", len(original))
			return true, true
		}
		// 不丢弃时保持原样透传（便于排查）。
	case imageURICompressed:
		// 对象形式把压缩结果写回 url 字段，字符串形式直接替换。
		writeBackImageField(partMap, field, compressed)
		return true, false
	default:
		// 不压缩（http(s)/file_id/已足够小等）；仅当修复改动了值才写回。
		if dataURI != original {
			writeBackImageField(partMap, field, dataURI)
			slog.Warn("openai_image_datauri_repaired", "field", field, "head", truncateImageURILog(original))
			return true, false
		}
	}
	return false, false
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

// imageURIStatus 描述 compressBase64DataURI 对一个图片 data URI 的处理结果。
type imageURIStatus int

const (
	// imageURIPassthrough 不需要改动（非 data URI / 非图片 / 已经是小图 / 无损重编码收益）。
	imageURIPassthrough imageURIStatus = iota
	// imageURICompressed 已被重新编码为 JPEG，应写回。
	imageURICompressed
	// imageURICorrupt base64 或图片字节本身无法解码（截断 / 损坏 / 不支持的格式）。
	// 这种值送上游必然被 400 拒绝，调用方应丢弃整个 part。
	imageURICorrupt
)

// openAIDropUndecodableImages 为 true 时，无法解码的图片 part 会被**丢弃**，而不是原样透传给上游。
//
// 背景：Codex 在 Windows 上有一个已知缺陷（openai/codex#46815），沙箱 helper 会把
// 超过约 1.5MB 的图片数据截断，导致 PNG deflate 流损坏。上游收到后会返回
//
//	400 input_image image_url could not parse image: ... Corrupt deflate stream
//
// 而这张坏图一旦进入会话历史，每一轮重试都会重发它，整个对话永久失败。
// 丢弃该 part 可以把「硬 400」变成「请求成功，只少这一张图」。
const openAIDropUndecodableImages = true

// shouldDropUndecodableImages 判断该账号的上游是否属于「收到损坏图片会直接 400」的那类。
// 仅对真正的 OpenAI / Codex 上游启用；CN 供应商（DeepSeek / Kimi 等）保持原样透传，
// 避免改变它们的图片处理语义（它们会把 input_image 改写成 url 形式）。
func shouldDropUndecodableImages(account *Account) bool {
	if account == nil {
		return false
	}
	if account.UsesOpenAICodexProtocol() {
		return true
	}
	if account.Platform == PlatformOpenAI && strings.TrimSpace(account.GetOpenAIBaseURL()) == "" {
		return true
	}
	return false
}

// compressBase64DataURI 解析 data:...;base64,... 并重编码为 data:image/jpeg;base64,...
// 第二个返回值是处理结果状态，见 imageURIStatus。
func compressBase64DataURI(raw string) (string, imageURIStatus) {
	if !strings.HasPrefix(raw, "data:") {
		return raw, imageURIPassthrough // 只处理 data URI，http(s) 外链不动
	}
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		return raw, imageURIPassthrough
	}
	header := raw[:comma]
	payload := raw[comma+1:]
	if !strings.Contains(header, "base64") {
		return raw, imageURIPassthrough
	}
	if !strings.Contains(header, "image/") {
		return raw, imageURIPassthrough // 只压图片；PDF/文本等 file_data 不动
	}

	trimmed := strings.TrimSpace(payload)
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		// 无 padding 的 RawStdEncoding 兜底
		decoded, err = base64.RawStdEncoding.DecodeString(trimmed)
		if err != nil {
			return raw, imageURICorrupt // base64 本身坏了
		}
	}
	if len(decoded) == 0 {
		return raw, imageURICorrupt
	}

	compressed, changed, parseable := compressDecodedImage(decoded)
	if !changed {
		if !parseable {
			return raw, imageURICorrupt
		}
		return raw, imageURIPassthrough
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(compressed), imageURICompressed
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
// 返回 (新字节, 是否应替换, 是否可解析)。
//   - parseable=false 表示这段字节根本不是能解码的图片（截断/损坏/不支持的格式），
//     这种值送上游必然被 400 拒绝，调用方应丢弃整个 part。
//   - 其余失败（编码失败、重编码反而更大、已经够小）都保留原图，parseable=true。
func compressDecodedImage(decoded []byte) ([]byte, bool, bool) {
	src, _, err := image.Decode(bytes.NewReader(decoded))
	if err != nil {
		return nil, false, false
	}
	b := src.Bounds()
	if b.Empty() {
		return nil, false, false
	}

	longSide := max(b.Dx(), b.Dy())
	if longSide <= openAIImageCompressMaxDim && len(decoded) <= openAIImageCompressMinBytes {
		return nil, false, true // 已经够小，跳过
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
		return nil, false, true
	}
	// 兜底：若重编码反而更大（极端情况），保留原图。
	if buf.Len() >= len(decoded) {
		return nil, false, true
	}
	compressed := buf.Bytes()
	dumpOpenAICompressedImage(decoded, compressed)
	return compressed, true, true
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
