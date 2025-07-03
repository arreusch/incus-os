// Package main is used for the image publisher.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	ghapi "github.com/google/go-github/v72/github"
	"github.com/lxc/incus/v6/shared/subprocess"

	apiupdate "github.com/lxc/incus-os/incus-osd/api/images"
)

func main() {
	err := do(context.TODO())
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func do(ctx context.Context) error {
	// Arguments.
	if len(os.Args) != 2 {
		return errors.New("missing target path")
	}

	targetPath := os.Args[1]

	// Config (optional).
	ghOrganization := os.Getenv("GH_ORGANIZATION")
	if ghOrganization == "" {
		ghOrganization = "lxc"
	}

	ghRepository := os.Getenv("GH_REPOSITORY")
	if ghRepository == "" {
		ghRepository = "incus-os"
	}

	updateChannel := os.Getenv("UPDATE_CHANNEL")
	if updateChannel == "" {
		updateChannel = "daily"
	}

	updateOrigin := os.Getenv("UPDATE_ORIGIN")
	if updateOrigin == "" {
		updateOrigin = "linuxcontainers.org"
	}

	updateSeverity := os.Getenv("UPDATE_SEVERITY")
	if updateSeverity == "" {
		updateSeverity = "none"
	}

	// Setup signer.
	sign := func(src string, dst string) error {
		if os.Getenv("SIG_KEY") == "" || os.Getenv("SIG_CERTIFICATE") == "" || os.Getenv("SIG_CHAIN") == "" {
			return nil
		}

		// Generate an SMIME signature.
		_, err := subprocess.RunCommandContext(ctx, "openssl", "smime", "-text", "-sign", "-signer", os.Getenv("SIG_CERTIFICATE"), "-inkey", os.Getenv("SIG_KEY"), "-in", src, "-out", dst, "-certfile", os.Getenv("SIG_CHAIN"))
		if err != nil {
			return err
		}

		return nil
	}

	// Setup client.
	gh := github{
		organization: ghOrganization,
		repository:   ghRepository,
		client:       ghapi.NewClient(nil),
	}

	if os.Getenv("GH_TOKEN") != "" {
		gh.client = gh.client.WithAuthToken(os.Getenv("GH_TOKEN"))
	}

	// Get the latest tag and file list.
	release, _, err := gh.client.Repositories.GetLatestRelease(ctx, ghOrganization, ghRepository)
	if err != nil {
		return err
	}

	releaseName := release.GetName()

	releaseAssets, _, err := gh.client.Repositories.ListReleaseAssets(ctx, ghOrganization, ghRepository, release.GetID(), nil)
	if err != nil {
		return err
	}

	// Create the release folder.
	err = os.Mkdir(filepath.Join(targetPath, releaseName), 0o755)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			slog.Info("Latest release already imported")

			return nil
		}

		return err
	}

	// Prepare the update.json.
	metaUpdate := apiupdate.Update{
		Format: "1.0",

		Channel:     updateChannel,
		Files:       []apiupdate.UpdateFile{},
		Origin:      updateOrigin,
		PublishedAt: time.Now(),
		Severity:    apiupdate.UpdateSeverity(updateSeverity),
		Version:     releaseName,
	}

	// Download the files.
	for _, asset := range releaseAssets {
		assetName := asset.GetName()

		// Check if file should be imported.
		var (
			assetComponent apiupdate.UpdateFileComponent
			assetType      apiupdate.UpdateFileType
		)

		switch {
		case assetName == "debug.raw.gz":
			assetComponent = apiupdate.UpdateFileComponentDebug
			assetType = apiupdate.UpdateFileTypeApplication
		case assetName == "incus.raw.gz":
			assetComponent = apiupdate.UpdateFileComponentIncus
			assetType = apiupdate.UpdateFileTypeApplication
		case strings.HasSuffix(assetName, ".efi.gz"):
			assetComponent = apiupdate.UpdateFileComponentOS
			assetType = apiupdate.UpdateFileTypeUpdateEFI
		case strings.HasSuffix(assetName, ".img.gz"):
			assetComponent = apiupdate.UpdateFileComponentOS
			assetType = apiupdate.UpdateFileTypeImageRaw
		case strings.HasSuffix(assetName, ".iso.gz"):
			assetComponent = apiupdate.UpdateFileComponentOS
			assetType = apiupdate.UpdateFileTypeImageISO
		case strings.Contains(assetName, ".usr-x86-64-verity."):
			assetComponent = apiupdate.UpdateFileComponentOS
			assetType = apiupdate.UpdateFileTypeUpdateUsrVerity
		case strings.Contains(assetName, ".usr-x86-64-verity-sig."):
			assetComponent = apiupdate.UpdateFileComponentOS
			assetType = apiupdate.UpdateFileTypeUpdateUsrVeritySignature
		case strings.Contains(assetName, ".usr-x86-64."):
			assetComponent = apiupdate.UpdateFileComponentOS
			assetType = apiupdate.UpdateFileTypeUpdateUsr
		default:
			continue
		}

		// Download the file.
		assetHash, assetSize, err := gh.downloadAsset(ctx, asset.GetID(), filepath.Join(targetPath, releaseName, assetName))
		if err != nil {
			return err
		}

		metaUpdate.Files = append(metaUpdate.Files, apiupdate.UpdateFile{
			Architecture: "x86_64",
			Component:    assetComponent,
			Filename:     assetName,
			Sha256:       assetHash,
			Size:         assetSize,
			Type:         assetType,
		})

		slog.Info("Downloaded", "name", assetName, "hash", assetHash, "size", assetSize)
	}

	// Write the update metadata.
	wr, err := os.Create(filepath.Join(targetPath, releaseName, "update.json")) //nolint:gosec
	if err != nil {
		return err
	}

	defer func() { _ = wr.Close() }()

	err = json.NewEncoder(wr).Encode(metaUpdate)
	if err != nil {
		return err
	}

	err = sign(filepath.Join(targetPath, releaseName, "update.json"), filepath.Join(targetPath, releaseName, "update.sjson"))
	if err != nil {
		return err
	}

	// Write the index metadata.
	metaIndex := apiupdate.Index{
		Format:  "1.0",
		Updates: []apiupdate.UpdateFull{{Update: metaUpdate, URL: "/" + metaUpdate.Version}},
	}

	wr, err = os.Create(filepath.Join(targetPath, "index.json")) //nolint:gosec
	if err != nil {
		return err
	}

	defer func() { _ = wr.Close() }()

	err = json.NewEncoder(wr).Encode(metaIndex)
	if err != nil {
		return err
	}

	err = sign(filepath.Join(targetPath, "index.json"), filepath.Join(targetPath, "index.sjson"))
	if err != nil {
		return err
	}

	return nil
}
