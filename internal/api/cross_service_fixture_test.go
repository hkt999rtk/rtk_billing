package api

import (
	"net/url"
	"testing"
)

// samePostgresEndpoint compares the server and database while deliberately
// ignoring credentials and query-string formatting. Cross-service tests must
// never share Billing's database, even when both fixtures use loopback.
func samePostgresEndpoint(left, right string) bool {
	l, lerr := url.Parse(left)
	r, rerr := url.Parse(right)
	if lerr != nil || rerr != nil || left == "" || right == "" {
		return left == right
	}
	return l.Scheme == r.Scheme && l.Host == r.Host && l.Path == r.Path
}

func TestSamePostgresEndpointIgnoresCredentialsAndOptions(t *testing.T) {
	if !samePostgresEndpoint(
		"postgres://one:secret@127.0.0.1:5432/billing?sslmode=disable",
		"postgres://two:other@127.0.0.1:5432/billing?connect_timeout=2",
	) {
		t.Fatal("same database was not recognized")
	}
	if samePostgresEndpoint(
		"postgres://one:secret@127.0.0.1:5432/billing",
		"postgres://one:secret@127.0.0.1:5433/billing",
	) {
		t.Fatal("separate fixture server was treated as shared")
	}
}
