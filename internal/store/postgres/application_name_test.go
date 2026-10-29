package postgres

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestApplicationNameBoundsBeforeDatabaseUse(t *testing.T) {
	for _, name := range []string{strings.Repeat("x", 64), "line\nfeed", "nul\x00", "é"} {
		if _, err := OpenWithOptions(context.Background(), "unused", "valid_schema", Options{ApplicationName: name}); !errors.Is(err, ErrInvalid) {
			t.Fatal("invalid application name was admitted")
		}
	}
}

func TestApplicationNamePreservesDSNAndExplicitOverride(t *testing.T) {
	dsn := os.Getenv("LIQUIDITYLAB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set explicit development PostgreSQL DSN")
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid development URI")
		}
		q := u.Query()
		q.Set("application_name", "existing_dsn_tag")
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " application_name='existing_dsn_tag'"
	}
	t.Setenv("LIQUIDITYLAB_TEST_POSTGRES_DSN", dsn)
	for _, tc := range []struct{ option, want string }{{"", "existing_dsn_tag"}, {"llbench-explicit", "llbench-explicit"}} {
		s := telemetryTestStore(t, Options{ApplicationName: tc.option})
		var name string
		if err := s.pool.QueryRow(context.Background(), "SELECT current_setting('application_name')").Scan(&name); err != nil || name != tc.want {
			t.Fatal("application startup parameter differs from requested compatibility behavior")
		}
	}
}
