package upstream

import (
	"strings"
)

// SanitizeText removes disallowed C0 control bytes (except tab, LF, CR) that can break upstream JSON parsing.
func SanitizeText(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FlattenContent extracts concatenated plain text and any image URLs/data-URIs from string or array content.
// When image parts are present, NormalizedContent preserves structured parts for Qoder VL models.
func FlattenContent(raw any) (text string, images []string, hasImages bool) {
	switch v := raw.(type) {
	case string:
		return SanitizeText(v), nil, false
	case []any:
		var parts []string
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				if s, ok := item.(string); ok {
					parts = append(parts, SanitizeText(s))
				}
				continue
			}
			typ := strings.ToLower(strings.TrimSpace(anyStr(m["type"])))
			switch typ {
			case "text", "input_text", "output_text":
				if t := anyStr(m["text"]); t != "" {
					parts = append(parts, SanitizeText(t))
				} else if c := anyStr(m["content"]); c != "" {
					parts = append(parts, SanitizeText(c))
				}
			case "image_url", "input_image":
				hasImages = true
				u := extractImageURL(m)
				if u != "" {
					images = append(images, u)
				}
			case "image":
				// Anthropic base64 image block
				hasImages = true
				if src, ok := m["source"].(map[string]any); ok {
					mt := anyStr(src["media_type"])
					data := anyStr(src["data"])
					if mt != "" && data != "" {
						images = append(images, "data:"+mt+";base64,"+data)
					} else if u := anyStr(src["url"]); u != "" {
						images = append(images, u)
					}
				}
			default:
				if t := anyStr(m["text"]); t != "" {
					parts = append(parts, SanitizeText(t))
				}
			}
		}
		return strings.Join(parts, "\n"), images, hasImages
	default:
		return "", nil, false
	}
}

func extractImageURL(m map[string]any) string {
	if iu, ok := m["image_url"].(map[string]any); ok {
		return strings.TrimSpace(anyStr(iu["url"]))
	}
	if iu, ok := m["image_url"].(string); ok {
		return strings.TrimSpace(iu)
	}
	if u := anyStr(m["url"]); u != "" {
		return u
	}
	return ""
}

// RequestHasImages checks whether any message in a request body carries image inputs.
func RequestHasImages(messages []any) bool {
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		_, imgs, has := FlattenContent(m["content"])
		if has || len(imgs) > 0 {
			return true
		}
	}
	return false
}

// NormalizeRole maps developer/unknown roles into upstream-compatible chat roles.
func NormalizeRole(role string) string {
	r := strings.ToLower(strings.TrimSpace(role))
	switch r {
	case "system", "developer":
		return "system"
	case "assistant", "user", "tool":
		return r
	default:
		return "user"
	}
}
