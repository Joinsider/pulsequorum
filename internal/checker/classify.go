// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 The Pulsequorum Authors

package checker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
)

// TLSError marks failures of the certificate check done by a checker itself.
type TLSError struct{ Err error }

func (e *TLSError) Error() string { return "tls: " + e.Err.Error() }
func (e *TLSError) Unwrap() error { return e.Err }

// AssertionError marks a response that arrived but did not meet an expectation
// (keyword, JSON path, response time).
type AssertionError struct{ Msg string }

func (e *AssertionError) Error() string { return e.Msg }

// StatusCodeError marks an unexpected HTTP status code.
type StatusCodeError struct {
	Code     int
	Expected string
}

func (e *StatusCodeError) Error() string {
	return fmt.Sprintf("unexpected status code %d (expected %s)", e.Code, e.Expected)
}

// Classify maps an error to an ErrorClass (FA-CHK-03). The order matters:
// specific causes win over the generic network errors that wrap them.
func Classify(err error) ErrorClass {
	if err == nil {
		return ClassNone
	}
	var (
		statusErr *StatusCodeError
		assertErr *AssertionError
		tlsErr    *TLSError
		verifyErr *tls.CertificateVerificationError
		unknownCA x509.UnknownAuthorityError
		hostErr   x509.HostnameError
		invalid   x509.CertificateInvalidError
		recordErr tls.RecordHeaderError
		alertErr  tls.AlertError
		dnsErr    *net.DNSError
		netErr    net.Error
		opErr     *net.OpError
	)
	switch {
	case errors.Is(err, ErrSSRFBlocked):
		return ClassSSRFBlocked
	case errors.As(err, &statusErr):
		return ClassHTTPStatus
	case errors.As(err, &assertErr):
		return ClassAssertion
	case errors.As(err, &tlsErr), errors.As(err, &verifyErr), errors.As(err, &unknownCA),
		errors.As(err, &hostErr), errors.As(err, &invalid), errors.As(err, &recordErr), errors.As(err, &alertErr):
		return ClassTLS
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	case errors.As(err, &dnsErr):
		if dnsErr.IsTimeout {
			return ClassTimeout
		}
		return ClassDNS
	case errors.As(err, &netErr) && netErr.Timeout():
		return ClassTimeout
	case errors.As(err, &opErr),
		errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return ClassConnect
	}
	return ClassInternal
}
