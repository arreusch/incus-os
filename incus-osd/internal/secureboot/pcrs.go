package secureboot

import (
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"debug/pe"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/google/go-eventlog/tcg"
	"github.com/lxc/incus/v6/shared/subprocess"
	"github.com/smallstep/pkcs7"

	"github.com/lxc/incus-os/incus-osd/internal/util"
)

// ForceUpdatePCRBindings takes the current system's PCR state and UKI signing key and force-overrides
// the current LUKS TPM bindings with these values. This is DANGEROUS and only intended to be used in
// a recovery-type situation, such as when the system had to be booted with a recovery passphrase.
//
// Immediately after a successful reset, the system will be rebooted.
func ForceUpdatePCRBindings(ctx context.Context, osName string, osVersion string, luksPassword string) error {
	// First, make sure Secure Boot is enabled so we can have some confidence in the current running system.
	sbEnabled, err := Enabled()
	if err != nil {
		return err
	} else if !sbEnabled {
		return errors.New("refusing to reset TPM encryption bindings because Secure Boot is disabled")
	}

	// Second, refuse to do anything if the TPM can unlock all LUKS volumes.
	luksVolumes, err := util.GetLUKSVolumePartitions()
	if err != nil {
		return err
	}

	atLeastOneVolumeNeedsFixing := false

	for volumeName, volumeDev := range luksVolumes {
		_, err = subprocess.RunCommandContext(ctx, "cryptsetup", "luksOpen", "--test-passphrase", volumeDev, volumeName)
		if err != nil {
			atLeastOneVolumeNeedsFixing = true

			break
		}
	}

	if !atLeastOneVolumeNeedsFixing {
		return errors.New("refusing to reset TPM encryption bindings because current state can unlock all volumes")
	}

	// WARNING: here be dragons as we're going to be blindly trusting inputs that in theory could be attacker-controlled.

	// Get the current PCR7 value directly from the TPM. Don't bother replaying the event log and computing the value,
	// since it should be the same.
	pcr7, err := readPCR7()
	if err != nil {
		return err
	}

	// Extract the signing certificate from the UKI we're running from.
	ukiCert, err := getPublicKeyFromUKI(fmt.Sprintf("/boot/EFI/Linux/%s_%s.efi", osName, osVersion))
	if err != nil {
		return err
	}

	// Write the UKI's cert to where systemd will pick it up.
	err = os.WriteFile("/run/systemd/tpm2-pcr-public-key.pem", ukiCert, 0o600)
	if err != nil {
		return err
	}

	// Finally, we're ready to update the TPM bindings for each LUKS volume.
	pcr7String := hex.EncodeToString(pcr7)

	for _, volume := range luksVolumes {
		_, _, err := subprocess.RunCommandSplit(ctx, append(os.Environ(), "PASSWORD="+luksPassword), nil, "systemd-cryptenroll", "--tpm2-device=auto", "--wipe-slot=tpm2", "--tpm2-pcrlock=", "--tpm2-pcrs=7:sha256="+pcr7String, volume)
		if err != nil {
			return err
		}
	}

	// Once complete, immediately reboot the system which should then auto-unlock.
	_, err = subprocess.RunCommandContext(ctx, "systemctl", "reboot")
	if err != nil {
		return err
	}

	return nil
}

// readPCR7 returns the current PCR7 value from the TPM.
func readPCR7() ([]byte, error) {
	pcr7File, err := os.Open("/sys/class/tpm/tpm0/pcr-sha256/7")
	if err != nil {
		return nil, err
	}
	defer pcr7File.Close()

	actualPCR7Buf := make([]byte, 64)

	numBytes, err := io.ReadFull(pcr7File, actualPCR7Buf)
	if err != nil {
		return nil, err
	} else if numBytes != 64 {
		return nil, fmt.Errorf("only read %d bytes from /sys/class/tpm/tpm0/pcr-sha256/7", numBytes)
	}

	return hex.DecodeString(string(actualPCR7Buf))
}

// computeNewPCR7Value will compute the future PCR7 value after the KEK, db, and/or dbx EFI variables are updated.
// IMPORTANT: It is assumed that the provided TPM event log has already been validated.
func computeNewPCR7Value(eventLog []tcg.Event) ([]byte, error) {
	actualPCR7Buf := make([]byte, 32)

	for _, e := range eventLog {
		if e.Index == 7 { // We only care about PCR7.
			switch e.Type { //nolint:exhaustive
			case tcg.EFIVariableDriverConfig:
				// If an EFI variable (SecureBoot, PK, KEK, db, dbx), fetch the current value and use it for computing the PCR.
				buf, err := computeExpectedVariableDriverConfig(e.Data)
				if err != nil {
					return nil, err
				}

				actualPCR7Buf, err = extendPCRValue(actualPCR7Buf, buf, true)
				if err != nil {
					return nil, err
				}
			case tcg.EFIVariableAuthority:
				// Variable authority is a certificate used to sign EFI binaries (typically systemd-boot and the IncusOS
				// image, but also potentially third-party EFI drivers). We expect the IncusOS certificate used to sign
				// the systemd-boot EFI stub to match what's in the TPM event log. If there's a mis-match, we are about
				// to boot with a new Secure Boot signing key. Fetch the expected new certificate from the EFI db variable
				// and use it for PCR7 computation.
				buf, err := computeExpectedVariableAuthority(e.Data)
				if err != nil {
					return nil, err
				}

				actualPCR7Buf, err = extendPCRValue(actualPCR7Buf, buf, true)
				if err != nil {
					return nil, err
				}
			default:
				// For all other types, re-use the existing digest from the event log.
				var err error

				actualPCR7Buf, err = extendPCRValue(actualPCR7Buf, e.ReplayedDigest(), false)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	return actualPCR7Buf, nil
}

// computeExpectedVariableDriverConfig reads the current EFI variable, potentially updates the
// existing value, and returns an array of encoded bytes.
func computeExpectedVariableDriverConfig(rawBuf []byte) ([]byte, error) {
	v, err := tcg.ParseUEFIVariableData(bytes.NewReader(rawBuf))
	if err != nil {
		return nil, err
	}

	// Read the current variable.
	buf, err := readEFIVariable(v.VarName())
	if err != nil {
		return nil, err
	}

	// Update in-memory values.
	v.Header.VariableDataLength = uint64(len(buf))
	v.VariableData = buf

	// Get the updated buffer and use for PCR calculation.
	return v.Encode()
}

// computeExpectedVariableAuthority checks if the signature used by the systemd-boot EFI stub has
// changed, and if so, computes the new expected value.
func computeExpectedVariableAuthority(rawBuf []byte) ([]byte, error) {
	v, err := tcg.ParseUEFIVariableData(bytes.NewReader(rawBuf))
	if err != nil {
		return nil, err
	}

	va, err := tcg.ParseUEFIVariableAuthority(v)
	if err != nil {
		return nil, err
	}

	if len(va.Certs) != 1 {
		return nil, fmt.Errorf("expected exactly one certificate in VariableAuthority, got %d", len(va.Certs))
	}

	// Get existing certificate from systemd-boot EFI stub.
	efiFiles, err := getArchEFIFiles()
	if err != nil {
		return nil, err
	}

	existingCert, err := extractCertificateFromPE(efiFiles["bootEFI"])
	if err != nil {
		return nil, err
	}

	// If the certificates match, no need for further updates.
	if va.Certs[0].Equal(existingCert) {
		return rawBuf, nil
	}

	// Use the first four "words" of the existing certificate's Subject field to determine if the variable
	// authority certificate we're considering is third-party or not. We can't rely on a simple whitelist of
	// either "our" expected certificates or third-party certificates.
	existingCertPrefix := strings.Join(strings.Split(existingCert.Subject.String(), " ")[:4], " ")

	// If this is a third-party certificate, there's nothing for us to do.
	if !strings.HasPrefix(va.Certs[0].Subject.String(), existingCertPrefix) {
		return rawBuf, nil
	}

	// There was a mismatch between the EFI stub's certificate and the certificate in the event log.
	// Try to get the expected certificate from the db.
	certs, err := GetCertificatesFromVar("db")
	if err != nil {
		return nil, err
	}

	// Find the matching certificate.
	index := slices.IndexFunc(certs, func(c x509.Certificate) bool {
		return c.Equal(existingCert)
	})
	if index < 0 {
		return nil, fmt.Errorf("failed to find matching certificate '%s' used by systemd-boot stub in EFI db variable", existingCert.Subject.String())
	}

	// Update the variable's contents with the expected certificate value.
	var newBuf bytes.Buffer

	_, err = newBuf.Write(v.VariableData[:16]) // The first 16 bytes are a header; we shouldn't need to care about updating it since we're replacing a certificate with the same type/size as the existing one.
	if err != nil {
		return nil, err
	}

	_, err = newBuf.Write(certs[index].Raw)
	if err != nil {
		return nil, err
	}

	if newBuf.Len() != len(v.VariableData) {
		return nil, fmt.Errorf("resulting buffer size (%d) != expected size (%d)", newBuf.Len(), len(v.VariableData))
	}

	// Update in-memory values.
	v.VariableData = newBuf.Bytes()

	// Get the updated buffer and use for PCR calculation.
	return v.Encode()
}

// extractCertificateFromPE returns the signing certificate from a given PE binary.
// Adapted from https://github.com/doowon/sigtool/blob/master/sigtool.go, released under Apache-2.0 license.
func extractCertificateFromPE(filename string) (*x509.Certificate, error) {
	peFile, err := pe.Open(filename)
	if err != nil {
		return nil, err
	}
	defer peFile.Close()

	var certSize int64

	var offset int64

	switch t := peFile.OptionalHeader.(type) {
	case *pe.OptionalHeader64:
		certSize = int64(t.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY].Size) - 8
		offset = int64(t.DataDirectory[pe.IMAGE_DIRECTORY_ENTRY_SECURITY].VirtualAddress) + 8
	default:
		return nil, fmt.Errorf("file '%s' doesn't appear to be a 64bit signed PE", filename)
	}

	if certSize <= -8 || offset <= 8 {
		return nil, fmt.Errorf("file '%s' doesn't appear to be a valid signed PE", filename)
	}

	// #nosec G304
	rawFile, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer rawFile.Close()

	buf := make([]byte, certSize)

	readBytes, err := rawFile.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	} else if int64(readBytes) != certSize {
		return nil, fmt.Errorf("only read %d of %d expected bytes for certificate from PE '%s'", readBytes, certSize, filename)
	}

	pkcs, err := pkcs7.Parse(buf)
	if err != nil {
		return nil, err
	}

	if len(pkcs.Certificates) != 1 {
		return nil, fmt.Errorf("got %d certificates from PE '%s', expected exactly one", len(pkcs.Certificates), filename)
	}

	return pkcs.Certificates[0], nil
}

// extendPCRValue takes an existing pcr and extends it using the provided content.
func extendPCRValue(pcr []byte, content []byte, computeSHA256 bool) ([]byte, error) {
	hash := crypto.SHA256.New()

	_, err := hash.Write(pcr)
	if err != nil {
		return nil, err
	}

	if computeSHA256 {
		sum := sha256.Sum256(content)

		_, err := hash.Write(sum[:])
		if err != nil {
			return nil, err
		}
	} else {
		_, err := hash.Write(content)
		if err != nil {
			return nil, err
		}
	}

	return hash.Sum(nil), nil
}
