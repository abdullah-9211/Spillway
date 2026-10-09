package secret

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestRoundTripAndFreshNonce(t *testing.T) {
	box, _ := New(key(1))
	plain := []byte(`{"Authorization":"Bearer abc"}`)
	a, _ := box.Encrypt(plain, []byte("tool-1"))
	b, _ := box.Encrypt(plain, []byte("tool-1"))
	if bytes.Equal(a, b) {
		t.Error("two encryptions of the same value must differ (random nonce)")
	}
	if bytes.Contains(a, []byte("abc")) {
		t.Error("the ciphertext must not contain the plaintext")
	}
	got, err := box.Decrypt(a, []byte("tool-1"))
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: %q %v", got, err)
	}
}

func TestRejectsWrongKeyContextTamperingAndShortInput(t *testing.T) {
	box, _ := New(key(1))
	other, _ := New(key(2))
	sealed, _ := box.Encrypt([]byte("secret"), []byte("ctx"))
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	for name, f := range map[string]func() ([]byte, error){
		"another key":   func() ([]byte, error) { return other.Decrypt(sealed, []byte("ctx")) },
		"another row":   func() ([]byte, error) { return box.Decrypt(sealed, []byte("other")) },
		"altered bytes": func() ([]byte, error) { return box.Decrypt(tampered, []byte("ctx")) },
		"too short":     func() ([]byte, error) { return box.Decrypt([]byte{1, 2, 3}, nil) },
		"empty":         func() ([]byte, error) { return box.Decrypt(nil, nil) },
	} {
		if _, err := f(); err != ErrCorrupt {
			t.Errorf("%s: err = %v, want ErrCorrupt", name, err)
		}
	}
}

func TestKeyParsing(t *testing.T) {
	good := base64.StdEncoding.EncodeToString(key(7))
	if k, err := ParseKey(good); err != nil || len(k) != 32 {
		t.Errorf("good key: %v", err)
	}
	for _, bad := range []string{"", "not base64!!", base64.StdEncoding.EncodeToString(key(1)[:16])} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if _, err := New(key(1)[:5]); err == nil || !strings.Contains(err.Error(), "32") {
		t.Errorf("New with a short key: %v", err)
	}
}
