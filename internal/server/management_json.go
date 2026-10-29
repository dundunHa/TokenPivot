package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"unicode"
	"unicode/utf8"
)

// maxManagementBodyBytes bounds project and source request bodies. It is
// independent of the 1 MiB ingestion limit.
const maxManagementBodyBytes = 16 << 10

// maxManagementLabelBytes is the longest accepted machine identifier or name.
const maxManagementLabelBytes = 128

// readManagementBody enforces the media type, the 16 KiB limit and the strict
// JSON rules for the project and source request bodies, then returns the decoded
// string fields. Only the names in allowed are known keys; the names in required
// must be present, and when required is empty at least one field must be. Each
// value must pass validManagementLabel. Messages are fixed and never repeat a
// request value.
func readManagementBody(w http.ResponseWriter, r *http.Request, allowed, required []string) (map[string]string, bool) {
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "content type must be application/json")
		return nil, false
	}
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be one JSON object")
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxManagementBodyBytes))
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "request body is too large")
			return nil, false
		}
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body could not be read")
		return nil, false
	}
	if !utf8.Valid(body) {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be valid UTF-8")
		return nil, false
	}
	fields, ok := decodeManagementFields(body, allowed)
	if !ok {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "request body must be one JSON object with only the documented string fields and no duplicate keys or trailing content")
		return nil, false
	}
	for _, name := range required {
		if _, present := fields[name]; !present {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "a required field is missing")
			return nil, false
		}
	}
	if len(required) == 0 && len(fields) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "at least one field is required")
		return nil, false
	}
	for _, value := range fields {
		if !validManagementLabel(value) {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "a field value is not acceptable")
			return nil, false
		}
	}
	return fields, true
}

// decodeManagementFields parses exactly one JSON object whose keys are all in
// allowed and whose values are all strings. Keys are compared exactly after
// decoding (encoding/json would match them case-insensitively), a repeated
// decoded key, a null or non-string value, and any trailing content are refused.
func decodeManagementFields(body []byte, allowed []string) (map[string]string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	token, err := decoder.Token()
	if err != nil {
		return nil, false
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, false
	}
	fields := make(map[string]string, len(allowed))
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyToken.(string)
		if !ok || !containsString(allowed, key) {
			return nil, false
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, false
		}
		valueToken, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		value, ok := valueToken.(string)
		if !ok {
			return nil, false
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return fields, true
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// validManagementLabel is the same observable rule as the ingestion labels:
// 1-128 UTF-8 bytes, no control characters, no leading or trailing Unicode
// white space, and no trimming or case folding.
func validManagementLabel(text string) bool {
	if len(text) < 1 || len(text) > maxManagementLabelBytes {
		return false
	}
	for _, character := range text {
		if character <= 0x1f || (character >= 0x7f && character <= 0x9f) {
			return false
		}
	}
	first, _ := utf8.DecodeRuneInString(text)
	last, _ := utf8.DecodeLastRuneInString(text)
	return !unicode.IsSpace(first) && !unicode.IsSpace(last)
}
