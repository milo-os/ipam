package testdb

import (
	"strings"
	"testing"
)

// describe goes into test output, so a format it mishandles leaks a password.
func TestDescribeNeverEchoesCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "keyword/value, as a locally-started container is addressed",
			dsn:  "host=127.0.0.1 port=63626 user=postgres password=hunter2 dbname=postgres sslmode=disable",
			want: "127.0.0.1:63626/postgres",
		},
		{
			name: "URL, as IPAM_TEST_POSTGRES_DSN is usually written",
			dsn:  "postgres://postgres:hunter2@db.example:5432/ipam?sslmode=require",
			want: "db.example:5432/ipam",
		},
		{
			name: "keyword/value without a port",
			dsn:  "host=/var/run/postgresql dbname=ipam password=hunter2",
			want: "/var/run/postgresql/ipam",
		},
		{
			name: "neither format",
			dsn:  "password=hunter2",
			want: "an unrecognised DSN",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := describe(tc.dsn)
			if got != tc.want {
				t.Errorf("describe() = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "hunter2") {
				t.Error("describe() leaked the password")
			}
		})
	}
}

// A database name must be a legal identifier, and unique per test.
func TestDatabaseNameIsLegalAndUnique(t *testing.T) {
	long := "TestSomething/" + strings.Repeat("a_very_long_subtest_name", 5)

	for _, tc := range []struct {
		name string
		in   string
	}{
		{"plain", "TestFoo"},
		{"subtest with punctuation", "TestFoo/a case, with spaces"},
		{"over the identifier limit", long},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := databaseName(tc.in)
			if len(got) > 63 {
				t.Errorf("databaseName(%q) is %d bytes, over PostgreSQL's 63", tc.in, len(got))
			}
			for _, r := range got {
				legal := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
				if !legal {
					t.Errorf("databaseName(%q) = %q contains %q, which needs quoting", tc.in, got, r)
				}
			}
		})
	}

	// Same prefix past the truncation point.
	a := databaseName(long + "/one")
	b := databaseName(long + "/two")
	if a == b {
		t.Errorf("two tests sharing a long prefix both got database %q", a)
	}
}
