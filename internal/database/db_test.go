package database

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The retry loop exists for a Postgres that is still starting. Errors that
// will never resolve must fail immediately.
//
// Observed cost of getting this wrong: an integration run against a dropped
// test database spent 30 seconds per test retrying a message that was already
// correct on the first attempt.
func TestPermanentConnectErrorsFailFast(t *testing.T) {
	permanent := map[string]string{
		"3D000": "database does not exist",
		"28P01": "authentication failed",
		"28000": "authorization rejected",
		"42501": "insufficient privileges",
	}
	for code, wantReason := range permanent {
		err := &pgconn.PgError{Code: code, Message: "test"}
		got, reason := permanentConnectError(err)
		if !got {
			t.Errorf("SQLSTATE %s was treated as retryable; it can never succeed", code)
		}
		if reason != wantReason {
			t.Errorf("SQLSTATE %s reason = %q, want %q", code, reason, wantReason)
		}
	}
}

// A server that is still coming up, or a network that is briefly unreachable,
// is exactly what the retry loop is for.
func TestTransientConnectErrorsAreRetried(t *testing.T) {
	transient := []error{
		// "the database system is starting up" -- the reason the loop exists.
		&pgconn.PgError{Code: "57P03", Message: "starting up"},
		&pgconn.PgError{Code: "53300", Message: "too many connections"},
		// Not a server response at all: dial failure, DNS, TLS.
		errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
	}
	for _, err := range transient {
		if got, _ := permanentConnectError(err); got {
			t.Errorf("%v was treated as permanent; the retry loop exists for exactly this", err)
		}
	}
}

// The check must see through wrapping, since pgx returns wrapped errors.
func TestPermanentConnectErrorSeesThroughWrapping(t *testing.T) {
	wrapped := errors.Join(
		errors.New("failed to connect to `user=photo database=missing`"),
		&pgconn.PgError{Code: "3D000", Message: "database does not exist"},
	)
	if got, _ := permanentConnectError(wrapped); !got {
		t.Error("a wrapped 3D000 was not detected; pgx wraps its errors")
	}
}
