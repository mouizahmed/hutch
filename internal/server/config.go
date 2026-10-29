package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/rpc"
)

var configLayers = map[string]config.Layer{"machine": config.Machine, "user": config.User}

func (s *Server) configGet(ctx context.Context, _ json.RawMessage, _ *rpc.Out) (any, error) {
	var c api.Configs
	var err error
	if c.Machine, err = s.store.config("machine"); err != nil {
		return nil, err
	}
	if c.User, err = s.store.config("user"); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Server) configPut(ctx context.Context, params json.RawMessage, _ *rpc.Out) (any, error) {
	var p api.ConfigPutParams
	if err := rpc.Decode(params, &p); err != nil {
		return nil, err
	}
	layer, ok := configLayers[p.Name]
	if !ok {
		return nil, fmt.Errorf("no config %q", p.Name)
	}
	if _, err := config.Parse(layer, p.Name+" config", []byte(p.Content)); err != nil {
		return nil, err
	}
	doc, conflict, err := s.store.putConfig(p.Name, p.BaseVersion, p.Content)
	if err != nil {
		return nil, err
	}
	if !conflict {
		s.mu.Lock()
		workers := make([]*workerConn, 0, len(s.workers))
		for _, w := range s.workers {
			workers = append(workers, w)
		}
		s.mu.Unlock()
		for _, w := range workers {
			s.sendWorkerSettings(ctx, w)
		}
	}
	return api.ConfigPutResult{Doc: doc, Conflict: conflict}, nil
}

// settings resolves the server and user layers, the config the server acts on.
func (s *Server) settings() (*config.Config, error) {
	var docs []*config.Doc
	for name, layer := range configLayers {
		d, err := s.store.config(name)
		if err != nil {
			return nil, err
		}
		doc, err := config.Parse(layer, name+" config", []byte(d.Content))
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	return config.New(docs...), nil
}

// sendWorkerSettings sends a worker the server settings it uses, the bucket, and
// the keys of the projects whose boxes it hosts.
func (s *Server) sendWorkerSettings(ctx context.Context, w *workerConn) {
	c, err := s.settings()
	if err != nil {
		log.Printf("worker %s: settings: %v", w.id, err)
		return
	}
	p := api.NewWorkerSettings(c)
	if v := s.vault.Load(); v != nil {
		cfg := v.cfg
		p.Bucket = &cfg
		p.Keys = map[string][]byte{}
		s.mu.Lock()
		projects := maps.Clone(w.projects)
		s.mu.Unlock()
		for project := range projects {
			k := s.master.ProjectKey(project)
			p.Keys[project] = k[:]
			s.projectRecord(ctx, project)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := rpc.Call(ctx, w.sess, api.WorkerSettings, p, nil); err != nil {
		log.Printf("worker %s: settings: %v", w.id, err)
	}
}

// hostProject gives a worker a project's key before it gets the project's boxes.
func (s *Server) hostProject(ctx context.Context, w *workerConn, project string) {
	if project == "" {
		return
	}
	s.mu.Lock()
	known := w.projects[project]
	if !known {
		w.projects[project] = true
	}
	s.mu.Unlock()
	if !known {
		s.sendWorkerSettings(ctx, w)
	}
}
