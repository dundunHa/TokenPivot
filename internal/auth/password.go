package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. They are the OWASP minimum recommendation, not a
// benchmarked local optimum; changing them is a deliberate release decision and
// never happens per request.
const (
	argonMemory      uint32 = 19456 // KiB, 19 MiB
	argonIterations  uint32 = 2
	argonParallelism uint8  = 1
	argonSaltLength         = 16
	argonKeyLength   uint32 = 32
)

// Bounds for parameters read back from a stored encoding. They stop a corrupted
// or hostile row from turning verification into unbounded work.
const (
	argonMaxMemory      uint64 = 1024 * 1024 // 1 GiB in KiB
	argonMinMemory      uint64 = 8 * 1024
	argonMaxIterations  uint64 = 16
	argonMaxParallelism uint64 = 16
	argonMinSaltLength  uint64 = 8
	argonMinKeyLength   uint64 = 16
)

// ErrInvalidHash reports a stored Argon2id encoding that cannot be parsed under
// the accepted bounds. It is a server data fault, never a credential error.
var ErrInvalidHash = errors.New("auth: invalid password hash encoding")

// HashPassword derives a new Argon2id encoding with a fresh 16-byte salt. The
// returned value includes the algorithm, version, parameters, salt and key, so
// verification needs no other state.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", errors.New("auth: password salt generation failed")
	}
	return encodeHash(salt, deriveKey(password, salt, argonMemory, argonIterations, argonParallelism, argonKeyLength)), nil
}

// VerifyPassword parses an Argon2id encoding and compares the derived key in
// constant time. A malformed encoding yields ErrInvalidHash; a wrong password
// yields false with no error.
func VerifyPassword(encoded, password string) (bool, error) {
	params, err := parseHash(encoded)
	if err != nil {
		return false, err
	}
	key := deriveKey(password, params.salt, params.memory, params.iterations, params.parallelism, uint32(len(params.key)))
	return subtle.ConstantTimeCompare(key, params.key) == 1, nil
}

func deriveKey(password string, salt []byte, memory, iterations uint32, parallelism uint8, keyLength uint32) []byte {
	return argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, keyLength)
}

// dummyPasswordHash is a precomputed Argon2id encoding of a fixed, publicly known
// value that is not any account's password. Login verifies against it when the
// email is unknown so both branches pay exactly one derivation and take
// comparable time; because the encoding is a constant, the unknown-email branch
// derives only during that single verification and never while building it.
//
// Parameters and shape match HashPassword: m=19456, t=2, p=1, v=19, a 16-byte
// salt and a 32-byte key, in the canonical six-part encoding. The salt decodes to
// the ASCII bytes "tokenpivot-dummy"; regenerating it is a deliberate change that
// TestDummyPasswordHashIsPrecomputedAndFixed pins.
const dummyPasswordHash = "$argon2id$v=19$m=19456,t=2,p=1$dG9rZW5waXZvdC1kdW1teQ$gupD8SM36KGNDefu5FFdqMOmmdE06Zo0pr35tTcIGDo"

// The dummy inputs are fixed and public on purpose: they identify no account.
// A test recomputes the encoding from them, proving the constant above is that
// precomputation rather than an ad-hoc value.
const (
	dummyPassword = "tokenpivot-dummy-password"
	dummySalt     = "tokenpivot-dummy" // exactly 16 ASCII bytes
)

func encodeHash(salt, key []byte) string {
	encoding := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonIterations, argonParallelism, encoding.EncodeToString(salt), encoding.EncodeToString(key))
}

// argonParams is one parsed Argon2id encoding.
type argonParams struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
	salt        []byte
	key         []byte
}

// parseHash accepts only the canonical six-part Argon2id encoding and rejects
// any value that is not exactly one of its parts.
func parseHash(encoded string) (argonParams, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return argonParams{}, ErrInvalidHash
	}
	version, ok := parseDecimal(strings.TrimPrefix(parts[2], "v="), strings.HasPrefix(parts[2], "v="))
	if !ok || version != argon2.Version {
		return argonParams{}, ErrInvalidHash
	}
	parameters := strings.Split(parts[3], ",")
	if len(parameters) != 3 {
		return argonParams{}, ErrInvalidHash
	}
	memory, okMemory := parseDecimal(strings.TrimPrefix(parameters[0], "m="), strings.HasPrefix(parameters[0], "m="))
	iterations, okIterations := parseDecimal(strings.TrimPrefix(parameters[1], "t="), strings.HasPrefix(parameters[1], "t="))
	parallelism, okParallelism := parseDecimal(strings.TrimPrefix(parameters[2], "p="), strings.HasPrefix(parameters[2], "p="))
	if !okMemory || !okIterations || !okParallelism {
		return argonParams{}, ErrInvalidHash
	}
	if memory < argonMinMemory || memory > argonMaxMemory || iterations < 1 || iterations > argonMaxIterations || parallelism < 1 || parallelism > argonMaxParallelism {
		return argonParams{}, ErrInvalidHash
	}
	encoding := base64.RawStdEncoding
	salt, errSalt := encoding.DecodeString(parts[4])
	key, errKey := encoding.DecodeString(parts[5])
	if errSalt != nil || errKey != nil || uint64(len(salt)) < argonMinSaltLength || uint64(len(key)) < argonMinKeyLength || uint64(len(key)) > uint64(argonKeyLength) {
		return argonParams{}, ErrInvalidHash
	}
	return argonParams{memory: uint32(memory), iterations: uint32(iterations), parallelism: uint8(parallelism), salt: salt, key: key}, nil
}

// parseDecimal accepts only a non-empty run of ASCII digits, so a value such as
// "+2", " 2" or "2x" is rejected instead of being silently coerced.
func parseDecimal(value string, prefixed bool) (uint64, bool) {
	if !prefixed || value == "" {
		return 0, false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, false
	}
	return parsed, true
}
