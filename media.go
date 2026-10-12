package bridge

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// MaxMediaBytes is the hard cap for a single /cgi-bin/media/get proxy body (25 MiB).
const MaxMediaBytes int64 = 25 << 20

// MediaPayload is sealed into inbox.payload_ref for image/file/voice messages.
type MediaPayload struct {
	MediaID  string `json:"media_id"`
	FileName string `json:"file_name,omitempty"`
}

// MediaDownload is the proxied result of WeCom /cgi-bin/media/get.
type MediaDownload struct {
	Data               []byte
	ContentType        string
	ContentDisposition string
}

// EncodeMediaPayload serializes media metadata for durable inbox storage.
// fileName is basename-sanitized; empty stays empty in storage.
func EncodeMediaPayload(mediaID, fileName string) string {
	p := MediaPayload{MediaID: strings.TrimSpace(mediaID)}
	if strings.TrimSpace(fileName) != "" {
		p.FileName = SafeFileName(fileName)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// DecodeMediaPayload parses a sealed media payload. Plain non-JSON strings are
// treated as a bare media_id for forward/backward compatibility.
func DecodeMediaPayload(raw string) (MediaPayload, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return MediaPayload{}, false
	}
	if raw[0] == '{' {
		var p MediaPayload
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return MediaPayload{}, false
		}
		p.MediaID = strings.TrimSpace(p.MediaID)
		if p.MediaID == "" {
			return MediaPayload{}, false
		}
		if p.FileName != "" {
			p.FileName = SafeFileName(p.FileName)
		}
		return p, true
	}
	return MediaPayload{MediaID: raw}, true
}

// SafeFileName keeps only the base name and rejects path traversal / empty.
// Empty or unsafe input becomes "file" so XML FileName is always present for file msgs.
func SafeFileName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "file"
	}
	name = strings.ReplaceAll(name, "\\", "/")
	base := filepath.Base(name)
	if base == "." || base == ".." || base == "/" || base == "" {
		return "file"
	}
	if !utf8.ValidString(base) {
		return "file"
	}
	for _, r := range base {
		if r < 0x20 {
			return "file"
		}
	}
	return base
}

// TruncateID shortens identifiers for logs (never log full media_id / tokens).
func TruncateID(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// DeliverableMsgType reports whether an inbox type is forwarded to cc-connect.
func DeliverableMsgType(t string) bool {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "text", "image", "file", "voice":
		return true
	default:
		return false
	}
}
