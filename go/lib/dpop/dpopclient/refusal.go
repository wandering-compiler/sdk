package dpopclient

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	w17pb "github.com/wandering-compiler/sdk/go/pb/w17"
)

// Reason is why the server refused a device credential.
type Reason string

const (
	// ReasonDeviceRevoked: the device was revoked; its key is no longer
	// accepted and the user must sign in again on a new device key.
	ReasonDeviceRevoked Reason = "DEVICE_REVOKED"
	// ReasonSessionExpired: the session ended; sign in again.
	ReasonSessionExpired Reason = "SESSION_EXPIRED"
)

// Sentinels matched by a [*RefusedError] with errors.Is.
var (
	ErrDeviceRevoked  = errors.New("dpopclient: device revoked")
	ErrSessionExpired = errors.New("dpopclient: session expired")
)

// RefusedError is a refusal carrying a [Reason]. It keeps the original
// status: Code for gRPC, HTTPStatus for HTTP (0 when the refusal came over
// gRPC).
type RefusedError struct {
	Reason     Reason
	Code       codes.Code
	HTTPStatus int
	// Err is the original gRPC error, nil for HTTP.
	Err error
}

func (e *RefusedError) Error() string {
	s := "dpopclient: refused: " + string(e.Reason)
	if e.HTTPStatus != 0 {
		return s + " (HTTP " + strconv.Itoa(e.HTTPStatus) + ")"
	}
	return s + " (" + e.Code.String() + ")"
}

// Is matches [ErrDeviceRevoked] or [ErrSessionExpired] by Reason.
func (e *RefusedError) Is(target error) bool {
	switch target {
	case ErrDeviceRevoked:
		return e.Reason == ReasonDeviceRevoked
	case ErrSessionExpired:
		return e.Reason == ReasonSessionExpired
	}
	return false
}

// Unwrap returns the original gRPC error.
func (e *RefusedError) Unwrap() error { return e.Err }

// GRPCStatus keeps status.FromError and status.Code working on the wrapped
// error.
func (e *RefusedError) GRPCStatus() *status.Status {
	if st, ok := status.FromError(e.Err); ok {
		return st
	}
	return status.New(e.Code, e.Error())
}

func knownReason(code string) (Reason, bool) {
	switch r := Reason(code); r {
	case ReasonDeviceRevoked, ReasonSessionExpired:
		return r, true
	}
	return "", false
}

// refusal turns a gRPC Unauthenticated or PermissionDenied error whose
// w17.ErrorDetail names a known [Reason] into a [*RefusedError]; any other
// error is returned unchanged.
func refusal(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok || (st.Code() != codes.Unauthenticated && st.Code() != codes.PermissionDenied) {
		return err
	}
	for _, d := range st.Details() {
		if ed, ok := d.(*w17pb.ErrorDetail); ok {
			if r, ok := knownReason(ed.GetCode()); ok {
				return &RefusedError{Reason: r, Code: st.Code(), Err: err}
			}
		}
	}
	return err
}

// maxRefusalBody bounds how much of a refusal body [Refusal] reads.
const maxRefusalBody = 64 << 10

// Refusal inspects an HTTP response: for a 401 or 403 whose error envelope
// (`{"error":{"details":[{"code":...}]}}`) names a known [Reason] it returns
// a [*RefusedError], otherwise nil. The body is left readable from the start
// either way.
func Refusal(resp *http.Response) error {
	if resp == nil || resp.Body == nil ||
		(resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
		return nil
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, maxRefusalBody))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	if err != nil {
		return nil
	}
	var env struct {
		Error struct {
			Details []struct {
				Code string `json:"code"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(head, &env) != nil {
		return nil
	}
	code := codes.Unauthenticated
	if resp.StatusCode == http.StatusForbidden {
		code = codes.PermissionDenied
	}
	for _, d := range env.Error.Details {
		if r, ok := knownReason(d.Code); ok {
			return &RefusedError{Reason: r, Code: code, HTTPStatus: resp.StatusCode}
		}
	}
	return nil
}
