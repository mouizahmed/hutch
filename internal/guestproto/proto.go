package guestproto

import "time"

// Version 2 added input to exec (Exec.Stdin).
const (
	Version = 2
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
	// Stdin means the host sends the command's input as Input frames after the
	// request, ending with InputEOF.
	Stdin bool `json:"stdin,omitempty"`
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
	InputEOF    = "eof"
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

// ThreadStatus is what a thread's terminal shows, the same for every agent.
type ThreadStatus struct {
	N          int   `json:"n"`
	Dead       bool  `json:"dead"`
	LastOutput int64 `json:"last_output"` // unix seconds
	// Bell: it rang the bell and has printed nothing since.
	Bell bool `json:"bell,omitempty"`
	// Redraw: its last output came right after its window was resized, the app
	// redrawing rather than working.
	Redraw bool `json:"redraw,omitempty"`
	// Viewed: a client shows it, other than a view the TUI keeps off screen.
	Viewed    bool   `json:"viewed,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// HiddenOption marks a tmux view session whose client isn't on screen, so it
// doesn't count as looking at its window.
const HiddenOption = "@hutch-hidden"

// ThreadInfo is written by the host to ThreadsDir/<n>.json when it starts an agent
// thread, so the guest can find its session file.
type ThreadInfo struct {
	N         int    `json:"n"`
	Sessions  string `json:"sessions"` // the adapter's session file glob, under ~/
	SessionID string `json:"session_id,omitempty"`
	Started   int64  `json:"started"` // unix seconds
}

const ThreadsDir = "/var/lib/hutch-guest/threads"

// ServicesPath lists a box's services, one shell command per line, run as dev by
// its init; a SIGHUP to PID 1 applies changes.
const ServicesPath = "/etc/hutch/services"

// InitStatusPath is where a box's init keeps what it runs: a list of ServiceStatus.
const InitStatusPath = "/run/hutch/init.json"

type ServiceStatus struct {
	Name  string    `json:"name"`
	PID   int       `json:"pid"`
	UID   int       `json:"uid"`
	Since time.Time `json:"since,omitzero"`
	Fails int       `json:"fails"`
}
