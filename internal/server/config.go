package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"strings"
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
	ws, err := s.store.workers()
	if err != nil {
		return nil, err
	}
	c.On = map[string]api.ConfigDoc{}
	for _, w := range ws {
		if !w.Gone {
			if c.On[w.Name], err = s.store.config(machineDoc(w.ID)); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

// machineDoc names the config holding one machine's own machine-layer values.
func machineDoc(workerID string) string { return "machine/" + workerID }

// machineNamed finds a machine by name (or ID).
func (s *Server) machineNamed(name string) (api.Worker, error) {
	ws, err := s.store.workers()
	if err != nil {
		return api.Worker{}, err
	}
	for _, w := range ws {
		if !w.Gone && (w.Name == name || w.ID == name) {
			return w, nil
		}
	}
	return api.Worker{}, fmt.Errorf("no machine %q (hutch machine lists them)", name)
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
	d, err := config.Parse(layer, p.Name+" config", []byte(p.Content))
	if err != nil {
		return nil, err
	}
	name := p.Name
	if p.On != "" {
		w, err := s.machineNamed(p.On)
		if err != nil {
			return nil, err
		}
		if p.Name != "machine" {
			return nil, errors.New("only machine config is set per machine")
		}
		for _, k := range d.Keys() {
			if !strings.HasPrefix(k, "machine.") {
				return nil, fmt.Errorf("%s is the same on every machine: set it with --machine, not --on", k)
			}
		}
		name = machineDoc(w.ID)
	}
	doc, conflict, err := s.store.putConfig(name, p.BaseVersion, p.Content)
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
		s.pushKeys(ctx, nil)
	}
	return api.ConfigPutResult{Doc: doc, Conflict: conflict}, nil
}

// settings resolves the machine and user layers as this machine has them, the
// config the server acts on.
func (s *Server) settings() (*config.Config, error) {
	s.mu.Lock()
	local := s.local
	s.mu.Unlock()
	return s.settingsOn(local)
}

// settingsOn resolves the config for one machine: its own machine-layer values
// over every machine's.
func (s *Server) settingsOn(workerID string) (*config.Config, error) {
	gen := s.store.configGen.Load()
	if c, ok := s.settingsCache.Load(workerID); ok && c.(cachedSettings).gen == gen {
		return c.(cachedSettings).cfg, nil
	}
	cfg, err := s.readSettings(workerID)
	if err == nil {
		s.settingsCache.Store(workerID, cachedSettings{gen, cfg})
	}
	return cfg, err
}

type cachedSettings struct {
	gen int64
	cfg *config.Config
}

// readSettings parses the config documents for one machine.
func (s *Server) readSettings(workerID string) (*config.Config, error) {
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
		if name == "machine" && workerID != "" {
			if d, err = s.store.config(machineDoc(workerID)); err != nil {
				return nil, err
			}
			own, err := config.Parse(layer, "its own machine config", []byte(d.Content))
			if err != nil {
				return nil, err
			}
			doc = config.Overlay(doc, own)
		}
		docs = append(docs, doc)
	}
	return config.New(docs...), nil
}

// sendWorkerSettings sends a worker the server settings it uses, the bucket, and
// the keys of the projects whose boxes it hosts.
func (s *Server) sendWorkerSettings(ctx context.Context, w *workerConn) {
	c, err := s.settingsOn(w.id)
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

// hostProject gives a worker a project's key, and its boxes' logins, before it
// gets the project's boxes.
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
		s.pushKeys(ctx, w)
	}
}

// runNoBoxes sets machine.run_boxes = false in machine id's own config, which
// has no box runtime; set to true, it's set back on its next connection.
func (s *Server) runNoBoxes(id string) error {
	c, err := s.settingsOn(id)
	if err != nil || !c.Bool("machine.run_boxes") {
		return err
	}
	d, err := s.store.config(machineDoc(id))
	if err != nil {
		return err
	}
	p, _ := config.SplitPath("machine.run_boxes")
	text, err := config.Set([]byte(d.Content), p, false)
	if err != nil {
		return err
	}
	_, _, err = s.store.putConfig(machineDoc(id), d.Version, string(text))
	return err
}
