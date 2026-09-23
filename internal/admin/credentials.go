package admin

import "context"

// CredentialStore is the broker-facing half of device administration. Its
// implementation may be the legacy file editor during migration or DynSec in
// steady state; the registry never receives a password from either one.
type CredentialStore interface {
	Provision(context.Context, string, []string) (string, error)
	Revoke(context.Context, string) error
	SetEnabled(context.Context, string, bool) error
}

type scriptCredentialStore struct{ path string }

func (s scriptCredentialStore) Provision(ctx context.Context, id string, topics []string) (string, error) {
	return Provision(ctx, s.path, id, topics)
}
func (s scriptCredentialStore) Revoke(ctx context.Context, id string) error {
	return Deprovision(ctx, s.path, id)
}

// Legacy password_file/acl_file has no disable primitive. The runtime still
// rejects traffic for disabled devices; DynSec closes this remaining gap.
func (scriptCredentialStore) SetEnabled(context.Context, string, bool) error { return nil }
