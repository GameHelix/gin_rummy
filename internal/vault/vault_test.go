package vault

import (
	"bytes"
	"strings"
	"testing"
)

func TestSealOpenAndOwnership(t *testing.T) {
	master, _ := NewMasterKey()
	v, err := New(master)
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
	w, _ := New(other)
	s2, _ := v.Seal(owner, secret)
	if _, err := w.Open(owner, s2); err == nil {
		t.Fatal("another master key must not open it")
	}
	if _, err := New("short"); err == nil {
		t.Fatal("a weak master key must be refused")
	}
}

func TestRotatingTheMasterKeyKeepsEveryRowReadable(t *testing.T) {
	k1, _ := NewMasterKey()
	k2, _ := NewMasterKey()
	v1, _ := New(k1)
	owner, secret := []byte("tenant-a/credential-1/binance"), []byte(`{"key":"k","secret":"s"}`)
	old, _ := v1.Seal(owner, secret)
	// During the rotation both keys are loaded: old rows open, new rows
	// seal under version 2.
	both, err := New("2:"+k2, k1)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := both.Open(owner, old); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("an old row opens during the rotation: %v", err)
	}
	if fresh, _ := both.Seal(owner, secret); fresh.MasterVersion != 2 || both.Version() != 2 {
		t.Fatal("new rows seal under the current version")
	}
	moved, err := both.Rewrap(owner, old)
	if err != nil || moved.MasterVersion != 2 || !bytes.Equal(moved.Ciphertext, old.Ciphertext) || bytes.Equal(moved.WrappedKey, old.WrappedKey) {
		t.Fatalf("a rewrap changes only the wrapped data key: %+v %v", moved, err)
	}
	if _, err := both.Rewrap([]byte("tenant-b/credential-1/binance"), old); err == nil {
		t.Fatal("a rewrap checks the owner")
	}
	// Once the old key is dropped, only re-wrapped rows open.
	only2, _ := New("2:" + k2)
	if got, err := only2.Open(owner, moved); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("a re-wrapped row opens with the new key alone: %v", err)
	}
	if _, err := only2.Open(owner, old); err == nil || !strings.Contains(err.Error(), "version 1") {
		t.Fatalf("a row under a dropped key says which version it needs: %v", err)
	}
	for _, bad := range []string{"0:" + k2, "x:" + k2, "2:short"} {
		if _, err := New(bad); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
	if _, err := New("2:"+k2, "2:"+k1); err == nil {
		t.Fatal("a version given twice is refused")
	}
}
