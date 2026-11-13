package cosy

import (
	"crypto/md5"
	"crypto/rand"
	"fmt"
	"time"
)

const (
	AppCode   = "cosy"
	SecretB64 = "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==" // base64("war, war never changes")
	sep       = "&"
)

// CurrentDate returns GMT RFC1123 date string used by legacy jobToken signing.
func CurrentDate() string {
	return time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
}

// SignLegacy computes the legacy MD5 signature for jobToken requests.
func SignLegacy(date string) string {
	return Md5Hex(AppCode + sep + SecretB64 + sep + date)
}

// Md5Hex returns lowercase hex MD5 digest of s.
func Md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return fmt.Sprintf("%x", h)
}

// NewUUID returns a random RFC 4122 UUID v4 string.
func NewUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// NewSimpleID returns a 32-char lowercase hex UUID without hyphens.
func NewSimpleID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x", b)
}

// NewRequestID returns a 24-hex request ID.
func NewRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// UnixSec returns current Unix timestamp in seconds.
func UnixSec() int64 { return time.Now().Unix() }

// UnixMs returns current Unix timestamp in milliseconds.
func UnixMs() int64 { return time.Now().UnixMilli() }
