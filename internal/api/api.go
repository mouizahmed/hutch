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
	BoxRename  = "box.rename" // hutchd only

	ProjectInit      = "project.init"
	TemplateBuild    = "template.build"
	TemplateSnapshot = "template.snapshot"
	TemplateList     = "template.list"
	TemplateBox      = "template.box"

	ThreadNew  = "thread.new"
	ThreadSend = "thread.send"

	ConfigGet = "config.get"
	ConfigPut = "config.put"

	WorkerList     = "worker.list"
	WorkerHello    = "worker.hello"
	WorkerSettings = "worker.settings"
	WorkerReport   = "worker.report"
	TokenCreate    = "token.create"
)

// Config documents the server keeps: "server" (the server layer) and "user" (the
// authoritative copy of the user config). Version 0 means never written.
type ConfigDoc struct {
	Name    string `json:"name"`
	Version int64  `json:"version"`
	Content string `json:"content"`
}

type Configs struct {
	Server ConfigDoc `json:"server"`
	User   ConfigDoc `json:"user"`
}

// ConfigPutParams replaces a document if it is still at BaseVersion.
type ConfigPutParams struct {
	Name        string `json:"name"`
	BaseVersion int64  `json:"base_version"`
	Content     string `json:"content"`
}

// ConfigPutResult holds the new document, or on a conflict the server's current one.
type ConfigPutResult struct {
	Doc      ConfigDoc `json:"doc"`
	Conflict bool      `json:"conflict,omitempty"`
}

// WorkerSettingsParams carries the server-layer keys hutchd uses.
type WorkerSettingsParams struct {
	IdleInterval  time.Duration `json:"idle_interval"`
	WorkingWindow time.Duration `json:"working_window"`
}

// Idle is a box's idle policy, resolved from its project when the box is created.
type Idle struct {
	Timeout      time.Duration `json:"timeout"`
	CPUThreshold float64       `json:"cpu_threshold"`
}

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
	Project    string    `json:"project,omitempty"`
	Kind       string    `json:"kind,omitempty"` // BoxTemplate, BoxVersion, or empty for a box
}

const (
	// The template box of a project: where builds run and template threads work.
	BoxTemplate = "template"
	// A frozen template version: never woken, only forked.
	BoxVersion = "version"
)

// Project is a repository Hutch builds templates for. Slug names its boxes.
type Project struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// Template is one frozen template version of a project.
type Template struct {
	Project string    `json:"project"`
	Version int       `json:"version"`
	Commit  string    `json:"commit"`
	Box     string    `json:"box"`
	VCPUs   int       `json:"vcpus"`
	MemMiB  int       `json:"mem_mib"`
	Created time.Time `json:"created"`
}

// TemplateParams describes a build: the recipe at Commit, cloned from CloneURL,
// in a fresh VM of the project's size.
type TemplateParams struct {
	Project      string        `json:"project"`
	CloneURL     string        `json:"clone_url"`
	Commit       string        `json:"commit"`
	VCPUs        int           `json:"vcpus"`
	MemMiB       int           `json:"mem_mib"`
	DiskBytes    int64         `json:"disk_bytes"`
	Idle         Idle          `json:"idle"`
	BuildTimeout time.Duration `json:"build_timeout"`
	QuietTimeout time.Duration `json:"quiet_timeout"`
}

type ProjectParams struct {
	Project string `json:"project"`
}

type RenameParams struct {
	ID    string `json:"id"`
	NewID string `json:"new_id"`
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
	CPUs     int       `json:"cpus,omitempty"`
	MemMiB   int       `json:"mem_mib,omitempty"`
	Boxes    []Box     `json:"boxes,omitempty"`
}

type BootParams struct {
	ID        string `json:"id"`
	Image     string `json:"image"`
	VCPUs     int    `json:"vcpus"`
	MemMiB    int    `json:"mem_mib"`
	DiskBytes int64  `json:"disk_bytes,omitempty"`
	Worker    string `json:"worker,omitempty"`
	Idle      Idle   `json:"idle"`
}

// ForkParams forks Source, or with Project the project's newest template version,
// then checks out Base (empty: the repo's default branch) on branch hutch/<id>.
type ForkParams struct {
	Source  string `json:"source,omitempty"`
	Project string `json:"project,omitempty"`
	Base    string `json:"base,omitempty"`
	ID      string `json:"id"`
	Idle    Idle   `json:"idle"`
}

type IDParams struct {
	ID string `json:"id"`
}

type ExecParams struct {
	ID   string   `json:"id"`
	Argv []string `json:"argv"`
	User string   `json:"user,omitempty"`
	Dir  string   `json:"dir,omitempty"`
	Env  []string `json:"env,omitempty"`
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
