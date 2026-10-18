package dataplane

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSimplifyValidationError(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		want    string
		wantNot []string
	}{
		{
			name: "nil error",
			err:  nil,
			want: "",
		},
		{
			name: "semantic validation error",
			err:  errors.New("semantic validation failed: configuration has semantic errors: haproxy validation failed: [ALERT] backend 'missing' not found"),
			want: "[ALERT] backend 'missing' not found",
		},
		{
			name: "semantic error without haproxy marker",
			err:  errors.New("semantic validation failed: configuration has semantic errors"),
			want: "semantic validation failed: configuration has semantic errors",
		},
		{
			name: "semantic validation error with multi-line context",
			err: errors.New(`semantic validation failed: configuration has semantic errors: haproxy validation failed:   userlist auth_users
      user admin password ...
→ [ALERT] (001) : parsing [haproxy.cfg:15] : unknown user 'missing' in userlist 'auth_users' (declared at haproxy.cfg:12)
  backend api
      server s1 127.0.0.1:8080`),
			want: `  userlist auth_users
      user admin password ...
→ [ALERT] (001) : parsing [haproxy.cfg:15] : unknown user 'missing' in userlist 'auth_users' (declared at haproxy.cfg:12)
  backend api
      server s1 127.0.0.1:8080`,
		},
		{
			name: "semantic validation error - backend has no server",
			err: errors.New(`semantic validation failed: configuration has semantic errors: haproxy validation failed:   defaults
      mode http
  backend api
→ [ALERT] (002) : parsing [haproxy.cfg:15] : backend 'api' has no server
      balance roundrobin`),
			want: `  defaults
      mode http
  backend api
→ [ALERT] (002) : parsing [haproxy.cfg:15] : backend 'api' has no server
      balance roundrobin`,
		},
		{
			name:    "schema validation error with value",
			err:     errors.New(`schema validation failed: configuration violates API schema constraints: Error at "/maxconn": must be >= 1` + "\nValue:\n  \"0\""),
			want:    "maxconn must be >= 1 (got 0)",
			wantNot: []string{"schema validation failed", "Error at"},
		},
		{
			name:    "schema validation error without value",
			err:     errors.New(`schema validation failed: Error at "/weight": number must be at most 256`),
			want:    "weight number must be at most 256",
			wantNot: []string{"Error at"},
		},
		{
			name: "schema error without Error at",
			err:  errors.New(`schema validation failed: some other error format`),
			want: `schema validation failed: some other error format`,
		},
		{
			name: "schema error with malformed field",
			err:  errors.New(`schema validation failed: Error at "/field`),
			want: `schema validation failed: Error at "/field`,
		},
		{
			name: "schema error with short constraint",
			err:  errors.New(`schema validation failed: Error at "/x": `),
			want: `schema validation failed: Error at "/x": `,
		},
		{
			name: "unknown error type",
			err:  errors.New("some other error"),
			want: "some other error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SimplifyValidationError(tt.err)
			assert.Equal(t, tt.want, got)

			for _, not := range tt.wantNot {
				assert.NotContains(t, got, not)
			}
		})
	}
}
