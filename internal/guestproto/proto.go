package guestproto

const (
	Version = 1
	Port    = 1024
)

const (
	TypeHello   = "hello"
	TypeRestore = "restore"
	TypeExec    = "exec"
	TypePTY     = "pty"
	TypeStatus  = "status"
)

const (
	RespHello  = "hello"
	RespOK     = "ok"
	RespError  = "error"
	RespStdout = "stdout"
	RespStderr = "stderr"
	RespExit   = "exit"
	RespStatus = "status"
)

type Request struct {
	Type    string   `json:"type"`
	Version int      `json:"version"`
	Restore *Restore `json:"restore,omitempty"`
	Exec    *Exec    `json:"exec,omitempty"`
	PTY     *PTY     `json:"pty,omitempty"`
}

type Restore struct {
	UnixNano int64  `json:"unix_nano"`
	BoxID    string `json:"box_id"`
	Hostname string `json:"hostname"`
	Seed     []byte `json:"seed,omitempty"`
}

type Exec struct {
	Argv []string `json:"argv"`
	User string   `json:"user,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	Env  []string `json:"env,omitempty"`
}

// PTY starts a command on a pseudo-terminal. After the guest answers "ok", the
// host sends Input frames and the guest sends "stdout" frames, then "exit".
type PTY struct {
	Argv []string `json:"argv"`
	User string   `json:"user,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	Env  []string `json:"env,omitempty"`
	Term string   `json:"term,omitempty"`
	Rows int      `json:"rows"`
	Cols int      `json:"cols"`
}

const (
	InputStdin  = "stdin"
	InputResize = "resize"
)

type Input struct {
	Type string `json:"type"`
	Data []byte `json:"data,omitempty"`
	Rows int    `json:"rows,omitempty"`
	Cols int    `json:"cols,omitempty"`
}

type Response struct {
	Type     string  `json:"type"`
	Version  int     `json:"version,omitempty"`
	Guest    string  `json:"guest,omitempty"`
	Error    string  `json:"error,omitempty"`
	Data     []byte  `json:"data,omitempty"`
	ExitCode int     `json:"exit_code,omitempty"`
	Status   *Status `json:"status,omitempty"`
}

// Status is what the guest knows about activity; hutchd adds CPU from the host.
type Status struct {
	Threads  []ThreadStatus `json:"threads"`
	Clients  int            `json:"clients"`  // attached tmux clients, any session
	Sessions int            `json:"sessions"` // open exec and terminal sessions
	Ports    []ListenPort   `json:"ports,omitempty"`
}

// ListenPort is a listening TCP socket in the guest.
type ListenPort struct {
	Port     int  `json:"port"`
	Loopback bool `json:"loopback"` // bound to 127.0.0.1 or ::1 only
}

type ThreadStatus struct {
	N          int   `json:"n"`
	Dead       bool  `json:"dead"`
	LastOutput int64 `json:"last_output"` // unix seconds
}
