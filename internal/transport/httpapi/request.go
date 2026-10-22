package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

func authenticated(actual, expected string) bool {
	a, b := sha256.Sum256([]byte(actual)), sha256.Sum256([]byte(expected))
	return expected != "" && subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func bearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return token
}

func decode(w http.ResponseWriter, r *http.Request, target any, strict bool) bool {
	if media := strings.Split(r.Header.Get("Content-Type"), ";")[0]; media != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json.")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	if strict {
		decoder.DisallowUnknownFields()
	}
	err := decoder.Decode(target)
	if err == nil {
		var extra any
		if next := decoder.Decode(&extra); next != io.EOF {
			err = errors.New("expected exactly one JSON value")
		}
	}
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large.")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON or request fields.")
		}
		return false
	}
	return true
}
