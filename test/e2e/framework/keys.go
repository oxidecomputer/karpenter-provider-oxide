package framework

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/oxidecomputer/oxide.go/oxide"
	"golang.org/x/crypto/ssh"
)

const sshKeyPrefix = "karpenter-e2e-"

func (e *Env) generateKey() error {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		return err
	}
	e.privateKey, e.signer = private, signer
	return nil
}

// registerKey uploads the run's public key so every nodeclass can select it by name.
func (e *Env) registerKey(ctx context.Context) error {
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(e.signer.PublicKey())))
	_, err := e.Oxide.CurrentUserSshKeyCreate(ctx, oxide.CurrentUserSshKeyCreateParams{
		Body: &oxide.SshKeyCreate{
			Name:        oxide.Name(e.SSHKeyName),
			Description: "karpenter e2e run " + e.Run,
			PublicKey:   public,
		},
	})
	if err != nil {
		return fmt.Errorf("registering ssh key %s: %w", e.SSHKeyName, err)
	}
	if !e.Keep {
		return nil
	}
	path, err := e.writePrivateKey()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "e2e: private key kept at %s (E2E_KEEP=true)\n", path)
	return nil
}

func (e *Env) writePrivateKey() (string, error) {
	block, err := ssh.MarshalPrivateKey(e.privateKey, "karpenter e2e "+e.Run)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(e.Artifacts, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(e.Artifacts, "id_ed25519")
	return path, os.WriteFile(path, pem.EncodeToMemory(block), 0o600)
}

func (e *Env) deleteKey(ctx context.Context) error {
	err := e.Oxide.CurrentUserSshKeyDelete(ctx, oxide.CurrentUserSshKeyDeleteParams{
		SshKey: oxide.NameOrId(e.SSHKeyName),
	})
	if err != nil && !errors.Is(err, oxide.ErrObjectNotFound) {
		return fmt.Errorf("deleting ssh key %s: %w", e.SSHKeyName, err)
	}
	return nil
}
