package applications

import (
	"context"
	"crypto/tls"
	"io"
)

// Application represents an installed application.
type Application interface { //nolint:interfacebloat
	GetCertificate() (*tls.Certificate, error)
	AddTrustedCertificate(ctx context.Context, name string, cert string) error
	Initialize(ctx context.Context) error
	IsPrimary() bool
	IsRunning(ctx context.Context) bool
	Start(ctx context.Context, version string) error
	Stop(ctx context.Context, version string) error
	Update(ctx context.Context, version string) error
	WipeLocalData() error
	FactoryReset(ctx context.Context) error
	GetBackup(archive io.Writer, complete bool) error
	RestoreBackup(archive io.Reader) error
}
