package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var githubAPI = "https://api.github.com"

// mergeLoop marks boxes whose branch has landed in its base as merged, and
// notifies once. Nothing is removed: the user removes merged boxes.
func (s *Server) mergeLoop(ctx context.Context) {
	for {
		cfg, err := s.settings()
		every := 10 * time.Minute
		if err == nil {
			every = cfg.Duration("git.merge_check")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
		if err := s.checkMerges(ctx); err != nil {
			log.Printf("merge check: %v", err)
		}
	}
}

func (s *Server) checkMerges(ctx context.Context) error {
	boxes, err := s.store.boxes()
	if err != nil {
		return err
	}
	token := s.githubToken()
	for _, b := range boxes {
		if b.Branch == "" || b.Kind != "" || b.Merged != "" {
			continue
		}
		repo, ok := strings.CutPrefix(b.Project, "github.com/")
		if !ok {
			continue
		}
		how, err := githubMerged(ctx, token, repo, b.Branch, b.Base)
		if err != nil {
			log.Printf("merge check %s: %v", b.ID, err)
			continue
		}
		if how == "" {
			continue
		}
		if err := s.store.setMerged(b.ID, how); err != nil {
			return err
		}
		msg := fmt.Sprintf("%s's branch %s is merged (%s); remove it with hutch rm %s", b.ID, b.Branch, how, b.ID)
		s.store.event(b.ID, "merged", how)
		s.notify("merged", b.ID, "Box merged", msg)
	}
	return nil
}

// githubToken is the gh login in the user layer, for private repositories.
func (s *Server) githubToken() string {
	creds, err := s.credentials()
	if err != nil {
		return ""
	}
	for _, c := range creds {
		if c.Part != "gh" {
			continue
		}
		for _, f := range c.Files {
			for _, line := range strings.Split(string(f.Data), "\n") {
				if v, ok := strings.CutPrefix(strings.TrimSpace(line), "oauth_token:"); ok {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	return ""
}

// githubMerged reports how a branch landed in base, or "": a merged pull request
// from it (which covers squash and rebase merges), else every commit of a branch
// still on origin being in base.
func githubMerged(ctx context.Context, token, repo, branch, base string) (string, error) {
	owner, _, _ := strings.Cut(repo, "/")
	var pulls []struct {
		Number   int        `json:"number"`
		MergedAt *time.Time `json:"merged_at"`
		State    string     `json:"state"`
	}
	q := url.Values{"head": {owner + ":" + branch}, "state": {"all"}, "per_page": {"10"}}
	if _, err := githubGet(ctx, token, "/repos/"+repo+"/pulls?"+q.Encode(), &pulls); err != nil {
		return "", err
	}
	for _, p := range pulls {
		if p.State == "open" {
			return "", nil
		}
	}
	for _, p := range pulls {
		if p.MergedAt != nil {
			return fmt.Sprintf("PR #%d", p.Number), nil
		}
	}
	var cmp struct {
		AheadBy int `json:"ahead_by"`
	}
	found, err := githubGet(ctx, token, "/repos/"+repo+"/compare/"+url.PathEscape(base)+"..."+url.PathEscape(branch), &cmp)
	if err != nil || !found {
		return "", err
	}
	if cmp.AheadBy == 0 {
		return "in " + base, nil
	}
	return "", nil
}

// githubGet decodes a GitHub API response; a 404 is not found, not an error.
func githubGet(ctx context.Context, token, path string, out any) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", githubAPI+path, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GitHub %s: %s", path, resp.Status)
	}
	return true, json.NewDecoder(resp.Body).Decode(out)
}
