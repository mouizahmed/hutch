package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// Project secrets are encrypted with a key that lives next to the database, so a
// copy of the database alone doesn't expose them. Phase 3 replaces this key with
// the per-project keys of the bucket.
func (s *Server) secretsKey() ([]byte, error) {
	path := filepath.Join(s.cfg.Dir, "secrets.key")
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		rand.Read(key)
		if err := os.WriteFile(path, key, 0o600); err != nil {
			return nil, err
		}
		return key, nil
	}
	if err == nil && len(key) != 32 {
		err = fmt.Errorf("%s: want 32 bytes", path)
	}
	return key, err
}

func (s *Server) seal(plain []byte) ([]byte, error) {
	gcm, err := s.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	return gcm.Seal(nonce, nonce, plain, nil), nil
}

func (s *Server) open(sealed []byte) ([]byte, error) {
	gcm, err := s.gcm()
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("project secrets are corrupt")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}

func (s *Server) gcm() (cipher.AEAD, error) {
	key, err := s.secretsKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// projectSecretFiles returns the stored secrets of a project and their digest.
func (s *Server) projectSecretFiles(project string) ([]guestproto.File, string, error) {
	digest, sealed, err := s.store.projectSecrets(project)
	if err != nil || sealed == nil {
		return nil, digest, err
	}
	plain, err := s.open(sealed)
	if err != nil {
		return nil, "", err
	}
	var files []guestproto.File
	return files, digest, json.Unmarshal(plain, &files)
}

func (s *Server) secretsGet(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ProjectParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	files, digest, err := s.projectSecretFiles(p.Project)
	if err != nil {
		return nil, err
	}
	for i := range files {
		files[i].Data = nil
	}
	return api.Secrets{Project: p.Project, Digest: digest, Files: files}, nil
}

func (s *Server) secretsPut(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.Secrets
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	if _, err := s.store.project(p.Project); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(p.Files)
	if err != nil {
		return nil, err
	}
	sealed, err := s.seal(plain)
	if err != nil {
		return nil, err
	}
	digest := api.SecretsDigest(p.Files)
	return api.Secrets{Project: p.Project, Digest: digest}, s.store.putProjectSecrets(p.Project, digest, sealed)
}
