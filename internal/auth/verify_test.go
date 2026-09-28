package auth

import (
	"context"
	"errors"
	"testing"
)

// TestVerifyPassword checks the fresh-password check of the off-site routes (phase4.md S29): the
// right password passes, a wrong one, an empty one and an unknown user are ErrInvalidCredentials,
// and nothing changes (the user's sessions stay valid).
func TestVerifyPassword(t *testing.T) {
	s, _ := newTestService(t)
	ctx := context.Background()
	u, err := s.Setup(ctx, "admin", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := s.Login(ctx, "admin", "correct horse", SessionMeta{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name     string
		userID   int64
		password string
		want     error
	}{
		{"right password", u.ID, "correct horse", nil},
		{"wrong password", u.ID, "wrong horse", ErrInvalidCredentials},
		{"empty password", u.ID, "", ErrInvalidCredentials},
		{"unknown user", u.ID + 100, "correct horse", ErrInvalidCredentials},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := s.VerifyPassword(ctx, tt.userID, tt.password); !errors.Is(err, tt.want) || (tt.want == nil && err != nil) {
				t.Fatalf("VerifyPassword = %v, want %v", err, tt.want)
			}
		})
	}
	if _, ok, err := s.SessionUser(ctx, sess.Token); err != nil || !ok {
		t.Fatalf("the session ended after VerifyPassword: ok %v, err %v", ok, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.VerifyPassword(cctx, u.ID, "correct horse"); err == nil {
		t.Fatal("VerifyPassword with a cancelled context succeeded")
	}
}
