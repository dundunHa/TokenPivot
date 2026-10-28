package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestArgon2idEncoding pins the encoded algorithm, version and parameters so the
// migration file, the documentation and the code cannot drift apart.
func TestArgon2idEncoding(t *testing.T) {
	encoded, err := HashPassword("synthetic-correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		t.Fatalf("unexpected encoding shape: %q", encoded)
	}
	if parts[2] != "v=19" {
		t.Fatalf("version = %q, want v=19", parts[2])
	}
	if parts[3] != "m=19456,t=2,p=1" {
		t.Fatalf("parameters = %q, want m=19456,t=2,p=1", parts[3])
	}
	params, err := parseHash(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(params.salt) != 16 {
		t.Fatalf("salt length = %d, want 16", len(params.salt))
	}
	if len(params.key) != 32 {
		t.Fatalf("key length = %d, want 32", len(params.key))
	}
	if strings.Contains(encoded, "synthetic-correct-horse") {
		t.Fatal("encoding contains the password")
	}
}

func TestHashPasswordUsesFreshSalt(t *testing.T) {
	first, err := HashPassword("synthetic-same-password")
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashPassword("synthetic-same-password")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("identical passwords produced identical encodings")
	}
}

func TestVerifyPassword(t *testing.T) {
	encoded, err := HashPassword("synthetic-correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyPassword(encoded, "synthetic-correct-horse")
	if err != nil || !ok {
		t.Fatalf("correct password rejected: ok=%t err=%v", ok, err)
	}
	ok, err = VerifyPassword(encoded, "synthetic-correct-horse ")
	if err != nil || ok {
		t.Fatalf("wrong password accepted or errored: ok=%t err=%v", ok, err)
	}
	// Passwords are never trimmed or normalized before hashing.
	ok, err = VerifyPassword(encoded, " Synthetic-Correct-Horse")
	if err != nil || ok {
		t.Fatalf("altered case or whitespace accepted: ok=%t err=%v", ok, err)
	}
}

func TestVerifyPasswordRejectsMalformedEncoding(t *testing.T) {
	valid, err := HashPassword("synthetic-correct-horse")
	if err != nil {
		t.Fatal(err)
	}
	for name, encoded := range map[string]string{
		"empty":            "",
		"no prefix":        strings.TrimPrefix(valid, "$"),
		"wrong algorithm":  strings.Replace(valid, "argon2id", "argon2i", 1),
		"wrong version":    strings.Replace(valid, "v=19", "v=16", 1),
		"missing parts":    "$argon2id$v=19$m=19456,t=2,p=1",
		"extra part":       valid + "$extra",
		"bad salt":         strings.Replace(valid, valid[strings.LastIndex(valid, "$")+1:], "!!!", 1),
		"memory too high":  strings.Replace(valid, "m=19456", "m=99999999", 1),
		"iterations zero":  strings.Replace(valid, "t=2", "t=0", 1),
		"parallelism zero": strings.Replace(valid, "p=1", "p=0", 1),
		"short key":        strings.Replace(valid, valid[strings.LastIndex(valid, "$")+1:], "AAAA", 1),
		"non-numeric":      strings.Replace(valid, "m=19456", "m=+19456", 1),
	} {
		t.Run(name, func(t *testing.T) {
			ok, err := VerifyPassword(encoded, "synthetic-correct-horse")
			if !errors.Is(err, ErrInvalidHash) {
				t.Fatalf("err = %v, want ErrInvalidHash", err)
			}
			if ok {
				t.Fatal("malformed encoding accepted a password")
			}
		})
	}
}

func TestDummyPasswordHashIsStableAndUnrelated(t *testing.T) {
	first := dummyPasswordHash()
	second := dummyPasswordHash()
	if first != second {
		t.Fatal("dummy hash was recomputed with a new salt")
	}
	params, err := parseHash(first)
	if err != nil {
		t.Fatal(err)
	}
	if params.memory != argonMemory || params.iterations != argonIterations || params.parallelism != argonParallelism {
		t.Fatal("dummy hash does not use production parameters")
	}
	ok, err := VerifyPassword(first, "tokenpivot-dummy-password")
	if err != nil || !ok {
		t.Fatal("dummy hash is not verifiable")
	}
}

// TestPasswordHashingDuration records the measured Argon2id cost so a release
// can see the real number; the parameters stay fixed regardless of the result.
func TestPasswordHashingDuration(t *testing.T) {
	start := time.Now()
	encoded, err := HashPassword("synthetic-duration-measurement")
	hashDuration := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	ok, err := VerifyPassword(encoded, "synthetic-duration-measurement")
	verifyDuration := time.Since(start)
	if err != nil || !ok {
		t.Fatalf("verification failed: ok=%t err=%v", ok, err)
	}
	t.Logf("argon2id m=%d,t=%d,p=%d hash=%s verify=%s", argonMemory, argonIterations, argonParallelism, hashDuration, verifyDuration)
}
