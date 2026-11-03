package agent

import (
	"testing"
	"time"

	"github.com/mouizahmed/hutch/internal/api"
	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/link"
)

// A worker restarted while cut off from its lead still writes its heartbeat,
// to the bucket as the lead last sent it.
func TestHeartbeatBucket(t *testing.T) {
	dir := link.Dir(t.TempDir())
	if _, _, ok := heartbeatBucket(nil, dir); ok {
		t.Fatal("a heartbeat with no bucket known")
	}
	saved := bucket.Config{Name: "saved", Region: "r"}
	if err := dir.SaveBucket(saved); err != nil {
		t.Fatal(err)
	}
	if cfg, _, ok := heartbeatBucket(nil, dir); !ok || cfg != saved {
		t.Fatalf("without the lead's settings: %+v %v", cfg, ok)
	}
	sent := bucket.Config{Name: "sent", Region: "r"}
	if cfg, every, ok := heartbeatBucket(&api.WorkerSettingsParams{Bucket: &sent, LeadCheck: time.Minute}, dir); !ok || cfg != sent || every != time.Minute {
		t.Fatalf("with the lead's settings: %+v %s %v", cfg, every, ok)
	}
}
