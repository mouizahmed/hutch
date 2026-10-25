package guestproto

import "time"

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
	TypeFiles   = "files"
	TypeDial    = "dial"
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
	Type    string        `json:"type"`
	Version int           `json:"version"`
	Restore *Restore      `json:"restore,omitempty"`
	Exec    *Exec         `json:"exec,omitempty"`
	PTY     *PTY          `json:"pty,omitempty"`
	Files   *Files        `json:"files,omitempty"`
	Dial    *Dial         `json:"dial,omitempty"`
	Status  *StatusParams `json:"status,omitempty"`
}

// StatusParams asks for hashes of files the host watches for changes (credentials
// an agent refreshes). Older guests ignore it.
type StatusParams struct {
	Watch []string `json:"watch,omitempty"`
}

// Files writes files atomically, owned by User (default dev).
type Files struct {
	User  string `json:"user,omitempty"`
	Files []File `json:"files"`
}

type File struct {
	// Path is absolute, or under the user's home with a "~/" prefix.
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Data []byte `json:"data"`
	// Merge deep-merges Data into the existing file instead of replacing it; the
	// format comes from the extension (.json or .toml).
	Merge bool `json:"merge,omitempty"`
}

// Dial connects to a TCP port on the guest's loopback. After the guest answers
// "ok", the connection carries the raw byte stream both ways.
type Dial struct {
	Port int `json:"port"`
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
	// Files maps each watched path that exists to the SHA-256 of its contents.
	Files map[string]string `json:"files,omitempty"`
	Push  *Push             `json:"push,omitempty"`
}

// PushConfig is where the guest pushes the box's branch after commits; the host
// writes it to PushConfigPath when the box is created.
type PushConfig struct {
	Dir      string        `json:"dir"`
	Branch   string        `json:"branch"`
	Interval time.Duration `json:"interval"`
}

const PushConfigPath = "/var/lib/hutch-guest/push.json"

// Push is the outcome of the guest's latest push of the box's branch.
type Push struct {
	Branch string `json:"branch"`
	Head   string `json:"head"`            // the commit pushed or tried
	Error  string `json:"error,omitempty"` // empty when origin has Head
	At     int64  `json:"at"`              // unix seconds
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
