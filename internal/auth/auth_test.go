package auth

import (
	"strings"
	"testing"
	"time"
)

// Fast settings for tests; production uses DefaultParams.
var testParams = Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 16, SaltLen: 8}

func TestHashAndVerify(t *testing.T) {
	h, err := Hash("correct horse", testParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8,t=1,p=1$") {
		t.Errorf("hash = %s", h)
	}
	for pw, want := range map[string]bool{"correct horse": true, "correct horse ": false, "Correct horse": false, "": false} {
		got, err := Verify(pw, h)
		if err != nil || got != want {
			t.Errorf("Verify(%q) = %v, %v; want %v", pw, got, err, want)
		}
	}
	h2, _ := Hash("correct horse", testParams)
	if h == h2 {
		t.Error("each hash needs its own random salt")
	}
	if strings.Contains(h, "correct horse") {
		t.Error("the hash must not contain the password")
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	good, _ := Hash("x", testParams)
	for name, h := range map[string]string{
		"empty": "", "plain text": "hunter2", "wrong algorithm": strings.Replace(good, "argon2id", "argon2i", 1),
		"wrong version": strings.Replace(good, "v=19", "v=16", 1), "bad params": strings.Replace(good, "m=8,t=1,p=1", "m=x", 1),
		"bad salt": strings.Replace(good, "$", "$!!", 4), "missing hash": good[:strings.LastIndex(good, "$")+1],
	} {
		if ok, err := Verify("x", h); ok || err == nil {
			t.Errorf("%s: ok=%v err=%v; a malformed hash is an error, never a match", name, ok, err)
		}
	}
}

func newTestSigner(t *testing.T) (*Signer, *time.Time) {
	t.Helper()
	s, err := NewSigner([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestTokenRoundTrip(t *testing.T) {
	s, _ := newTestSigner(t)
	tok, exp, err := s.Issue(Claims{UserID: "u1", Username: "admin", Role: RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Unix(1_800_000_000, 0).Add(12 * time.Hour).UTC(); !exp.Equal(want) {
		t.Errorf("expires %v, want %v", exp, want)
	}
	c, err := s.Verify(tok)
	if err != nil || c.Username != "admin" || c.Role != RoleAdmin || c.UserID != "u1" {
		t.Fatalf("claims %+v, err %v", c, err)
	}
}

func TestTokenExpiry(t *testing.T) {
	s, now := newTestSigner(t)
	tok, _, _ := s.Issue(Claims{Username: "a", Role: RoleViewer})
	*now = now.Add(12*time.Hour - time.Second)
	if _, err := s.Verify(tok); err != nil {
		t.Errorf("just before expiry: %v", err)
	}
	*now = now.Add(time.Second)
	if _, err := s.Verify(tok); err != ErrExpiredToken {
		t.Errorf("at expiry: %v, want ErrExpiredToken", err)
	}
}

func TestTokenTampering(t *testing.T) {
	s, _ := newTestSigner(t)
	viewer, _, _ := s.Issue(Claims{Username: "v", Role: RoleViewer})
	admin, _, _ := s.Issue(Claims{Username: "a", Role: RoleAdmin})
	vPayload, vSig, _ := strings.Cut(viewer, ".")
	aPayload, _, _ := strings.Cut(admin, ".")

	other, _ := NewSigner([]byte(strings.Repeat("z", 32)))
	other.now = s.now
	foreign, _, _ := other.Issue(Claims{Username: "a", Role: RoleAdmin})

	tests := map[string]string{
		"empty":                           "",
		"no dot":                          "abc",
		"only payload":                    vPayload + ".",
		"only signature":                  "." + vSig,
		"admin payload, viewer signature": aPayload + "." + vSig, // the privilege-escalation attempt
		"flipped signature byte":          vPayload + "." + flip(vSig),
		"flipped payload byte":            flip(vPayload) + "." + vSig,
		"signed with another secret":      foreign,
		"garbage":                         "!!!.???",
		"extra segment":                   viewer + ".x",
	}
	for name, tok := range tests {
		if _, err := s.Verify(tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func flip(s string) string {
	b := []byte(s)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	return string(b)
}

func TestTokenRejectsUnknownRole(t *testing.T) {
	s, _ := newTestSigner(t)
	tok, _, _ := s.Issue(Claims{Username: "a", Role: "superuser"})
	if _, err := s.Verify(tok); err == nil {
		t.Error("a role other than admin or viewer must be rejected even when correctly signed")
	}
}

func TestShortSecretRejected(t *testing.T) {
	if _, err := NewSigner([]byte("short")); err == nil {
		t.Error("a short secret must be refused")
	}
}

func TestThrottle(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	th := NewThrottle(5, time.Minute)
	th.now = func() time.Time { return now }

	for i := 0; i < 5; i++ {
		if blocked, _ := th.Blocked("1.1.1.1", "admin"); blocked {
			t.Fatalf("blocked after only %d failures", i)
		}
		th.Fail("1.1.1.1", "admin")
		now = now.Add(2 * time.Second)
	}
	blocked, wait := th.Blocked("1.1.1.1", "admin")
	if !blocked || wait <= 0 || wait > time.Minute {
		t.Fatalf("5 failures in a minute must block: blocked=%v wait=%v", blocked, wait)
	}
	// The same username from another address is blocked too, and so is another username from the same address.
	if b, _ := th.Blocked("2.2.2.2", "ADMIN"); !b {
		t.Error("the username limit is per username, case-insensitive, across addresses")
	}
	if b, _ := th.Blocked("1.1.1.1", "viewer"); !b {
		t.Error("the address limit holds across usernames")
	}
	if b, _ := th.Blocked("3.3.3.3", "someone-else"); b {
		t.Error("an unrelated address and username must not be blocked")
	}
	now = now.Add(wait)
	if b, _ := th.Blocked("1.1.1.1", "admin"); b {
		t.Error("blocked past the window")
	}
}

func TestThrottleWindowSlides(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	th := NewThrottle(3, time.Minute)
	th.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		th.Fail("ip", "u")
		now = now.Add(25 * time.Second)
	}
	// Failures were at t=0, 25, 50; now is t=75, so the first has aged out.
	if b, _ := th.Blocked("ip", "u"); b {
		t.Error("only 2 failures remain in the window")
	}
	th.Fail("ip", "u")
	if b, _ := th.Blocked("ip", "u"); !b {
		t.Error("3 in the window blocks")
	}
}

func TestThrottleReset(t *testing.T) {
	th := NewThrottle(2, time.Minute)
	th.Fail("ip", "u")
	th.Fail("ip", "u")
	th.Reset("u")
	if b, _ := th.Blocked("other-ip", "u"); b {
		t.Error("a successful sign-in clears that username's failures")
	}
}
