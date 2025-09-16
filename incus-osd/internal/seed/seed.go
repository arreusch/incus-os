package seed

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

// GetSeedPath defines the path to the expected seed configuration. It will first search for any
// disk with a "SEED_DATA" label, which would be externally provided by the user. If not found,
// defaults to the "seed-data" partition that exists on install media.
func GetSeedPath() string {
	_, err := os.Stat("/dev/disk/by-partlabel/SEED_DATA")
	if err == nil {
		return "/dev/disk/by-partlabel/SEED_DATA"
	}

	_, err = os.Stat("/dev/disk/by-label/SEED_DATA")
	if err == nil {
		return "/dev/disk/by-label/SEED_DATA"
	}

	return "/dev/disk/by-partlabel/seed-data"
}

// IsMissing checks whether the provided error is an expected error for missing seed data.
func IsMissing(e error) bool {
	for _, entry := range []error{ErrNoSeedPartition, ErrNoSeedData, ErrNoSeedSection} {
		if errors.Is(e, entry) {
			return true
		}
	}

	return false
}

// parseFileContents searches for a given file in the seed configuration and returns its contents as a byte array if found.
func parseFileContents(partition string, filename string, target any) error {
	// First, try to get seed data by mounting a user-provided seed.
	err := parseFileContentsFromUserPartition(partition, filename, target)
	if err == nil {
		return nil
	}

	// Fallback to seed data from install media.
	return parseFileContentsFromRawTar(partition, filename, target)
}

// parseFileContentsFromUserPartition searches for a given file in the user-provided seed partition and returns its contents as a byte array if found.
func parseFileContentsFromUserPartition(partition string, filename string, target any) error {
	// Mount the seed partition.
	mountDir, err := os.MkdirTemp("", "incus-os-seed")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mountDir)

	// Try to mount as vfat.
	err = unix.Mount(partition, mountDir, "vfat", 0, "ro")
	if err != nil {
		// Try to mount as iso9660.
		err = unix.Mount(partition, mountDir, "iso9660", 0, "ro")
		if err != nil {
			return err
		}
	}
	defer unix.Unmount(mountDir, 0)

	files, err := os.ReadDir(mountDir)
	if err != nil {
		return err
	}

	// Search for the seed file.
	for _, file := range files {
		switch file.Name() {
		case filename + ".json":
			f, err := os.Open(filepath.Join(mountDir, filename+".json")) //nolint:gosec
			if err != nil {
				return err
			}
			defer f.Close() //nolint:revive

			decoder := json.NewDecoder(f)

			err = decoder.Decode(target)
			if err != nil {
				return err
			}

			return nil

		case filename + ".yaml":
			f, err := os.Open(filepath.Join(mountDir, filename+".yaml")) //nolint:gosec
			if err != nil {
				return err
			}
			defer f.Close() //nolint:revive

			decoder := yaml.NewDecoder(f)

			err = decoder.Decode(target)
			if err != nil {
				return err
			}

			return nil

		case filename + ".yml":
			f, err := os.Open(filepath.Join(mountDir, filename+".yml")) //nolint:gosec
			if err != nil {
				return err
			}
			defer f.Close() //nolint:revive

			decoder := yaml.NewDecoder(f)

			err = decoder.Decode(target)
			if err != nil {
				return err
			}

			return nil

		default:
		}
	}

	return errors.New("no seed data for " + filename + " found in user-provided seed partition")
}

// parseFileContentsFromRawTar searches for a given file in the seed partition on the install media and returns its contents as a byte array if found.
func parseFileContentsFromRawTar(partition string, filename string, target any) error {
	// Open the seed-data partition.
	f, err := os.Open(partition) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNoSeedPartition
		}

		return err
	}

	defer f.Close()

	// Check if seed-data is a tarball.
	header := make([]byte, 263)

	n, err := f.Read(header)
	if err != nil {
		return err
	}

	if n != 263 || !bytes.Equal(header[257:262], []byte{'u', 's', 't', 'a', 'r'}) {
		return ErrNoSeedData
	}

	_, err = f.Seek(0, 0)
	if err != nil {
		return err
	}

	// Parse the tarball.
	var hdr *tar.Header

	tr := tar.NewReader(f)
	for {
		// Get the next file.
		hdr, err = tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return ErrNoSeedSection
			}

			return err
		}

		// Check if expected file.
		switch hdr.Name {
		case filename + ".json":
			decoder := json.NewDecoder(tr)

			err = decoder.Decode(target)
			if err != nil {
				return err
			}

			return nil

		case filename + ".yaml", filename + ".yml":
			decoder := yaml.NewDecoder(tr)

			err = decoder.Decode(target)
			if err != nil {
				return err
			}

			return nil

		default:
		}
	}
}
