// Package vault encrypts users' keys at rest with envelope encryption: each
// secret gets its own random data key, and only that data key is encrypted
// with the engine's master key. The additional data binds a ciphertext to
// its owner, so a row copied to another tenant or kind will not decrypt.
// Master keys carry a version, so the key can be rotated: a row sealed
// under an older key still opens while that key is loaded, and Rewrap
// moves it to the current key without touching its secret.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type Vault struct {
	version int
	keys    map[int]cipher.AEAD
}

// Sealed is a secret as stored: nothing in it reveals the plaintext.
type Sealed struct {
	Ciphertext    []byte
	Nonce         []byte
	WrappedKey    []byte
	KeyNonce      []byte
	MasterVersion int
}

// New takes the current master key and any older ones still needed to
// open stored rows. A key is VERSION:BASE64, the base64 of 32 random
// bytes; without a version it is version 1. New rows seal under the
// current key, and each row opens with the key of its own version.
func New(current string, previous ...string) (*Vault, error) {
	v := &Vault{keys: map[int]cipher.AEAD{}}
	var err error
	if v.version, err = v.add(current); err != nil {
		return nil, fmt.Errorf("ENGINE_MASTER_KEY: %w", err)
	}
	for _, p := range previous {
		if _, err := v.add(p); err != nil {
			return nil, fmt.Errorf("ENGINE_MASTER_KEY_PREVIOUS: %w", err)
		}
	}
	return v, nil
}

func (v *Vault) add(s string) (int, error) {
	version := 1
	if pre, rest, ok := strings.Cut(s, ":"); ok {
		n, err := strconv.Atoi(pre)
		if err != nil || n < 1 {
			return 0, errors.New("a master key is VERSION:BASE64, or the base64 alone for version 1")
		}
		version, s = n, rest
	}
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(key) != 32 {
		return 0, errors.New("a master key is 32 random bytes in base64")
	}
	if _, dup := v.keys[version]; dup {
		return 0, fmt.Errorf("master key version %d is given twice", version)
	}
	if v.keys[version], err = gcm(key); err != nil {
		return 0, err
	}
	return version, nil
}

// Version is the master key version new rows are sealed under.
func (v *Vault) Version() int { return v.version }

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func random(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return b, err
}

func (v *Vault) Seal(owner []byte, secret []byte) (Sealed, error) {
	dataKey, err := random(32)
	if err != nil {
		return Sealed{}, err
	}
	defer clear(dataKey)
	data, err := gcm(dataKey)
	if err != nil {
		return Sealed{}, err
	}
	master := v.keys[v.version]
	s := Sealed{MasterVersion: v.version}
	if s.Nonce, err = random(data.NonceSize()); err != nil {
		return Sealed{}, err
	}
	if s.KeyNonce, err = random(master.NonceSize()); err != nil {
		return Sealed{}, err
	}
	s.Ciphertext = data.Seal(nil, s.Nonce, secret, owner)
	s.WrappedKey = master.Seal(nil, s.KeyNonce, dataKey, owner)
	return s, nil
}

// unwrap recovers a row's data key with the master key of its version.
func (v *Vault) unwrap(owner []byte, s Sealed) ([]byte, error) {
	master, ok := v.keys[s.MasterVersion]
	if !ok {
		return nil, fmt.Errorf("sealed with master key version %d, which the engine does not have", s.MasterVersion)
	}
	dataKey, err := master.Open(nil, s.KeyNonce, s.WrappedKey, owner)
	if err != nil {
		return nil, errors.New("cannot decrypt key: wrong master key or tampered record")
	}
	return dataKey, nil
}

func (v *Vault) Open(owner []byte, s Sealed) ([]byte, error) {
	dataKey, err := v.unwrap(owner, s)
	if err != nil {
		return nil, err
	}
	defer clear(dataKey)
	data, err := gcm(dataKey)
	if err != nil {
		return nil, err
	}
	plain, err := data.Open(nil, s.Nonce, s.Ciphertext, owner)
	if err != nil {
		return nil, errors.New("cannot decrypt key: tampered record")
	}
	return plain, nil
}

// Rewrap seals a row's data key under the current master key, so the row
// opens once its old key is gone. The secret and its ciphertext stay as
// they are.
func (v *Vault) Rewrap(owner []byte, s Sealed) (Sealed, error) {
	dataKey, err := v.unwrap(owner, s)
	if err != nil {
		return Sealed{}, err
	}
	defer clear(dataKey)
	master := v.keys[v.version]
	out := s
	out.MasterVersion = v.version
	if out.KeyNonce, err = random(master.NonceSize()); err != nil {
		return Sealed{}, err
	}
	out.WrappedKey = master.Seal(nil, out.KeyNonce, dataKey, owner)
	return out, nil
}

// NewMasterKey returns a fresh master key in the form New expects, as
// version 1; prefix it with "VERSION:" for a rotation.
func NewMasterKey() (string, error) {
	key, err := random(32)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}
