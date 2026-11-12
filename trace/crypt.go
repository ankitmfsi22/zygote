package trace

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
)

var (
	// ErrEncrypted indicates an encrypted bundle was opened without a key.
	ErrEncrypted = errors.New("bundle is encrypted: supply --identity-file or a passphrase (ZYG_PASSPHRASE_FILE)")
	// ErrDecrypt indicates the supplied key or passphrase cannot decrypt the bundle.
	ErrDecrypt = errors.New("cannot decrypt bundle: no matching key or wrong passphrase")
	// ErrCorruptEncrypted indicates an encrypted bundle that is truncated, tampered with or not a bundle.
	ErrCorruptEncrypted = errors.New("corrupt encrypted bundle")
	// ErrNoEncryptionKey indicates encryption was requested without a recipient or passphrase.
	ErrNoEncryptionKey = errors.New("encryption requested but no recipient or passphrase was provided")
	// ErrAmbiguousEncryption indicates both recipients and a passphrase were given.
	ErrAmbiguousEncryption = errors.New("encryption accepts either recipients or a passphrase, not both")
)

var (
	ageMagic = []byte("age-encryption.org/v1\n")
	zipMagic = []byte("PK\x03\x04")
)

// ImportOption configures ImportFile.
type ImportOption func(*importConfig)

type importConfig struct {
	identityFile string
	passphrase   string
}

// WithIdentityFile decrypts an encrypted bundle with the age identities in the named file.
func WithIdentityFile(path string) ImportOption {
	return func(c *importConfig) {
		c.identityFile = path
	}
}

// WithPassphrase decrypts a passphrase-encrypted bundle.
func WithPassphrase(passphrase string) ImportOption {
	return func(c *importConfig) {
		c.passphrase = passphrase
	}
}

// WithRecipients encrypts the exported bundle to the given age X25519 public keys.
func WithRecipients(recipients ...string) ExportOption {
	return func(c *exportConfig) {
		c.encrypt = true
		c.recipients = append(c.recipients, recipients...)
	}
}

// WithEncryptionPassphrase encrypts the exported bundle with a passphrase.
func WithEncryptionPassphrase(passphrase string) ExportOption {
	return func(c *exportConfig) {
		c.encrypt = true
		c.passphrase = passphrase
	}
}

// ValidateExportOptions reports whether the encryption options of ExportFile are usable without writing anything.
func ValidateExportOptions(opts ...ExportOption) error {
	var cfg exportConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	_, err := cfg.ageRecipients()
	return err
}

func (c exportConfig) ageRecipients() ([]age.Recipient, error) {
	if !c.encrypt {
		return nil, nil
	}
	if len(c.recipients) > 0 && c.passphrase != "" {
		return nil, ErrAmbiguousEncryption
	}
	if c.passphrase != "" {
		r, err := age.NewScryptRecipient(c.passphrase)
		if err != nil {
			return nil, fmt.Errorf("invalid passphrase: %w", err)
		}
		return []age.Recipient{r}, nil
	}
	if len(c.recipients) == 0 {
		return nil, ErrNoEncryptionKey
	}
	out := make([]age.Recipient, 0, len(c.recipients))
	for _, s := range c.recipients {
		r, err := age.ParseX25519Recipient(s)
		if err != nil {
			return nil, fmt.Errorf("invalid recipient %q: %w", s, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func (c importConfig) ageIdentities() ([]age.Identity, error) {
	var ids []age.Identity
	if c.identityFile != "" {
		b, err := os.ReadFile(c.identityFile)
		if err != nil {
			return nil, fmt.Errorf("reading identity file: %w", err)
		}
		parsed, err := age.ParseIdentities(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("parsing identity file: %w", err)
		}
		ids = append(ids, parsed...)
	}
	if c.passphrase != "" {
		id, err := age.NewScryptIdentity(c.passphrase)
		if err != nil {
			return nil, fmt.Errorf("invalid passphrase: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// IsEncrypted reports whether the named file is an encrypted bundle.
func IsEncrypted(filename string) (bool, error) {
	f, err := os.Open(filename) //nolint:gosec
	if err != nil {
		return false, err
	}
	defer f.Close() //nolint:errcheck

	head := make([]byte, len(ageMagic))
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	return bytes.HasPrefix(head[:n], ageMagic), nil
}

func decryptBundle(data []byte, cfg importConfig) ([]byte, error) {
	ids, err := cfg.ageIdentities()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, ErrEncrypted
	}
	r, err := age.Decrypt(bytes.NewReader(data), ids...)
	if err != nil {
		var noMatch *age.NoIdentityMatchError
		if errors.As(err, &noMatch) {
			return nil, ErrDecrypt
		}
		return nil, fmt.Errorf("%w: %v", ErrCorruptEncrypted, err)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorruptEncrypted, err)
	}
	if !bytes.HasPrefix(plain, zipMagic) {
		return nil, fmt.Errorf("%w: payload is not a bundle", ErrCorruptEncrypted)
	}
	return plain, nil
}

func writeEncrypted(filename string, recipients []age.Recipient, write func(io.Writer) error) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(filename), ".zyg-enc-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if err = tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return err
	}
	w, err := age.Encrypt(tmp, recipients...)
	if err != nil {
		return err
	}
	if err = write(w); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, filename)
}
