package cosy

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

const (
	// Version aligns with official qodercli 1.1.75 (overrideable via QD_COSY_VERSION).
	DefaultCosyVersion = "1.1.75"
	DefaultClientType  = "5"
	DefaultUserAgent   = "undici"
	DefaultUserType    = "personal_professional_trial"
)

func CosyVersion() string {
	if v := strings.TrimSpace(os.Getenv("QD_COSY_VERSION")); v != "" {
		return v
	}
	return DefaultCosyVersion
}

func CosyClientType() string {
	if v := strings.TrimSpace(os.Getenv("QD_COSY_CLIENTTYPE")); v != "" {
		return v
	}
	return DefaultClientType
}

func CosyUserAgent() string {
	if v := strings.TrimSpace(os.Getenv("QD_COSY_UA")); v != "" {
		return v
	}
	return DefaultUserAgent
}

const ServerPubKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// AuthIdentity holds the 9 fields encrypted inside COSY's info block.
type AuthIdentity struct {
	Name               string
	Aid                string
	Uid                string
	YxUid              string
	OrganizationId     string
	OrganizationName   string
	UserType           string
	SecurityOauthToken string
	RefreshToken       string
}

// SessionContext holds the RSA-wrapped AES session key and encrypted identity info.
type SessionContext struct {
	TempKey      []byte
	CosyKey      string
	Info         string
	Identity     AuthIdentity
	MachineId    string
	MachineToken string
	MachineType  string
	Hostname     string
}

// NewSession constructs a new COSY encrypted session context for an account.
func NewSession(id AuthIdentity, machineId, machineToken, machineType string) (*SessionContext, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	tempKey := []byte(fmt.Sprintf("%x", raw)[:16])

	cosyKeyBytes, err := rsaEncrypt(tempKey)
	if err != nil {
		return nil, err
	}
	cosyKey := base64.StdEncoding.EncodeToString(cosyKeyBytes)

	if id.UserType == "" {
		id.UserType = DefaultUserType
	}
	payloadBytes := authPayloadJSON(id)
	encPayload, err := aesCBCEncrypt(payloadBytes, tempKey)
	if err != nil {
		return nil, err
	}
	info := base64.StdEncoding.EncodeToString(encPayload)

	return &SessionContext{
		TempKey:      tempKey,
		CosyKey:      cosyKey,
		Info:         info,
		Identity:     id,
		MachineId:    machineId,
		MachineToken: machineToken,
		MachineType:  machineType,
		Hostname:     DeriveMachineHostname(FingerprintSeed(id.Uid, id.SecurityOauthToken)),
	}, nil
}

// SignRequest computes the COSY request MD5 signature over payload, key, timestamp, body, and path.
func SignRequest(payloadB64, cosyKey, cosyDate, body, pathWithoutAlgo string) string {
	s := payloadB64 + "\n" + cosyKey + "\n" + cosyDate + "\n" + body + "\n" + pathWithoutAlgo
	return Md5Hex(s)
}

// BuildPayloadB64 builds the deterministic sorted JSON payload for Bearer COSY.<payload>.<sig>.
func BuildPayloadB64(info string) string {
	m := map[string]string{
		"cosyVersion": CosyVersion(),
		"ideVersion":  "",
		"info":        info,
		"requestId":   NewUUID(),
		"version":     "v1",
	}
	return base64.StdEncoding.EncodeToString([]byte(marshalSortedStringMap(m)))
}

// ComposeBearer builds the Bearer COSY header value.
func ComposeBearer(payloadB64, sig string) string {
	return "Bearer COSY." + payloadB64 + "." + sig
}

func marshalSortedStringMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		vb, _ := json.Marshal(m[k])
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return sb.String()
}

func authPayloadJSON(id AuthIdentity) []byte {
	m := map[string]string{
		"name":                 id.Name,
		"aid":                  id.Aid,
		"uid":                  id.Uid,
		"yx_uid":               id.YxUid,
		"organization_id":      id.OrganizationId,
		"organization_name":    id.OrganizationName,
		"user_type":            id.UserType,
		"security_oauth_token": id.SecurityOauthToken,
		"refresh_token":        id.RefreshToken,
	}
	return []byte(marshalSortedStringMap(m))
}

func rsaEncrypt(plaintext []byte) ([]byte, error) {
	pemData := strings.ReplaceAll(ServerPubKeyPEM, "\r", "")
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("not RSA public key")
	}
	return rsa.EncryptPKCS1v15(rand.Reader, rsaPub, plaintext)
}

func aesCBCEncrypt(plain, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	pad := bs - len(plain)%bs
	padded := append([]byte(nil), plain...)
	padded = append(padded, bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	mode := cipher.NewCBCEncrypter(block, key)
	mode.CryptBlocks(out, padded)
	return out, nil
}

// SessionCache caches COSY sessions per UID and invalidates automatically when access token changes.
type SessionCache struct {
	mu      sync.Mutex
	entries map[string]cachedSession
}

type cachedSession struct {
	token string
	sess  *SessionContext
}

// NewSessionCache returns a thread-safe SessionCache.
func NewSessionCache() *SessionCache {
	return &SessionCache{entries: make(map[string]cachedSession)}
}

// Get returns a cached SessionContext for id, building one if absent or access token changed.
func (c *SessionCache) Get(id AuthIdentity) (*SessionContext, error) {
	token := id.SecurityOauthToken
	if token == "" {
		return nil, fmt.Errorf("cosy: empty access token")
	}
	key := id.Uid
	if key == "" && len(token) >= 16 {
		key = token[:16]
	} else if key == "" {
		key = token
	}
	c.mu.Lock()
	if hit, ok := c.entries[key]; ok && hit.token == token {
		c.mu.Unlock()
		return hit.sess, nil
	}
	c.mu.Unlock()

	seed := FingerprintSeed(id.Uid, token)
	sess, err := NewSession(id, DeriveMachineID(seed), DeriveMachineToken(seed), DeriveMachineType(seed))
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[key] = cachedSession{token: token, sess: sess}
	c.mu.Unlock()
	return sess, nil
}

// Invalidate removes any cached session for uid (e.g. after token refresh).
func (c *SessionCache) Invalidate(uid string) {
	c.mu.Lock()
	delete(c.entries, uid)
	c.mu.Unlock()
}
