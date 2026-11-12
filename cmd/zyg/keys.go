package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mindfire/zygote/trace"
	"github.com/mindfire/zygote/vfs"
	"github.com/spf13/cobra"
)

const (
	envPassphrase     = "ZYG_PASSPHRASE"      //nolint:gosec
	envPassphraseFile = "ZYG_PASSPHRASE_FILE" //nolint:gosec
	envIdentityFile   = "ZYG_IDENTITY_FILE"
)

var identityFile string

type encryptFlags struct {
	encrypt        bool
	recipients     []string
	recipientsFile string
	passphrase     bool
}

func init() {
	rootCmd.PersistentFlags().StringVar(&identityFile, "identity-file", "", "age identity file for decrypting bundles (env "+envIdentityFile+")")
}

func (f *encryptFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&f.encrypt, "encrypt", false, "Encrypt the output bundle with age")
	cmd.Flags().StringArrayVar(&f.recipients, "recipient", nil, "age X25519 public key to encrypt to (repeatable)")
	cmd.Flags().StringVar(&f.recipientsFile, "recipients-file", "", "File of age public keys, one per line")
	cmd.Flags().BoolVar(&f.passphrase, "passphrase", false, "Encrypt with the passphrase from "+envPassphraseFile+" or "+envPassphrase)
}

func (f *encryptFlags) exportOptions() ([]trace.ExportOption, error) {
	if !f.encrypt {
		if len(f.recipients) > 0 || f.recipientsFile != "" || f.passphrase {
			return nil, errors.New("--recipient, --recipients-file and --passphrase require --encrypt")
		}
		return nil, nil
	}

	recipients := append([]string(nil), f.recipients...)
	if f.recipientsFile != "" {
		fromFile, err := readRecipients(f.recipientsFile)
		if err != nil {
			return nil, err
		}
		recipients = append(recipients, fromFile...)
	}

	opts := []trace.ExportOption{trace.WithRecipients(recipients...)}
	if f.passphrase {
		pass, err := passphraseFromEnv()
		if err != nil {
			return nil, err
		}
		if pass == "" {
			return nil, fmt.Errorf("--passphrase requires %s or %s", envPassphraseFile, envPassphrase)
		}
		opts = append(opts, trace.WithEncryptionPassphrase(pass))
	}
	if err := trace.ValidateExportOptions(opts...); err != nil {
		return nil, err
	}
	return opts, nil
}

func readRecipients(path string) ([]string, error) {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("reading recipients file: %w", err)
	}
	defer f.Close() //nolint:errcheck

	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading recipients file: %w", err)
	}
	return out, nil
}

func passphraseFromEnv() (string, error) {
	if path := os.Getenv(envPassphraseFile); path != "" {
		b, err := os.ReadFile(path) //nolint:gosec
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", envPassphraseFile, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return os.Getenv(envPassphrase), nil
}

func importOptions() ([]trace.ImportOption, error) {
	var opts []trace.ImportOption
	path := identityFile
	if path == "" {
		path = os.Getenv(envIdentityFile)
	}
	if path != "" {
		opts = append(opts, trace.WithIdentityFile(path))
	}
	pass, err := passphraseFromEnv()
	if err != nil {
		return nil, err
	}
	if pass != "" {
		opts = append(opts, trace.WithPassphrase(pass))
	}
	return opts, nil
}

func loadBundle(path string, store vfs.Store) (trace.Bundle, error) {
	opts, err := importOptions()
	if err != nil {
		return trace.Bundle{}, err
	}
	return trace.ImportFile(path, store, opts...)
}

func agentEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case envPassphrase, envPassphraseFile, envIdentityFile:
			continue
		}
		out = append(out, kv)
	}
	return out
}
