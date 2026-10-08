package keys

import (
	"bytes"
	"strings"
	"testing"
)

func TestGenerate(t *testing.T) {
	a, ha, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := Generate()
	if a == b {
		t.Fatal("keys must be random")
	}
	if !strings.HasPrefix(a, "spw_") || len(a) != 4+48 {
		t.Errorf("unexpected key shape %q", a)
	}
	if !bytes.Equal(ha, Hash(a)) || len(ha) != 32 {
		t.Error("hash must be the sha256 of the full key")
	}
	if strings.Contains(string(ha), a[4:]) {
		t.Error("hash must not contain the key")
	}
}
