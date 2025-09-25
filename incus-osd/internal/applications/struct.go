package applications

import (
	"context"
	"crypto/tls"
)

// Application represents an installed application.
type Application interface {
	GetCertificate() (*tls.Certificate, error)
	Initialize(ctx context.Context) error
	IsPrimary() bool
	IsRunning(ctx context.Context) bool
	Start(ctx context.Context, version string) error
	Stop(ctx context.Context, version string) error
	Update(ctx context.Context, version string) error
}
