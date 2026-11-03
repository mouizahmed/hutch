package bucket

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestS3ConditionalDelete(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusNoContent, nil},
		{http.StatusPreconditionFailed, ErrChanged},
		{http.StatusConflict, ErrChanged},
		{http.StatusNotFound, ErrNotFound},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.Header.Get("If-Match") != `"listed"` ||
					!strings.Contains(r.Header.Get("Authorization"), ";if-match;") {
					t.Errorf("delete must carry a signed If-Match: %s %v", r.Method, r.Header)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			b, err := New(Config{Endpoint: srv.URL, Region: "test", Name: "test", AccessKey: "test", SecretKey: "test"})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.DeleteIfMatch(t.Context(), "chunks/x", `"listed"`); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestS3ObjectETag(t *testing.T) {
	const etag = `"stored"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") == "2" {
			fmt.Fprint(w, `<ListBucketResult><Contents><Key>chunks/x</Key><ETag>&quot;stored&quot;</ETag><Size>4</Size></Contents></ListBucketResult>`)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Length", "4")
		if r.Method != http.MethodHead {
			fmt.Fprint(w, "data")
		}
	}))
	defer srv.Close()
	b, err := New(Config{Endpoint: srv.URL, Region: "test", Name: "test", AccessKey: "test", SecretKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	objects, err := b.List(t.Context(), "chunks/")
	if err != nil || len(objects) != 1 || objects[0].ETag != etag {
		t.Fatalf("listed objects: %+v, %v", objects, err)
	}
	object, err := b.Head(t.Context(), "chunks/x")
	if err != nil || object.ETag != etag {
		t.Fatalf("head: %+v, %v", object, err)
	}
	_, object, err = b.Fetch(t.Context(), "chunks/x")
	if err != nil || object.ETag != etag {
		t.Fatalf("fetch: %+v, %v", object, err)
	}
}
