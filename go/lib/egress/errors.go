package egress

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Reasons an egress error carries in its ErrorInfo, beside
// UPSTREAM_HTTP_<status>.
const (
	ReasonTransport    = "UPSTREAM_TRANSPORT"
	ReasonTooLarge     = "UPSTREAM_BODY_TOO_LARGE"
	ReasonUndecodable  = "UPSTREAM_UNDECODABLE"
	ReasonUnconfigured = "CLIENT_UNCONFIGURED"
	ReasonBadRequest   = "REQUEST_UNENCODABLE"
)

// maxErrorBody bounds the upstream error body an ErrorInfo carries.
const maxErrorBody = 4 << 10

// codeFor is the gRPC code of an upstream status the call ended on. A code
// describes the CALLER's situation: a business handler passing the error on
// hands it to the gateway, which turns PERMISSION_DENIED into a 403 for the
// end user — and an upstream 401 means our credential is wrong, never theirs.
func codeFor(httpStatus int) codes.Code {
	switch {
	case httpStatus == 400, httpStatus == 422:
		return codes.InvalidArgument
	case httpStatus == 404:
		return codes.NotFound
	case httpStatus == 409:
		return codes.Aborted
	case httpStatus == 401, httpStatus == 403:
		return codes.Internal
	case httpStatus == 429, httpStatus >= 500:
		return codes.Unavailable
	}
	return codes.Internal
}

// callError is an egress error: a status with a google.rpc.ErrorInfo whose
// domain is the client and whose metadata says what happened upstream. Not
// w17.ErrorDetail — the gateway forwards only those to an end user, and
// upstream text is ours to debug, not theirs to read. Neither the message
// nor the metadata ever carries the DSN.
func callError(code codes.Code, client, reason, msg string, meta map[string]string) error {
	st := status.New(code, client+": "+msg)
	if withInfo, err := st.WithDetails(&errdetails.ErrorInfo{Domain: client, Reason: reason, Metadata: meta}); err == nil {
		st = withInfo
	}
	return st.Err()
}

func httpError(client string, httpStatus, attempts int, body []byte) error {
	meta := map[string]string{
		"http_status": strconv.Itoa(httpStatus),
		"attempts":    strconv.Itoa(attempts),
	}
	if len(body) > 0 {
		meta["body"] = truncate(body, maxErrorBody)
	}
	return callError(codeFor(httpStatus), client, "UPSTREAM_HTTP_"+strconv.Itoa(httpStatus),
		"upstream answered HTTP "+strconv.Itoa(httpStatus), meta)
}

// truncate cuts b to at most n bytes without splitting a UTF-8 sequence, and
// replaces invalid sequences: the text goes into a proto string, which must
// be valid UTF-8, and an upstream error body need not be.
func truncate(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
		for i := 0; i < utf8.UTFMax-1 && len(b) > 0; i++ {
			if r, size := utf8.DecodeLastRune(b); r != utf8.RuneError || size > 1 {
				break
			}
			b = b[:len(b)-1]
		}
	}
	return strings.ToValidUTF8(string(b), "\uFFFD")
}
