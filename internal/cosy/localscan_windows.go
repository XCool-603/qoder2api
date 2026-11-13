//go:build windows

package cosy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func dpapiUnprotect(encrypted []byte) ([]byte, error) {
	if len(encrypted) == 0 {
		return nil, fmt.Errorf("empty DPAPI input")
	}
	in := dataBlob{cbData: uint32(len(encrypted)), pbData: &encrypted[0]}
	var out dataBlob
	r, _, err := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("CryptUnprotectData failed: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	res := unsafe.Slice(out.pbData, out.cbData)
	cp := make([]byte, len(res))
	copy(cp, res)
	return cp, nil
}

func scanDesktopAppAuth(realm, domain, appDirName string) *LocalCredentialCandidate {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		home, _ := os.UserHomeDir()
		if home == "" {
			return nil
		}
		appData = filepath.Join(home, "AppData", "Roaming")
	}
	dir := filepath.Join(appData, appDirName)
	authPath := filepath.Join(dir, "auth.v1.dat")
	if _, err := os.Stat(authPath); err != nil {
		return nil
	}
	cand := &LocalCredentialCandidate{
		Kind:   "app",
		Path:   authPath,
		File:   "auth.v1.dat",
		Realm:  realm,
		Domain: domain,
	}

	stateBytes, err := os.ReadFile(filepath.Join(dir, "Local State"))
	if err != nil {
		cand.Error = err.Error()
		return cand
	}
	var state struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(stateBytes, &state); err != nil || state.OSCrypt.EncryptedKey == "" {
		cand.Error = "Local State missing os_crypt.encrypted_key"
		return cand
	}
	keyBlob, err := base64.StdEncoding.DecodeString(state.OSCrypt.EncryptedKey)
	if err != nil || len(keyBlob) < 5 || string(keyBlob[:5]) != "DPAPI" {
		cand.Error = "invalid encrypted_key header"
		return cand
	}
	aesKey, err := dpapiUnprotect(keyBlob[5:])
	if err != nil {
		cand.Error = err.Error()
		return cand
	}
	encDat, err := os.ReadFile(authPath)
	if err != nil {
		cand.Error = err.Error()
		return cand
	}
	plain, err := ChromiumDecryptV10(encDat, aesKey)
	if err != nil {
		cand.Error = err.Error()
		return cand
	}
	var doc struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    any    `json:"expiresAt"`
		User         struct {
			ID               string `json:"id"`
			Name             string `json:"name"`
			UserType         string `json:"userType"`
			UserTypeSnake    string `json:"user_type"`
			OrganizationID   string `json:"organization_id"`
			OrganizationName string `json:"organization_name"`
		} `json:"user"`
	}
	if err := json.Unmarshal(plain, &doc); err != nil || doc.Token == "" {
		cand.Error = "unexpected auth.v1.dat schema"
		return cand
	}

	var exp int64
	switch v := doc.ExpiresAt.(type) {
	case string:
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
			exp = t.Unix()
		}
	default:
		exp = extractInt64(v)
	}

	cand.Readable = true
	cand.Valid = strings.HasPrefix(doc.Token, "dt-") || doc.RefreshToken != ""
	cand.UID = doc.User.ID
	cand.Nickname = doc.User.Name
	cand.ExpiresAt = exp
	cand.AccessToken = doc.Token
	cand.RefreshToken = doc.RefreshToken
	cand.UserType = doc.User.UserType
	if cand.UserType == "" {
		cand.UserType = doc.User.UserTypeSnake
	}
	cand.OrganizationID = doc.User.OrganizationID
	cand.OrganizationName = doc.User.OrganizationName
	return cand
}
