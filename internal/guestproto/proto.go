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
// an agent refreshes), and for the workspace's git status. Older guests ignore it.
type StatusParams struct {
	Watch []string `json:"watch,omitempty"`
	Git   bool     `json:"git,omitempty"`
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
	Viewers  int            `json:"viewers"`  // those that keep the box awake: all but the TUI's off-screen views
	Sessions int            `json:"sessions"` // open exec and terminal sessions, but tmux clients
	Ports    []ListenPort   `json:"ports,omitempty"`
	// Files maps each watched path that exists to the SHA-256 of its contents.
	Files map[string]string `json:"files,omitempty"`
	Git   *GitStatus        `json:"git,omitempty"`
}

// GitStatus is /workspace's repository as it is now: the branch checked out (empty
// when detached), uncommitted and untracked files, and commits on no remote.
type GitStatus struct {
	Branch   string `json:"branch"`
	Changes  int    `json:"changes"`
	Unpushed int    `json:"unpushed"`
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
	// Bell: it rang the bell and has printed nothing since; BellAt is when it
	// last rang (unix seconds).
	Bell   bool  `json:"bell,omitempty"`
	BellAt int64 `json:"bell_at,omitempty"`
	// Redraw: its last output came right after its window was resized, the app
	// redrawing rather than working.
	Redraw bool `json:"redraw,omitempty"`
	// Viewed: a client shows it, other than a view the TUI keeps off screen.
	Viewed bool `json:"viewed,omitempty"`
	// What hutchd started in the window, from its tmux options: the agent (empty for
	// a command), its autonomy, the prompt's first line or the command, your name
	// for it, and when (unix seconds); and the terminal title its program sets.
	Agent    string `json:"agent,omitempty"`
	Autonomy string `json:"autonomy,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Name     string `json:"name,omitempty"`
	Created  int64  `json:"created,omitempty"`
	Title    string `json:"title,omitempty"`
}

// A thread's tmux window carries what hutchd started in it, so the box's tmux is
// the only record of its threads; the session carries the highest number used, so
// numbers aren't reused.
const (
	OptAgent    = "@hutch-agent"
	OptAutonomy = "@hutch-autonomy"
	OptPrompt   = "@hutch-prompt"
	OptName     = "@hutch-name"
	OptCreated  = "@hutch-created"
	OptLast     = "@hutch-last"
)

// ThreadsPath keeps the box's thread list on its disk, to recreate agent threads
// after a cold boot.
const ThreadsPath = "/var/lib/hutch-guest/threads.json"

// HiddenOption marks a tmux view session the TUI keeps: HiddenOff while it's off
// screen, which neither looks at its window nor keeps the box awake; HiddenAway
// while it's on screen in a terminal that isn't focused, which keeps the box
// awake without looking.
const (
	HiddenOption = "@hutch-hidden"
	HiddenOff    = "off"
	HiddenAway   = "away"
)

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
