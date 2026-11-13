package cosy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AESCBCDecrypt decrypts AES-128-CBC ciphertext with PKCS7 padding.
func AESCBCDecrypt(data, key, iv []byte) ([]byte, error) {
	if len(key) != 16 || len(iv) != 16 {
		return nil, fmt.Errorf("aes key/iv must be 16 bytes")
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext length %d not block aligned", len(data))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, data)
	pad := int(out[len(out)-1])
	if pad < 1 || pad > aes.BlockSize || len(out) < pad {
		return nil, fmt.Errorf("bad PKCS7 padding")
	}
	for i := len(out) - pad; i < len(out); i++ {
		if int(out[i]) != pad {
			return nil, fmt.Errorf("bad PKCS7 padding")
		}
	}
	return out[:len(out)-pad], nil
}

// ChromiumDecryptV10 decrypts Chromium "v10" AES-256-GCM blobs (used by Electron auth.v1.dat).
func ChromiumDecryptV10(blob, key []byte) ([]byte, error) {
	if len(blob) < 3+12+16 || string(blob[:3]) != "v10" {
		return nil, fmt.Errorf("unsupported chromium blob format")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := blob[3:15]
	ciphertext := blob[15:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func hkdfSHA256(ikm, salt, info []byte, length int) []byte {
	if len(salt) == 0 {
		salt = make([]byte, 32)
	}
	mac := hmac.New(sha256.New, salt)
	mac.Write(ikm)
	prk := mac.Sum(nil)
	var out, t []byte
	for i := byte(1); len(out) < length; i++ {
		h := hmac.New(sha256.New, prk)
		h.Write(t)
		h.Write(info)
		h.Write([]byte{i})
		t = h.Sum(nil)
		out = append(out, t...)
	}
	return out[:length]
}

// DecryptQMCCatalog decrypts Qoder local client model cache (.models/<uid>/catalog-v6|v7).
func DecryptQMCCatalog(blobB64, uid string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(blobB64))
	if err != nil {
		return nil, err
	}
	if len(raw) < 4+12+16 || string(raw[:4]) != "QMC\x01" {
		return nil, fmt.Errorf("unsupported QMC envelope")
	}
	key := hkdfSHA256([]byte(uid), []byte("qoder-model-cache-enc"), []byte("model-cache-v1"), 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := raw[4:16]
	sealed := raw[16:]
	return gcm.Open(nil, nonce, sealed, nil)
}

// DecryptCLIUserFile reads and decrypts ~/.qoder[.cn]/.auth/user* files.
func DecryptCLIUserFile(path, machineID string) (map[string]any, error) {
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(rawBytes))
	if strings.HasPrefix(raw, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			return nil, err
		}
		return obj, nil
	}
	if len(machineID) < 16 {
		return nil, fmt.Errorf("machine_id shorter than 16 bytes")
	}
	ct, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, err
	}
	key := []byte(machineID[:16])
	pt, err := AESCBCDecrypt(ct, key, key)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	if err := json.Unmarshal(pt, &obj); err != nil {
		return nil, err
	}
	return obj, nil
}

// LocalCredentialCandidate describes one discovered local Qoder Desktop or CLI credential.
type LocalCredentialCandidate struct {
	Kind      string `json:"kind"` // "app" | "cli"
	Path      string `json:"path"`
	File      string `json:"file"`
	Realm     string `json:"realm"` // "cn" | "global"
	Domain    string `json:"domain"`
	Readable  bool   `json:"readable"`
	Valid     bool   `json:"valid"`
	UID       string `json:"uid,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Error     string `json:"error,omitempty"`

	// Decrypted secrets kept unexported in JSON responses unless needed for import
	AccessToken      string `json:"-"`
	RefreshToken     string `json:"-"`
	UserType         string `json:"-"`
	OrganizationID   string `json:"-"`
	OrganizationName string `json:"-"`
}

// ScanLocalCredentials performs a read-only scan for local Qoder Desktop App & CLI credentials
// across both CN (.qoder-cn / com.qodercn.app.stable) and Global (.qoder / com.qoder.app.stable).
func ScanLocalCredentials() []LocalCredentialCandidate {
	home, _ := os.UserHomeDir()
	var results []LocalCredentialCandidate

	type realmSpec struct {
		realm   string
		domain  string
		homeDir string
		appDir  string
	}
	specs := []realmSpec{
		{realm: "global", domain: "qoder.com", homeDir: ".qoder", appDir: "com.qoder.app.stable"},
		{realm: "cn", domain: "qoder.com.cn", homeDir: ".qoder-cn", appDir: "com.qodercn.app.stable"},
	}

	for _, sp := range specs {
		// 1. Desktop App auth.v1.dat (Windows DPAPI + AES-256-GCM)
		if appCred := scanDesktopAppAuth(sp.realm, sp.domain, sp.appDir); appCred != nil {
			results = append(results, *appCred)
		}

		// 2. CLI credentials (~/.qoder[.cn]/.auth/user*)
		if home == "" {
			continue
		}
		authDir := filepath.Join(home, sp.homeDir, ".auth")
		entries, err := os.ReadDir(authDir)
		if err != nil {
			continue
		}
		midBytes, _ := os.ReadFile(filepath.Join(authDir, "machine_id"))
		machineID := strings.TrimSpace(string(midBytes))

		for _, ent := range entries {
			if ent.IsDir() {
				continue
			}
			name := ent.Name()
			if name != "user" && !strings.HasPrefix(name, "user.") {
				continue
			}
			fullPath := filepath.Join(authDir, name)
			cand := LocalCredentialCandidate{
				Kind:   "cli",
				Path:   fullPath,
				File:   name,
				Realm:  sp.realm,
				Domain: sp.domain,
			}
			obj, err := DecryptCLIUserFile(fullPath, machineID)
			if err != nil {
				cand.Error = err.Error()
				results = append(results, cand)
				continue
			}
			cand.Readable = true
			tok, _ := obj["access_token"].(string)
			ref, _ := obj["refresh_token"].(string)
			uid, _ := obj["uid"].(string)
			nick, _ := obj["name"].(string)
			ut, _ := obj["user_type"].(string)
			if ut == "" {
				ut, _ = obj["userType"].(string)
			}
			orgID, _ := obj["organization_id"].(string)
			orgName, _ := obj["organization_name"].(string)
			exp := extractInt64(obj["expire_time"])
			cand.Valid = strings.HasPrefix(tok, "dt-") || strings.HasPrefix(tok, "jt-") || ref != ""
			cand.UID = uid
			cand.Nickname = nick
			cand.ExpiresAt = exp
			cand.AccessToken = tok
			cand.RefreshToken = ref
			cand.UserType = ut
			cand.OrganizationID = orgID
			cand.OrganizationName = orgName
			results = append(results, cand)
		}
	}
	return results
}

func extractInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		if x > 1e12 {
			return int64(x / 1000)
		}
		return int64(x)
	case int64:
		if x > 1e12 {
			return x / 1000
		}
		return x
	case json.Number:
		if n, err := x.Int64(); err == nil {
			if n > 1e12 {
				return n / 1000
			}
			return n
		}
	}
	return 0
}
