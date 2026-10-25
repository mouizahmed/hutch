// Package api holds the methods and types shared by the CLI, server, and hutchd.
package api

import "time"

// Methods the server serves to the CLI. The box.* methods are also served by
// hutchd to the server, which forwards them to the box's worker.
const (
	BoxList    = "box.list"
	BoxBoot    = "box.boot"
	BoxFork    = "box.fork"
	BoxSleep   = "box.sleep"
	BoxWake    = "box.wake"
	BoxRm      = "box.rm"
	BoxExec    = "box.exec"
	BoxPTY     = "box.pty"
	BoxConsole = "box.console"
	BoxSerial  = "box.serial"

	ThreadNew  = "thread.new"
	ThreadSend = "thread.send"

	WorkerList   = "worker.list"
	WorkerHello  = "worker.hello"
	WorkerReport = "worker.report"
	TokenCreate  = "token.create"
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
	Threads    []Thread  `json:"threads,omitempty"`
}

// Thread is one agent session: a tmux window in the box's "hutch" session.
type Thread struct {
	Box     string    `json:"box"`
	N       int       `json:"n"`
	Agent   string    `json:"agent"`
	Title   string    `json:"title"`
	Created time.Time `json:"created"`
	State   string    `json:"state,omitempty"` // working, waiting, exited; from the latest report
}

const (
	ThreadWorking = "working"
	ThreadWaiting = "waiting"
	ThreadExited  = "exited"
)

// Report is sent by hutchd after each activity check and after it puts a box to sleep.
type Report struct {
	Boxes []BoxReport `json:"boxes"`
}

type BoxReport struct {
	Box        Box           `json:"box"`
	Threads    []ThreadState `json:"threads,omitempty"`
	LastActive time.Time     `json:"last_active"`
}

type ThreadState struct {
	N          int       `json:"n"`
	State      string    `json:"state"`
	LastOutput time.Time `json:"last_output"`
}

type ThreadParams struct {
	Box    string `json:"box"`
	Agent  string `json:"agent,omitempty"`
	Cmd    string `json:"cmd,omitempty"`
	Prompt string `json:"prompt,omitempty"`
}

type SendParams struct {
	Box     string `json:"box"`
	N       int    `json:"n"`
	Message string `json:"message"`
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

type PTYParams struct {
	ID   string   `json:"id"`
	Argv []string `json:"argv"`
	User string   `json:"user,omitempty"`
	Term string   `json:"term,omitempty"`
	Rows int      `json:"rows"`
	Cols int      `json:"cols"`
}

type ConsoleParams struct {
	ID     string `json:"id"`
	Follow bool   `json:"follow,omitempty"`
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
