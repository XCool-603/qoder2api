package cosy

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	plain := []byte(`{"model":"qmodel_38max","stream":true,"text":"你好 Qoder"}`)
	enc, err := Encode(plain)
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	dec, err := Decode(enc)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if string(dec) != string(plain) {
		t.Fatalf("mismatch: got %q, want %q", dec, plain)
	}
}

func TestFingerprintStabilityAndSalt(t *testing.T) {
	t.Setenv(noMachineSaltEnv, "")
	SetInstallSalt("")
	a := DeriveMachineID("u123")
	b := DeriveMachineID("u123")
	if a != b || len(a) != 36 {
		t.Fatalf("expected stable 36-char UUIDv4 machine ID, got %q / %q", a, b)
	}
	SetInstallSalt("custom-salt")
	defer SetInstallSalt("")
	c := DeriveMachineID("u123")
	if c == a {
		t.Fatalf("expected install salt to isolate derived fingerprint")
	}
}

func TestNewSessionAndSign(t *testing.T) {
	sess, err := NewSession(AuthIdentity{
		Name:               "tester",
		Aid:                "uid_1",
		Uid:                "uid_1",
		SecurityOauthToken: "dt-abc",
	}, DeriveMachineID("uid_1"), DeriveMachineToken("uid_1"), DeriveMachineType("uid_1"))
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if sess.CosyKey == "" || sess.Info == "" {
		t.Fatalf("expected non-empty CosyKey and Info")
	}
	payloadB64 := BuildPayloadB64(sess.Info)
	sig := SignRequest(payloadB64, sess.CosyKey, "1700000000", "body", "/api/v2/model/list")
	if len(sig) != 32 {
		t.Fatalf("expected 32-hex MD5 signature, got %q", sig)
	}
}

func TestDecryptQMCCatalog(t *testing.T) {
	uid := "uid_test_qmc"
	key := hkdfSHA256([]byte(uid), []byte("qoder-model-cache-enc"), []byte("model-cache-v1"), 32)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, 12)
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	plain := []byte(`{"chat":[{"key":"qmodel_38max"}]}`)
	sealed := gcm.Seal(nil, nonce, plain, nil)
	env := append([]byte("QMC\x01"), nonce...)
	env = append(env, sealed...)
	dec, err := DecryptQMCCatalog(base64.StdEncoding.EncodeToString(env), uid)
	if err != nil {
		t.Fatalf("DecryptQMCCatalog failed: %v", err)
	}
	if string(dec) != string(plain) {
		t.Fatalf("got %q, want %q", dec, plain)
	}
}
