package modbus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"

	gridmodbus "github.com/grid-x/modbus"
)

// ErrorKind identifies how a request failed and whether retrying is safe.
type ErrorKind string

const (
	ErrorProtocolException ErrorKind = "protocol_exception"
	ErrorTransportTimeout  ErrorKind = "transport_timeout"
	ErrorTransportClosed   ErrorKind = "transport_closed"
	ErrorContextDeadline   ErrorKind = "context_deadline"
	ErrorContextCanceled   ErrorKind = "context_canceled"
	ErrorLocal             ErrorKind = "local_error"
)

// RequestError carries the classified request failure.
type RequestError struct {
	Kind          ErrorKind
	ExceptionCode byte
	Attempts      int
	Err           error
}

type validationError struct {
	exceptionCode byte
	err           error
}

type upstreamCommunicationError struct {
	err error
}

func (e *validationError) Error() string {
	return e.err.Error()
}

func (e *validationError) Unwrap() error {
	return e.err
}

func (e *upstreamCommunicationError) Error() string {
	return e.err.Error()
}

func (e *upstreamCommunicationError) Unwrap() error {
	return e.err
}

func (e *RequestError) Error() string {
	if e.ExceptionCode != 0 {
		return fmt.Sprintf("%s (exception 0x%02X): %v", e.Kind, e.ExceptionCode, e.Err)
	}
	return fmt.Sprintf("%s: %v", e.Kind, e.Err)
}

// Unwrap exposes the original upstream or context error.
func (e *RequestError) Unwrap() error {
	return e.Err
}

func classifyError(err error) (ErrorKind, byte) {
	if err == nil {
		return "", 0
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorContextDeadline, 0
	}
	if errors.Is(err, context.Canceled) {
		return ErrorContextCanceled, 0
	}

	var validationErr *validationError
	if errors.As(err, &validationErr) {
		return ErrorLocal, validationErr.exceptionCode
	}

	var upstreamErr *upstreamCommunicationError
	if errors.As(err, &upstreamErr) {
		return ErrorTransportClosed, 0
	}

	var mbErr *gridmodbus.Error
	if errors.As(err, &mbErr) {
		return ErrorProtocolException, mbErr.ExceptionCode
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return ErrorTransportTimeout, 0
	}

	if errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.EPIPE) {
		return ErrorTransportClosed, 0
	}
	if errors.As(err, &netErr) {
		return ErrorTransportClosed, 0
	}

	return ErrorLocal, 0
}

// ErrorKindOf returns the classified kind for an error.
func ErrorKindOf(err error) ErrorKind {
	var reqErr *RequestError
	if errors.As(err, &reqErr) {
		return reqErr.Kind
	}
	kind, _ := classifyError(err)
	return kind
}

// DownstreamException maps an upstream failure to a Modbus exception code.
func DownstreamException(err error) byte {
	var reqErr *RequestError
	if errors.As(err, &reqErr) && reqErr.Kind == ErrorProtocolException {
		return reqErr.ExceptionCode
	}
	if errors.As(err, &reqErr) && reqErr.Kind == ErrorLocal && reqErr.ExceptionCode != 0 {
		return reqErr.ExceptionCode
	}

	var upstreamErr *upstreamCommunicationError
	if errors.As(err, &upstreamErr) {
		return ExcGatewayTargetFailed
	}

	var mbErr *gridmodbus.Error
	if errors.As(err, &mbErr) {
		return mbErr.ExceptionCode
	}

	var validationErr *validationError
	if errors.As(err, &validationErr) {
		return validationErr.exceptionCode
	}

	switch ErrorKindOf(err) {
	case ErrorTransportTimeout, ErrorTransportClosed, ErrorContextDeadline, ErrorContextCanceled:
		return ExcGatewayTargetFailed
	default:
		return ExcServerFailure
	}
}

func requestError(err error, attempts int) *RequestError {
	kind, exceptionCode := classifyError(err)
	return &RequestError{
		Kind:          kind,
		ExceptionCode: exceptionCode,
		Attempts:      attempts,
		Err:           err,
	}
}

func newValidationError(exceptionCode byte, format string, args ...any) error {
	return &validationError{
		exceptionCode: exceptionCode,
		err:           fmt.Errorf(format, args...),
	}
}
