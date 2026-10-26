package server

import (
	"context"
	"encoding/json"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/durable"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/rpc"
)

// Project secrets are sealed with the project's key, so a copy of the database alone
// doesn't expose them.
func (s *Server) secretsKey(project string) (durable.Key, string) {
	return s.master.ProjectKey(project), "secrets/" + project
}

// projectSecretFiles returns the stored secrets of a project and their digest.
func (s *Server) projectSecretFiles(project string) ([]guestproto.File, string, error) {
	digest, sealed, err := s.store.projectSecrets(project)
	if err != nil || sealed == nil {
		return nil, digest, err
	}
	k, aad := s.secretsKey(project)
	plain, err := k.Open(aad, sealed)
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
	k, aad := s.secretsKey(p.Project)
	sealed := k.Seal(aad, plain)
	digest := api.SecretsDigest(p.Files)
	return api.Secrets{Project: p.Project, Digest: digest}, s.store.putProjectSecrets(p.Project, digest, sealed)
}
