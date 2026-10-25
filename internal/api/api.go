// Package api holds the methods and types shared by the CLI, server, and hutchd.
package api

import "time"

// Methods the server serves to the CLI. The box.* methods are also served by
// hutchd to the server, which forwards them to the box's worker.
const (
	BoxList  = "box.list"
	BoxBoot  = "box.boot"
	BoxFork  = "box.fork"
	BoxSleep = "box.sleep"
	BoxWake  = "box.wake"
	BoxRm    = "box.rm"
	BoxExec  = "box.exec"

	WorkerList  = "worker.list"
	WorkerHello = "worker.hello"
	TokenCreate = "token.create"
)

type Box struct {
	ID         string    `json:"id"`
	Worker     string    `json:"worker,omitempty"`
	State      string    `json:"state"`
	Image      string    `json:"image"`
	VCPUs      int       `json:"vcpus"`
	MemMiB     int       `json:"mem_mib"`
	Generation int       `json:"generation"`
	Class      string    `json:"class"`
	ReachIP    string    `json:"reach_ip,omitempty"`
	Created    time.Time `json:"created"`
}

type Worker struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Class    string    `json:"class"`
	Version  string    `json:"version"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"last_seen"`
	Boxes    []Box     `json:"boxes,omitempty"`
}

type BootParams struct {
	ID     string `json:"id"`
	Image  string `json:"image"`
	VCPUs  int    `json:"vcpus"`
	MemMiB int    `json:"mem_mib"`
	Worker string `json:"worker,omitempty"`
}

type ForkParams struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}

type IDParams struct {
	ID string `json:"id"`
}

type ExecParams struct {
	ID   string   `json:"id"`
	Argv []string `json:"argv"`
	User string   `json:"user,omitempty"`
}

type Token struct {
	Token string `json:"token"`
}

// Enrollment happens on the worker port before the worker has a certificate.
type EnrollRequest struct {
	Secret string `json:"secret"`
	Name   string `json:"name"`
	CSR    []byte `json:"csr"`
}

type EnrollResponse struct {
	WorkerID string `json:"worker_id"`
	Cert     []byte `json:"cert"`
	CA       []byte `json:"ca"`
	Error    string `json:"error,omitempty"`
}
