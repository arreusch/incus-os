package seed

import (
	"context"

	apiseed "github.com/lxc/incus-os/incus-osd/api/seed"
)

// GetApplications extracts the list of applications from the seed data.
func GetApplications(_ context.Context, partition string) (*apiseed.Applications, error) {
	// Get applications list
	var apps apiseed.Applications

	err := parseFileContents(partition, "applications", &apps)
	if err != nil {
		return nil, err
	}

	return &apps, nil
}
