package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A branch is merged when a pull request from it was merged (squash merges leave
// its commits out of the base) or when it is still on origin with nothing beyond
// the base; an open pull request or a branch never pushed is not.
func TestGithubMerged(t *testing.T) {
	pulls := map[string]string{
		"squashed": `[{"number":7,"state":"closed","merged_at":"2026-10-27T10:00:00Z"}]`,
		"open":     `[{"number":8,"state":"open","merged_at":null}]`,
		"closed":   `[{"number":9,"state":"closed","merged_at":null}]`,
	}
	ahead := map[string]string{"contained": `{"ahead_by":0}`, "closed": `{"ahead_by":2}`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			_, branch, _ := strings.Cut(r.URL.Query().Get("head"), ":")
			if p, ok := pulls[branch]; ok {
				w.Write([]byte(p))
				return
			}
			w.Write([]byte(`[]`))
		case strings.Contains(r.URL.Path, "/compare/"):
			_, branch, _ := strings.Cut(r.URL.Path, "...")
			if a, ok := ahead[branch]; ok {
				w.Write([]byte(a))
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	githubAPI = srv.URL
	for branch, want := range map[string]string{"squashed": "PR #7", "open": "", "closed": "", "contained": "in main", "unpushed": ""} {
		got, err := githubMerged(context.Background(), "", "me/app", branch, "main")
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", branch, got, err, want)
		}
	}
}
