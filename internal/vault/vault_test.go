package vault

import (
	"bytes"
	"testing"
)

func TestSealOpenAndOwnership(t *testing.T) {
	master, _ := NewMasterKey()
	v, err := New(master, 1)
	if err != nil {
		t.Fatal(err)
	}
	owner := []byte("tenant-a/credential-1/binance")
	secret := []byte(`{"key":"k","secret":"s"}`)
	s, err := v.Seal(owner, secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(s.Ciphertext, []byte("secret")) {
		t.Fatal("the ciphertext must not contain the plaintext")
	}
	got, err := v.Open(owner, s)
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("round trip: %s %v", got, err)
	}
	if _, err := v.Open([]byte("tenant-b/credential-1/binance"), s); err == nil {
		t.Fatal("a sealed key copied to another owner must not open")
	}
	s.Ciphertext[0] ^= 1
	if _, err := v.Open(owner, s); err == nil {
		t.Fatal("a tampered ciphertext must not open")
	}
	other, _ := NewMasterKey()
	w, _ := New(other, 1)
	s2, _ := v.Seal(owner, secret)
	if _, err := w.Open(owner, s2); err == nil {
		t.Fatal("another master key must not open it")
	}
	if _, err := New("short", 1); err == nil {
		t.Fatal("a weak master key must be refused")
	}
}
