// Package api holds the methods and types shared by the CLI, server, and hutchd.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/mouizahmed/hutch/internal/guestproto"
)

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
	BoxFiles   = "box.files"  // hutchd only
	BoxStatus  = "box.status"
	BoxDial    = "box.dial"

	UserGet       = "user.get"
	UserPutConfig = "user.put_config"
	UserLoginBox  = "user.login_box"
	UserCapture   = "user.capture"

	ProjectInit      = "project.init"
	TemplateBuild    = "template.build"
	TemplateSnapshot = "template.snapshot"
	TemplateList     = "template.list"
	TemplateBox      = "template.box"
	SecretsGet       = "secrets.get"
	SecretsPut       = "secrets.put"

	ThreadNew    = "thread.new"
	ThreadSend   = "thread.send"
	ThreadRm     = "thread.rm"
	ThreadPin    = "thread.pin"
	ThreadRename = "thread.rename"
	ThreadResume = "thread.resume"

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
	Branch     string    `json:"branch,omitempty"`
	Base       string    `json:"base,omitempty"`
	PushError  string    `json:"push_error,omitempty"`
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
	Secrets string    `json:"secrets,omitempty"` // digest of the project secrets it was built with
}

// Secrets are a project's uploads.txt files; Digest identifies their contents.
type Secrets struct {
	Project string            `json:"project"`
	Digest  string            `json:"digest"`
	Files   []guestproto.File `json:"files,omitempty"`
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
	Credentials  []string      `json:"credentials,omitempty"`
	PushInterval time.Duration `json:"push_interval,omitempty"`
	Allow        []string      `json:"allow,omitempty"`
	BuildTimeout time.Duration `json:"build_timeout"`
	QuietTimeout time.Duration `json:"quiet_timeout"`
}

type ProjectParams struct {
	Project string `json:"project"`
}

// UserLayer is the part of the user layer synced from a laptop: no secrets.
type UserLayer struct {
	Adapters  map[string]string            `json:"adapters,omitempty"` // user adapter files, by file name
	Files     map[string][]guestproto.File `json:"files,omitempty"`    // config files, by adapter
	GitName   string                       `json:"git_name,omitempty"`
	GitEmail  string                       `json:"git_email,omitempty"`
	UserSetup string                       `json:"user_setup,omitempty"` // ~/.config/hutch/user-setup.sh
}

// Credentials are one adapter's captured login files, or with a "/mcp" suffix the
// secrets split out of its MCP config.
type Credentials struct {
	Part    string            `json:"part"`
	Version int64             `json:"version"`
	Files   []guestproto.File `json:"files,omitempty"`
	Updated time.Time         `json:"updated"`
}

type UserState struct {
	Version     int64         `json:"version"`
	Layer       UserLayer     `json:"layer"`
	Credentials []Credentials `json:"credentials"` // files without data
}

type UserPutConfigParams struct {
	BaseVersion int64     `json:"base_version"`
	Layer       UserLayer `json:"layer"`
	// MCP secrets split out of config, by adapter; they replace each adapter's "/mcp" part.
	Secrets map[string][]guestproto.File `json:"secrets,omitempty"`
}

type LoginParams struct {
	Adapters []string `json:"adapters"`
}

type CaptureParams struct {
	Box     string `json:"box"`
	Adapter string `json:"adapter"`
}

type FilesParams struct {
	ID    string           `json:"id"`
	Files guestproto.Files `json:"files"`
	Watch []string         `json:"watch,omitempty"` // report hashes of these files from now on
}

// DialParams opens a stream to a port on a box's loopback: after a guestproto "ok"
// response, raw bytes.
type DialParams struct {
	ID   string `json:"id"`
	Port int    `json:"port"`
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
	Tokens  int64     `json:"tokens,omitempty"`
	Pinned  bool      `json:"pinned,omitempty"`
	Renamed bool      `json:"renamed,omitempty"` // Title is the user's, not the agent's
	// SessionID is the agent's own session, for resuming it.
	SessionID string `json:"session_id,omitempty"`
}

// ThreadTarget names one thread, with what to change for pin and rename.
type ThreadTarget struct {
	Box    string `json:"box"`
	N      int    `json:"n"`
	Title  string `json:"title,omitempty"`
	Pinned bool   `json:"pinned,omitempty"`
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
	Box        Box               `json:"box"`
	Threads    []ThreadState     `json:"threads,omitempty"`
	LastActive time.Time         `json:"last_active"`
	Files      map[string]string `json:"files,omitempty"` // hashes of watched files
	Push       *guestproto.Push  `json:"push,omitempty"`
}

type ThreadState struct {
	N          int       `json:"n"`
	State      string    `json:"state"`
	LastOutput time.Time `json:"last_output"`
	Title      string    `json:"title,omitempty"` // the agent's own title for the conversation
	Tokens     int64     `json:"tokens,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
}

type ThreadParams struct {
	Box      string `json:"box"`
	Agent    string `json:"agent,omitempty"`
	Cmd      string `json:"cmd,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Autonomy string `json:"autonomy,omitempty"` // full or ask
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
	ID        string   `json:"id"`
	Image     string   `json:"image"`
	VCPUs     int      `json:"vcpus"`
	MemMiB    int      `json:"mem_mib"`
	DiskBytes int64    `json:"disk_bytes,omitempty"`
	Worker    string   `json:"worker,omitempty"`
	Idle      Idle     `json:"idle"`
	Allow     []string `json:"allow,omitempty"` // private host:port targets the box may reach
}

// ForkParams forks Source, or with Project the project's newest template version,
// then checks out Base (empty: the repo's default branch) on branch hutch/<id>.
type ForkParams struct {
	Source  string `json:"source,omitempty"`
	Project string `json:"project,omitempty"`
	Base    string `json:"base,omitempty"`
	ID      string `json:"id"`
	Idle    Idle   `json:"idle"`
	// Logins the box gets ("*": all), resolved from the project's grants.
	Credentials  []string      `json:"credentials,omitempty"`
	PushInterval time.Duration `json:"push_interval,omitempty"`
	Allow        []string      `json:"allow,omitempty"`
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

// SecretsDigest identifies a set of secret files by their paths and contents.
func SecretsDigest(files []guestproto.File) string {
	sorted := append([]guestproto.File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s\x00%d\x00", f.Path, len(f.Data))
		h.Write(f.Data)
	}
	return hex.EncodeToString(h.Sum(nil))
}
