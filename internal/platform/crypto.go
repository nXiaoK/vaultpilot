package platform

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/crypto/argon2"
)

const formatVersion = 1

// SetMaster installs a verified master key for trusted offline recovery tools.
// HTTP handlers must never expose this capability directly.
func (k *Keyring) SetMaster(key []byte) error { return k.set(key) }

// UnlockRecoveryKit authenticates an offline kit independently of SQLite,
// node keys and OAuth credentials.
func UnlockRecoveryKit(raw []byte, password string) (RecoveryPayload, error) {
	var kit RecoveryKit
	if len(raw) > 16*1024*1024 || json.Unmarshal(raw, &kit) != nil {
		return RecoveryPayload{}, errors.New("恢复包格式错误")
	}
	return openRecovery(kit, password)
}

type Keyring struct {
	mu  sync.RWMutex
	key []byte
}

func (k *Keyring) Unlocked() bool { k.mu.RLock(); defer k.mu.RUnlock(); return len(k.key) == 32 }
func (k *Keyring) Lock() {
	k.mu.Lock()
	defer k.mu.Unlock()
	for i := range k.key {
		k.key[i] = 0
	}
	k.key = nil
}
func (k *Keyring) set(key []byte) error {
	if len(key) != 32 {
		return errors.New("invalid master key")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for i := range k.key {
		k.key[i] = 0
	}
	k.key = append([]byte(nil), key...)
	return nil
}
func (k *Keyring) Seal(b []byte) ([]byte, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if len(k.key) != 32 {
		return nil, errors.New("vault locked")
	}
	return encrypt(k.key, b, []byte("vaultpilot-control-v1"))
}
func (k *Keyring) Open(b []byte) ([]byte, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if len(k.key) != 32 {
		return nil, errors.New("vault locked")
	}
	return decrypt(k.key, b, []byte("vaultpilot-control-v1"))
}
func encrypt(key, plain, aad []byte) ([]byte, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	n := make([]byte, g.NonceSize())
	if _, e = rand.Read(n); e != nil {
		return nil, e
	}
	return g.Seal(n, n, plain, aad), nil
}
func decrypt(key, box, aad []byte) ([]byte, error) {
	block, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	g, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	if len(box) < g.NonceSize()+g.Overhead() {
		return nil, errors.New("invalid encrypted data")
	}
	return g.Open(nil, box[:g.NonceSize()], box[g.NonceSize():], aad)
}
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return b
}
func newID() string { return fmt.Sprintf("%x", randomBytes(16)) }

type PasswordHash struct {
	Salt string `json:"salt"`
	Hash string `json:"hash"`
}

var argonSlots = make(chan struct{}, 2)

func deriveKey(password, salt []byte, time, memory uint32, threads uint8) []byte {
	argonSlots <- struct{}{}
	defer func() { <-argonSlots }()
	return argon2.IDKey(password, salt, time, memory, threads, 32)
}

func hashPassword(password string) PasswordHash {
	s := randomBytes(16)
	h := deriveKey([]byte(password), s, 3, 64*1024, 2)
	return PasswordHash{base64.StdEncoding.EncodeToString(s), base64.StdEncoding.EncodeToString(h)}
}
func verifyPassword(password string, p PasswordHash) bool {
	s, e := base64.StdEncoding.DecodeString(p.Salt)
	if e != nil || len(s) != 16 {
		return false
	}
	want, e := base64.StdEncoding.DecodeString(p.Hash)
	if e != nil || len(want) != 32 {
		return false
	}
	got := deriveKey([]byte(password), s, 3, 64*1024, 2)
	return subtle.ConstantTimeCompare(got, want) == 1
}

type RecoveryKit struct {
	Format      string `json:"format"`
	Version     int    `json:"version"`
	KDF         string `json:"kdf"`
	Salt        string `json:"salt"`
	Time        uint32 `json:"time"`
	Memory      uint32 `json:"memory_kib"`
	Parallelism uint8  `json:"parallelism"`
	Ciphertext  string `json:"ciphertext"`
}
type DiscoveryEntry struct {
	Backend   string `json:"backend"`
	DriveID   string `json:"drive_id,omitempty"`
	Region    string `json:"region,omitempty"`
	TenantID  string `json:"tenant_id,omitempty"`
	LocalPath string `json:"local_path,omitempty"`
	Path      string `json:"path"`
}
type RecoveryPayload struct {
	Version   int              `json:"version"`
	VaultID   string           `json:"vault_id"`
	MasterKey string           `json:"master_key"`
	Entries   []DiscoveryEntry `json:"discovery_entries"`
}

func protectRecovery(p RecoveryPayload, password string) (RecoveryKit, error) {
	kit := RecoveryKit{Format: "vaultpilot-recovery", Version: 1, KDF: "argon2id", Time: 3, Memory: 64 * 1024, Parallelism: 2}
	salt := randomBytes(16)
	kit.Salt = base64.StdEncoding.EncodeToString(salt)
	key := deriveKey([]byte(password), salt, kit.Time, kit.Memory, kit.Parallelism)
	plain, e := json.Marshal(p)
	if e != nil {
		return kit, e
	}
	box, e := encrypt(key, plain, []byte("vaultpilot-recovery-v1"))
	for i := range key {
		key[i] = 0
	}
	if e != nil {
		return kit, e
	}
	kit.Ciphertext = base64.StdEncoding.EncodeToString(box)
	return kit, nil
}
func openRecovery(kit RecoveryKit, password string) (RecoveryPayload, error) {
	var p RecoveryPayload
	if kit.Format != "vaultpilot-recovery" || kit.Version != 1 || kit.KDF != "argon2id" || kit.Time != 3 || kit.Memory != 64*1024 || kit.Parallelism != 2 {
		return p, errors.New("不支持的恢复包格式或 KDF 参数")
	}
	salt, e := base64.StdEncoding.DecodeString(kit.Salt)
	if e != nil || len(salt) != 16 {
		return p, errors.New("恢复包格式错误")
	}
	box, e := base64.StdEncoding.DecodeString(kit.Ciphertext)
	if e != nil || len(box) > 16*1024*1024 {
		return p, errors.New("恢复包格式错误")
	}
	key := deriveKey([]byte(password), salt, kit.Time, kit.Memory, kit.Parallelism)
	plain, e := decrypt(key, box, []byte("vaultpilot-recovery-v1"))
	for i := range key {
		key[i] = 0
	}
	if e != nil {
		return p, errors.New("恢复密码错误或恢复包已损坏")
	}
	defer func() {
		for i := range plain {
			plain[i] = 0
		}
	}()
	if e = json.Unmarshal(plain, &p); e != nil {
		return p, errors.New("恢复资料格式错误")
	}
	master, e := base64.StdEncoding.DecodeString(p.MasterKey)
	vault, vaultErr := hex.DecodeString(p.VaultID)
	if e != nil || len(master) != 32 || vaultErr != nil || len(vault) != 16 || p.Version != 1 {
		return p, errors.New("恢复资料格式错误")
	}
	for i := range master {
		master[i] = 0
	}
	for _, entry := range p.Entries {
		if entry.Backend != "local" && entry.Backend != "onedrive" || entry.Path != "VaultPilot/"+p.VaultID+"/catalog/" {
			return p, errors.New("恢复发现入口格式错误")
		}
	}
	return p, nil
}
