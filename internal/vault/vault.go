// Package vault encrypts users' keys at rest with envelope encryption: each
// secret gets its own random data key, and only that data key is encrypted
// with the engine's master key. The additional data binds a ciphertext to
// its owner, so a row copied to another tenant or kind will not decrypt.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

type Vault struct {
	master  cipher.AEAD
	version int
}

// Sealed is a secret as stored: nothing in it reveals the plaintext.
type Sealed struct {
	Ciphertext    []byte
	Nonce         []byte
	WrappedKey    []byte
	KeyNonce      []byte
	MasterVersion int
}

// New takes the master key as standard base64 of 32 random bytes.
func New(masterKey string, version int) (*Vault, error) {
	key, err := base64.StdEncoding.DecodeString(masterKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("ENGINE_MASTER_KEY must be 32 random bytes in base64")
	}
	aead, err := gcm(key)
	if err != nil {
		return nil, err
	}
	return &Vault{master: aead, version: version}, nil
}

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
	s := Sealed{MasterVersion: v.version}
	if s.Nonce, err = random(data.NonceSize()); err != nil {
		return Sealed{}, err
	}
	if s.KeyNonce, err = random(v.master.NonceSize()); err != nil {
		return Sealed{}, err
	}
	s.Ciphertext = data.Seal(nil, s.Nonce, secret, owner)
	s.WrappedKey = v.master.Seal(nil, s.KeyNonce, dataKey, owner)
	return s, nil
}

func (v *Vault) Open(owner []byte, s Sealed) ([]byte, error) {
	if s.MasterVersion != v.version {
		return nil, fmt.Errorf("sealed with master key version %d, engine has %d", s.MasterVersion, v.version)
	}
	dataKey, err := v.master.Open(nil, s.KeyNonce, s.WrappedKey, owner)
	if err != nil {
		return nil, errors.New("cannot decrypt key: wrong master key or tampered record")
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

// NewMasterKey returns a fresh master key in the form New expects.
func NewMasterKey() (string, error) {
	key, err := random(32)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}
