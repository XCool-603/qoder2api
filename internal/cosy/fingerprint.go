package cosy

import (
	"crypto/md5"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
)

const (
	legacyDeviceIDEnv = "QD_DEVICE_ID_LEGACY"
	noMachineSaltEnv  = "QD_NO_MACHINE_SALT"
)

func machineSaltDisabled() bool {
	return os.Getenv(noMachineSaltEnv) == "1"
}

var (
	installSaltMu sync.RWMutex
	installSalt   string
)

// SetInstallSalt configures the persistent per-installation salt mixed into derived fingerprints.
func SetInstallSalt(salt string) {
	if machineSaltDisabled() {
		return
	}
	installSaltMu.Lock()
	installSalt = salt
	installSaltMu.Unlock()
}

func saltedSeed(seed string) string {
	if machineSaltDisabled() {
		return seed
	}
	installSaltMu.RLock()
	defer installSaltMu.RUnlock()
	if installSalt == "" {
		return seed
	}
	return seed + "|salt:" + installSalt
}

// FingerprintSeed picks UID when available, falling back to credential hash seed.
func FingerprintSeed(uid, credential string) string {
	if uid != "" {
		return uid
	}
	return "cred:" + credential
}

func rawDeriveID(seed, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + saltedSeed(seed)))
	return fmt.Sprintf("%x", sum)
}

func deriveID(seed, salt string) string {
	hx := rawDeriveID(seed, salt)
	if os.Getenv(legacyDeviceIDEnv) == "1" {
		return hx
	}
	b := []byte(hx)
	b[12] = '4'
	b[16] = 'a'
	return fmt.Sprintf("%s-%s-%s-%s-%s", b[0:8], b[8:12], b[12:16], b[16:20], b[20:32])
}

// DeriveMachineID derives a stable UUIDv4-shaped cosy-machineid.
func DeriveMachineID(seed string) string { return deriveID(seed, "machine") }

// DeriveSessionID derives a stable UUIDv4-shaped session ID.
func DeriveSessionID(seed string) string { return deriveID(seed, "session") }

// DeriveMachineType derives an 18-char hex machine type.
func DeriveMachineType(seed string) string { return rawDeriveID(seed, "machinetype")[:18] }

// DeriveMachineToken derives a 43-char base64url machine token.
func DeriveMachineToken(seed string) string {
	sum := sha512.Sum512([]byte("machinetoken:" + saltedSeed(seed)))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:43]
}

// DeriveMachineHostname derives an isolated virtual hostname (DESKTOP-XXXXXXX).
func DeriveMachineHostname(seed string) string {
	sum := md5.Sum([]byte("hostname:" + saltedSeed(seed)))
	return "DESKTOP-" + strings.ToUpper(fmt.Sprintf("%x", sum)[:7])
}

// MachineOS returns official {arch}_{platform} token (overrideable via QD_COSY_MACHINE_OS).
func MachineOS() string {
	if v := strings.TrimSpace(os.Getenv("QD_COSY_MACHINE_OS")); v != "" {
		if strings.EqualFold(v, "auto") {
			arch := "x86_64"
			if runtime.GOARCH == "arm64" {
				arch = "aarch64"
			}
			plat := "linux"
			switch runtime.GOOS {
			case "windows":
				plat = "win32"
			case "darwin":
				plat = "darwin"
			}
			return arch + "_" + plat
		}
		return v
	}
	if runtime.GOOS == "windows" {
		return "x86_64_win32"
	}
	return "x86_64_linux"
}
