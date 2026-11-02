package operator

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// safeFailure contains only messages authored by the operator. Never construct it
// from a driver, API response, SQL statement, connection string, or Secret value.
type safeFailure string

func (e safeFailure) Error() string { return string(e) }

type stageError struct {
	stage string
	err   error
}

func (e *stageError) Error() string { return e.stage + ": " + e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

// atStage accepts only static operation names, never SQL statements or credentials.
func atStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &stageError{stage: stage, err: err}
}

var sqlStatePattern = regexp.MustCompile(`^[0-9A-Z]{5}$`)

// ErrorSummary logs operation context and allowlisted error metadata. Error text
// from external systems is deliberately omitted: SQL errors may quote passwords,
// and API validation errors may contain complete Secret values.
func ErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	if e, ok := err.(*stageError); ok {
		return e.stage + ": " + ErrorSummary(e.err)
	}
	if e, ok := err.(interface{ Unwrap() []error }); ok {
		var parts []string
		for _, child := range e.Unwrap() {
			if child != nil {
				parts = append(parts, ErrorSummary(child))
			}
		}
		return strings.Join(parts, "; ")
	}
	if e, ok := err.(safeFailure); ok {
		return string(e)
	}
	for _, cause := range []error{context.DeadlineExceeded, context.Canceled, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ENETUNREACH, syscall.EHOSTUNREACH, io.EOF, io.ErrUnexpectedEOF, pgconn.ErrConnClosed} {
		if errors.Is(err, cause) {
			return cause.Error()
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if sqlStatePattern.MatchString(pgErr.Code) {
			return "SQL error (SQLSTATE " + pgErr.Code + ")"
		}
		return "SQL error"
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		s := status.Status()
		// Reasons and messages are supplied by the API; only print known reasons.
		reason := "unknown reason"
		switch s.Reason {
		case "Unauthorized", "Forbidden", "NotFound", "AlreadyExists", "Conflict", "Invalid", "Timeout", "ServerTimeout", "TooManyRequests", "BadRequest", "InternalError", "ServiceUnavailable", "Expired", "Gone", "MethodNotAllowed", "NotAcceptable", "RequestEntityTooLarge", "UnsupportedMediaType":
			reason = string(s.Reason)
		}
		return fmt.Sprintf("Kubernetes API %s (HTTP %d)", reason, s.Code)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "DNS lookup failed"
	}
	var certErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	if errors.As(err, &certErr) || errors.As(err, &unknownCA) || errors.As(err, &hostnameErr) || errors.As(err, &invalidCert) {
		return "TLS certificate verification failed"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "network timeout"
	}
	if child := errors.Unwrap(err); child != nil {
		return ErrorSummary(child)
	}
	return fmt.Sprintf("unclassified error (%T)", err)
}
