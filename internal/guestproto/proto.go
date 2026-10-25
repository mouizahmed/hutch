package guestproto

const (
	Version = 1
	Port    = 1024
)

const (
	TypeHello   = "hello"
	TypeRestore = "restore"
	TypeExec    = "exec"
)

const (
	RespHello  = "hello"
	RespOK     = "ok"
	RespError  = "error"
	RespStdout = "stdout"
	RespStderr = "stderr"
	RespExit   = "exit"
)

type Request struct {
	Type    string   `json:"type"`
	Version int      `json:"version"`
	Restore *Restore `json:"restore,omitempty"`
	Exec    *Exec    `json:"exec,omitempty"`
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

type Response struct {
	Type     string `json:"type"`
	Version  int    `json:"version,omitempty"`
	Guest    string `json:"guest,omitempty"`
	Error    string `json:"error,omitempty"`
	Data     []byte `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}
