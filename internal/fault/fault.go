package fault

import (
	"encoding/json"
	"errors"
	"fmt"
)

type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Cause     error  `json:"-"`
}

func (e *Error) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	}{
		Code: e.Code, Message: e.Error(), Retryable: e.Retryable,
	})
}

func (e *Error) Error() string {
	if e.Cause == nil {
		return e.Message
	}
	return fmt.Sprintf("%s: %v", e.Message, e.Cause)
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, message string, retryable bool) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable}
}

func Wrap(code, message string, retryable bool, err error) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable, Cause: err}
}

func As(err error) *Error {
	var target *Error
	if errors.As(err, &target) {
		return target
	}
	return Wrap("INTERNAL_ERROR", "unexpected internal error", false, err)
}
