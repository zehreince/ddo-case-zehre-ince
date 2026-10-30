// Package httpx API genelinde ortak JSON yanıt ve hata biçimini sağlar:
//
//	{"error":{"code":"...","message":"..."}}
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// Hata kodları istemcinin dallanabileceği sabit değerlerdir; mesaj ise insan içindir.
const (
	CodeBadRequest    = "bad_request"
	CodeValidation    = "validation_failed"
	CodeUnauthorized  = "unauthorized"
	CodeForbidden     = "forbidden"
	CodeNotFound      = "not_found"
	CodeConflict      = "conflict"
	CodeUnavailable   = "service_unavailable"
	CodeInternal      = "internal_error"
	CodeMethodInvalid = "method_not_allowed"
)

type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

func Error(w http.ResponseWriter, status int, code, message string) {
	JSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: message}})
}

const maxBodyBytes = 1 << 20

// Decode gövdeyi sıkı biçimde çözer: bilinmeyen alan ve birden fazla JSON değeri reddedilir.
func Decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("gövde tek bir JSON nesnesi olmalı")
	}
	return nil
}

func NotFound(w http.ResponseWriter, _ *http.Request) {
	Error(w, http.StatusNotFound, CodeNotFound, "kaynak bulunamadı")
}

func MethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	Error(w, http.StatusMethodNotAllowed, CodeMethodInvalid, "bu uç nokta bu yöntemi desteklemiyor")
}
