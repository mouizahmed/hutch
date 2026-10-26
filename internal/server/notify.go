package server

import (
	"context"
	"log"
	"os"
	"os/exec"
	"time"
)

// notify runs the user's notify command for an event, if they set one.
func (s *Server) notify(event, box, title, message string) {
	cfg, err := s.settings()
	if err != nil {
		return
	}
	cmd := cfg.String("notify")
	if cmd == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, "sh", "-c", cmd)
		c.Env = append(os.Environ(), "HUTCH_EVENT="+event, "HUTCH_BOX="+box, "HUTCH_TITLE="+title, "HUTCH_MESSAGE="+message)
		if out, err := c.CombinedOutput(); err != nil {
			log.Printf("notify %s: %v: %s", event, err, out)
		}
	}()
}
