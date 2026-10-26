// Package api holds the methods and types shared by the CLI, server, and hutchd.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/sigv4"
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
	BoxRename  = "box.rename"  // hutchd only
	BoxFiles   = "box.files"   // hutchd only
	BoxRestore = "box.restore" // hutchd only
	BoxArchive = "box.archive" // hutchd only
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
	ProjectConfirm   = "project.confirm"
	ProjectRm        = "project.rm"
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

	ServerInit  = "server.init"
	ServerGC    = "server.gc"
	ProviderAdd = "provider.add"
	CostsGet    = "costs.get"
	BoxMove     = "box.move"
	WorkerDrain = "worker.drain"
	WorkerRm    = "worker.rm"
)

type MoveParams struct {
	ID string `json:"id"`
	To string `json:"to,omitempty"` // a worker; empty lets the scheduler choose
}

// MoveResult says how a box moved: warm (everything survived) or cold (its disk
// survived; processes restarted and agents resumed).
type MoveResult struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type DrainParams struct {
	Worker string `json:"worker"`
	Undo   bool   `json:"undo,omitempty"`
}

// CostsParams asks for the last Days days of the ledger, by box or by project.
type CostsParams struct {
	Days int    `json:"days"`
	By   string `json:"by,omitempty"`
}

// CostRow is one ledger amount. Box is "" for fleet overhead; by project, it holds
// the project.
type CostRow struct {
	Day      string  `json:"day"`
	Box      string  `json:"box,omitempty"`
	Category string  `json:"category"`
	USD      float64 `json:"usd"`
}

type Costs struct {
	Rows   []CostRow `json:"rows"`
	Billed []CostRow `json:"billed"`
}

// ProviderParams connects the server to AWS with credentials that can run instances.
type ProviderParams struct {
	Credentials     sigv4.Credentials `json:"credentials"`
	InstanceProfile string            `json:"instance_profile,omitempty"`
	// Set by hutch server init, which tagged what it created with Fleet.
	Fleet        string `json:"fleet,omitempty"`
	ServerDiskGB int    `json:"server_disk_gb,omitempty"`
}

type ProviderInfo struct {
	Region  string `json:"region"`
	Image   string `json:"image"`
	Address string `json:"address"`
}

// ServerInitParams connects the server to its bucket and backs up its master key
// there, wrapped by the recovery passphrase.
type ServerInitParams struct {
	Bucket     bucket.Config `json:"bucket"`
	Passphrase string        `json:"passphrase"`
}

type GCResult struct {
	Manifests int   `json:"manifests"`
	Pruned    int   `json:"pruned"` // manifests removed by retention
	Chunks    int   `json:"chunks"`
	Deleted   int   `json:"deleted"`
	Bytes     int64 `json:"bytes"`
}

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

// WorkerSettingsParams carries the server-layer keys hutchd uses, and the bucket
// with the keys of the projects it hosts. hutchd keeps them in memory only.
type WorkerSettingsParams struct {
	IdleInterval  time.Duration `json:"idle_interval"`
	WorkingWindow time.Duration `json:"working_window"`
	Checkpoint    time.Duration `json:"checkpoint"`
	// ChunkCache is the share of the worker's disk downloaded chunks may use, in percent.
	ChunkCache float64 `json:"chunk_cache"`
	// Refresh is half the GC grace period: older chunks are written again when reused.
	Refresh time.Duration     `json:"refresh"`
	Bucket  *bucket.Config    `json:"bucket,omitempty"`
	Keys    map[string][]byte `json:"keys,omitempty"` // project ID → key
}

// Store places a box's snapshots in the bucket (see worker.Store).
type Store struct {
	Project string `json:"project"`
	Prefix  string `json:"prefix"`
}

// RestoreParams creates box ID on a worker from generation Generation in the bucket.
type RestoreParams struct {
	ID         string `json:"id"`
	Store      Store  `json:"store"`
	Generation int    `json:"generation"`
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
	Uploaded   int       `json:"uploaded,omitempty"` // the generation the bucket has
	Class      string    `json:"class"`
	ReachIP    string    `json:"reach_ip,omitempty"`
	Created    time.Time `json:"created"`
	Threads    []Thread  `json:"threads,omitempty"`
	Project    string    `json:"project,omitempty"`
	Kind       string    `json:"kind,omitempty"` // BoxTemplate, BoxVersion, or empty for a box
	Branch     string    `json:"branch,omitempty"`
	Base       string    `json:"base,omitempty"`
	PushError  string    `json:"push_error,omitempty"`
	// PerHour is the box's share of its worker's price while awake (by reserved memory).
	PerHour float64 `json:"per_hour,omitempty"`
}

const (
	// The template box of a project: where builds run and template threads work.
	BoxTemplate = "template"
	// A frozen template version: never woken, only forked.
	BoxVersion = "version"
)

// Project is a repository Hutch builds templates for. Slug names its boxes.
type Project struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Peaks *Peaks `json:"peaks,omitempty"` // measured in the latest template build
	// Confirmed identifies the project settings last confirmed in the project form.
	Confirmed string `json:"confirmed,omitempty"`
}

type ConfirmParams struct {
	Project  string `json:"project"`
	Settings string `json:"settings"`
}

// Peaks is the most memory and CPU a template build used, and the box it ran in.
type Peaks struct {
	MemMiB  int       `json:"mem_mib"`
	CPUs    float64   `json:"cpus"` // busy vCPUs
	OfVCPUs int       `json:"of_vcpus"`
	OfMiB   int       `json:"of_mib"`
	At      time.Time `json:"at"`
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
	Store *Store `json:"store,omitempty"`
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
	Disk  Disk        `json:"disk"`
	// Uploading counts boxes with snapshots not yet in the bucket; a worker isn't
	// stopped until it's zero.
	Uploading int `json:"uploading"`
	// Requests counts bucket requests since hutchd started, by price tier, for
	// cost estimates.
	Requests [2]int64 `json:"requests"`
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
	Arch     string    `json:"arch,omitempty"`
	// Classes are the snapshot classes it restores warm; Class, the newest, is new boxes'.
	Classes []string `json:"classes,omitempty"`
	Disk    Disk     `json:"disk"`
	Boxes    []Box     `json:"boxes,omitempty"`
	// ReservedMiB is the memory its awake boxes reserve; set by the server.
	ReservedMiB int `json:"reserved_mib,omitempty"`
	// Node is the cloud instance of a provisioned worker ("" for persistent ones).
	Node    string    `json:"node,omitempty"`
	Stopped time.Time `json:"stopped,omitzero"` // when the server stopped it
	Gone    bool      `json:"gone,omitempty"`   // destroyed; its boxes restore elsewhere
	// Draining workers take no new boxes.
	Draining bool    `json:"draining,omitempty"`
	PerHour  float64 `json:"per_hour,omitempty"`
}

// Disk is a worker's data volume, in bytes.
type Disk struct {
	Total int64 `json:"total,omitempty"`
	Free  int64 `json:"free,omitempty"`
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
	Store     *Store   `json:"store,omitempty"`
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
	Store        *Store        `json:"store,omitempty"` // set by the server
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
