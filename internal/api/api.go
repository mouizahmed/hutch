// Package api holds the methods and types shared by the CLI, server, and hutchd.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mouizahmed/hutch/internal/bucket"
	"github.com/mouizahmed/hutch/internal/config"
	"github.com/mouizahmed/hutch/internal/guestproto"
	"github.com/mouizahmed/hutch/internal/keys"
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
	BoxExecIn  = "box.exec_in" // a stream: exec with the caller's input
	BoxPTY     = "box.pty"
	BoxConsole = "box.console"
	BoxSerial  = "box.serial"
	BoxRename  = "box.rename" // hutchd only
	BoxFiles   = "box.files"  // hutchd only
	// BoxGuestVersion is the protocol version a running box's hutch-guest speaks.
	BoxGuestVersion = "box.guest_version" // hutchd only
	BoxRestore      = "box.restore"       // hutchd only
	BoxResize       = "box.resize"
	BoxPlan         = "box.plan"
	BoxArchive      = "box.archive" // hutchd only
	BoxStatus       = "box.status"
	BoxDial         = "box.dial"
	// Events is a stream of Event: the current boxes, threads, and workers, then
	// each change.
	Events = "events"

	UserGet       = "user.get"
	UserPutConfig = "user.put_config"
	UserLoginBox  = "user.login_box"
	UserCapture   = "user.capture"

	ImageBuild            = "image.build"             // hutchd only
	ImageRemoveDiagnostic = "image.remove_diagnostic" // hutchd only; temporary doctor images

	ProjectInit      = "project.init"
	TemplateBuild    = "template.build"
	TemplateSnapshot = "template.snapshot"
	TemplateList     = "template.list"
	TemplateBox      = "template.box"
	ProjectConfirm   = "project.confirm"
	ProjectRm        = "project.rm"
	ProjectList      = "project.list"
	SecretsGet       = "secrets.get"
	SecretsPut       = "secrets.put"

	ThreadNew    = "thread.new"
	ThreadSend   = "thread.send"
	ThreadRm     = "thread.rm"
	ThreadRename = "thread.rename"
	ThreadResume = "thread.resume"

	ConfigGet = "config.get"
	ConfigPut = "config.put"

	WorkerList     = "worker.list"
	WorkerHello    = "worker.hello"
	WorkerSettings = "worker.settings"
	WorkerReport   = "worker.report"
	WorkerThreads  = "worker.threads" // a box's threads changed state, between reports
	WorkerKeys     = "worker.keys"    // the keys proxy's table, to the worker
	// The keys proxy refreshed a login: its new real values, to store.
	WorkerKeysRefreshed = "worker.keys_refreshed"

	ServerInit     = "server.init"
	ServerGC       = "server.gc"
	BoxMove        = "box.move"
	WorkerDrain    = "worker.drain"
	WorkerRm       = "worker.rm"
	Hello          = "hello"
	NotifyTest     = "notify.test"
	BucketGet      = "bucket.get"
	Doctor         = "doctor"          // the server's checks
	DoctorTemplate = "doctor.template" // two diagnostic builds of a committed recipe
	KeysIdentity   = "keys.identity"   // public box identity verification keys
	WorkerDoctor   = "worker.doctor"   // hutchd only
)

// Check is one hutch doctor result: Status ok, warn, or fail, with a fix for the others.
type Check struct {
	Area   string `json:"area"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// DoctorResult is the server's checks and its version.
type DoctorResult struct {
	Version string  `json:"version"`
	Checks  []Check `json:"checks"`
}

// Plan is what hutch new would do, without doing it.
type Plan struct {
	Template string   `json:"template"` // "v3 (recipe @ 1a2b3c)"
	Size     string   `json:"size"`
	Worker   string   `json:"worker,omitempty"`
	Why      string   `json:"why"`
	Class    string   `json:"class,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// ResizeParams changes a box's size; zero values keep what it has.
type ResizeParams struct {
	ID        string `json:"id"`
	VCPUs     int    `json:"vcpus,omitempty"`
	MemMiB    int    `json:"mem_mib,omitempty"`
	DiskBytes int64  `json:"disk_bytes,omitempty"`
}

type MoveParams struct {
	ID string `json:"id"`
	To string `json:"to,omitempty"` // a worker; empty lets the scheduler choose
}

// MoveResult says how a box moved: warm (everything survived) or cold (its disk
// survived; processes restarted, agents' threads exited).
type MoveResult struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

type DrainParams struct {
	Worker string `json:"worker"`
	Undo   bool   `json:"undo,omitempty"`
}

// Protocol is the API's version, raised when a change breaks older CLIs or an
// older hutchd. hutchd accepts CLIs of its protocol and the one before.
const Protocol = 1

// Handshake is the CLI's and hutchd's protocol and version, exchanged on connecting.
type Handshake struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
}

// BucketInfo names the machine's bucket, if it has one; never its credentials.
type BucketInfo struct {
	Name string `json:"name,omitempty"`
}

// MinPassphrase is the shortest recovery passphrase accepted.
const MinPassphrase = 12

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

// Config documents the machine keeps: "machine" (the machine layer) and "user"
// (the authoritative copy of the user config). Version 0 means never written.
type ConfigDoc struct {
	Name    string `json:"name"`
	Version int64  `json:"version"`
	Content string `json:"content"`
}

type Configs struct {
	Machine ConfigDoc `json:"machine"`
	User    ConfigDoc `json:"user"`
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

type KeysRefreshedParams struct {
	Adapter string        `json:"adapter"`
	Changes []keys.Change `json:"changes"`
}

// WorkerSettingsParams carries the server-layer keys hutchd uses, and the bucket
// with the keys of the projects it hosts. hutchd keeps them in memory only.
type WorkerSettingsParams struct {
	IdleInterval    time.Duration `json:"idle_interval"`
	ThreadInterval  time.Duration `json:"thread_interval"`
	WorkingWindow   time.Duration `json:"working_window"`
	RedrawWindow    time.Duration `json:"redraw_window"`
	EchoWindow      time.Duration `json:"echo_window"`
	Checkpoint      time.Duration `json:"checkpoint"`
	FirstCheckpoint time.Duration `json:"first_checkpoint"`
	// ChunkCache is the share of the worker's disk downloaded chunks may use, in percent.
	ChunkCache float64 `json:"chunk_cache"`
	// Refresh is half the GC grace period: older chunks are written again when reused.
	Refresh time.Duration     `json:"refresh"`
	Bucket  *bucket.Config    `json:"bucket,omitempty"`
	Keys    map[string][]byte `json:"keys,omitempty"` // project ID → key
	// Overcommit above 1 turns on memory pressure handling, at these shares of the
	// worker's memory, in percent; boxes count at their peak over PeakWindow.
	Overcommit     float64       `json:"overcommit,omitempty"`
	MemoryHigh     float64       `json:"memory_high,omitempty"`
	MemoryCritical float64       `json:"memory_critical,omitempty"`
	PeakWindow     time.Duration `json:"peak_window,omitempty"`
	// PressureStall: memory stalls this long within a second trigger a check at once.
	PressureStall time.Duration `json:"pressure_stall,omitempty"`
}

// NewWorkerSettings is the part of the worker's settings that comes from config.
func NewWorkerSettings(c *config.Config) WorkerSettingsParams {
	return WorkerSettingsParams{IdleInterval: c.Duration("idle.check_interval"), ThreadInterval: c.Duration("threads.check_interval"), WorkingWindow: c.Duration("threads.working_window"), RedrawWindow: c.Duration("threads.redraw_window"), EchoWindow: c.Duration("threads.echo_window"),
		Checkpoint: c.Duration("checkpoint.interval"), FirstCheckpoint: c.Duration("checkpoint.first_after"), ChunkCache: c.Percent("machine.chunk_cache"), Refresh: c.Duration("gc.grace") / 2,
		Overcommit: c.Float("machine.overcommit"), MemoryHigh: c.Percent("machine.memory_high"), MemoryCritical: c.Percent("machine.memory_critical"),
		PeakWindow: c.Duration("machine.peak_window"), PressureStall: c.Duration("machine.pressure_stall")}
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
	// Changes and Unpushed are uncommitted files and commits on no remote, from the
	// box's latest report.
	Changes  int `json:"changes,omitempty"`
	Unpushed int `json:"unpushed,omitempty"`
	// Path is the laptop directory /workspace also appears at; threads start there.
	Path string `json:"path,omitempty"`
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
	// Image is what its image was built from: the base images, pinned by digest.
	Image string `json:"image,omitempty"`
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
	Allow        []string      `json:"allow,omitempty"`
	BuildTimeout time.Duration `json:"build_timeout"`
	QuietTimeout time.Duration `json:"quiet_timeout"`
	// Recipe is .hutch/ at Commit, file name → content: the Dockerfile's build context.
	Recipe map[string][]byte `json:"recipe,omitempty"`
	// Image is a published image to build from instead of the recipe's Dockerfile.
	Image string `json:"image,omitempty"`
	// Pins are the base images of the version being rebuilt, at their digests.
	Pins []string `json:"pins,omitempty"`
	// Worker pins the build to one worker: a template built for its snapshot class.
	Worker string `json:"worker,omitempty"`
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
	Digest  string            `json:"digest,omitempty"` // MCP change detection without returning file contents
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
	// KeepSecrets leaves the stored MCP secrets alone, for updates that don't carry them.
	KeepSecrets bool `json:"keep_secrets,omitempty"`
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
// A Thread is a terminal in a box running an agent or a command, as the box last
// reported it (threads.md).
type Thread struct {
	Box      string    `json:"box"`
	N        int       `json:"n"`
	Agent    string    `json:"agent,omitempty"` // empty for a command
	Autonomy string    `json:"autonomy,omitempty"`
	Title    string    `json:"title"`
	Created  time.Time `json:"created"`
	State    string    `json:"state,omitempty"` // working, waiting, exited
	// Unread: it stopped working while nobody was looking at it.
	Unread bool `json:"unread,omitempty"`
}

// ThreadTarget names one thread, with a new name for rename.
type ThreadTarget struct {
	Box   string `json:"box"`
	N     int    `json:"n"`
	Title string `json:"title,omitempty"`
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
	// The worker's memory in MiB, and, under overcommit, "high" when it asks the
	// server to move a working box off it, and the boxes it slept at critical.
	MemTotal     int      `json:"mem_total,omitempty"`
	MemAvailable int      `json:"mem_available,omitempty"`
	Pressure     string   `json:"pressure,omitempty"`
	ForceSlept   []string `json:"force_slept,omitempty"`
	Crashed      []Crash  `json:"crashed,omitempty"`
}

// Crash is a box whose VM died and was rolled back to its snapshot from Since
// (asleep or archived), or failed when it had none.
type Crash struct {
	Box   string    `json:"box"`
	State string    `json:"state"`
	Since time.Time `json:"since"`
}

type BoxReport struct {
	Box        Box                   `json:"box"`
	Threads    []ThreadState         `json:"threads,omitempty"`
	LastActive time.Time             `json:"last_active"`
	Git        *guestproto.GitStatus `json:"git,omitempty"`
	// MemMiB is the host memory the box's VM holds now; PeakMiB its highest
	// over the peak window.
	MemMiB  int `json:"mem_mib,omitempty"`
	PeakMiB int `json:"peak_mib,omitempty"`
}

type ThreadState struct {
	N          int       `json:"n"`
	State      string    `json:"state"`
	LastOutput time.Time `json:"last_output"`
	Bell       time.Time `json:"bell"`             // when it last rang the bell
	Viewed     bool      `json:"viewed,omitempty"` // a client shows it now
	// What its window carries (guestproto.ThreadStatus).
	Agent    string    `json:"agent,omitempty"`
	Autonomy string    `json:"autonomy,omitempty"`
	Prompt   string    `json:"prompt,omitempty"`
	Name     string    `json:"name,omitempty"`
	Created  time.Time `json:"created"`
	Title    string    `json:"title,omitempty"`
}

// WorkerThreadsParams reports a box's thread states when they change.
type WorkerThreadsParams struct {
	Box     string        `json:"box"`
	Threads []ThreadState `json:"threads"`
}

// An Event is one change: a box (without its threads) or thread as it is now, a
// worker, or a box, thread, or worker removed.
type Event struct {
	Type   string  `json:"type"`
	Box    *Box    `json:"box,omitempty"`
	Thread *Thread `json:"thread,omitempty"`
	Worker *Worker `json:"worker,omitempty"`
	// ID and N name what was removed.
	ID string `json:"id,omitempty"`
	N  int    `json:"n,omitempty"`
}

const (
	EventBox           = "box"
	EventBoxRemoved    = "box_removed"
	EventThread        = "thread"
	EventThreadRemoved = "thread_removed"
	EventWorker        = "worker"
	EventWorkerRemoved = "worker_removed"
	// EventAlert is an agent wanting you: it rang the bell or exited while no
	// one looked. Sent once, never part of the state.
	EventAlert = "alert"
	// EventSynced follows the current state when a stream starts.
	EventSynced = "synced"
)

type ThreadParams struct {
	Box      string `json:"box"`
	Agent    string `json:"agent,omitempty"`
	Cmd      string `json:"cmd,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Autonomy string `json:"autonomy,omitempty"` // full or ask
	Model    string `json:"model,omitempty"`    // passed through the adapter's model setting; empty for its default
	// Resume continues this agent session, from Files written first (hutch cloud).
	Resume string            `json:"resume,omitempty"`
	Files  []guestproto.File `json:"files,omitempty"`
	// Dir is where the thread starts, within the box's Path (default: Path).
	Dir string `json:"dir,omitempty"`
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
	Boxes   []Box    `json:"boxes,omitempty"`
	// ReservedMiB is the memory its awake boxes reserve, and UsedMiB what its
	// last report found in use; set by the server.
	ReservedMiB int  `json:"reserved_mib,omitempty"`
	UsedMiB     int  `json:"used_mib,omitempty"`
	Gone        bool `json:"gone,omitempty"` // removed; its boxes restore elsewhere
	// Draining workers take no new boxes.
	Draining bool `json:"draining,omitempty"`
}

// Disk is a worker's data volume, in bytes.
type Disk struct {
	Total int64 `json:"total,omitempty"`
	Free  int64 `json:"free,omitempty"`
}

// GVisorClass reports whether a snapshot class is gVisor's: such a machine's boxes
// take memory as they use it, their size a cap.
func GVisorClass(class string) bool { return strings.Contains(class, "/gvisor-") }

// DefaultImage is the image projects without a Dockerfile use.
const DefaultImage = "base"

// DefaultImageRef is how a project's Dockerfile names the default image (FROM it).
// Builds on the machine take it from the machine's own copy, never a registry.
const DefaultImageRef = "ghcr.io/mouizahmed/hutch-default"

// ImageBuildParams builds image Name: from a Dockerfile in Files (the build
// context), from a published image Ref, or with neither, Hutch's default image.
type ImageBuildParams struct {
	Name       string            `json:"name"`
	Files      map[string][]byte `json:"files,omitempty"`
	Ref        string            `json:"ref,omitempty"`
	CacheBytes int64             `json:"cache_bytes"`
	// Pins are base images ("ref@sha256:…") to use at those digests.
	Pins []string `json:"pins,omitempty"`
	// Reuse keeps an existing image of that name instead of building it again.
	Reuse bool `json:"reuse,omitempty"`
	// NoCache reruns Dockerfile steps; doctor uses a separate temporary image name.
	NoCache bool `json:"no_cache,omitempty"`
}

type Image struct {
	Name    string    `json:"name"`
	Sources []string  `json:"sources"` // base images, pinned by digest
	Built   time.Time `json:"built"`
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
	Credentials []string `json:"credentials,omitempty"`
	Allow       []string `json:"allow,omitempty"`
	Store       *Store   `json:"store,omitempty"` // set by the server
	Path        string   `json:"path,omitempty"`
	// Branch makes the box take over an existing branch at Commit instead of
	// starting hutch/<id> (hutch cloud), with Delta applied.
	Branch string `json:"branch,omitempty"`
	Commit string `json:"commit,omitempty"`
	Delta  *Delta `json:"delta,omitempty"`
}

// Delta is local work a box starts with: commits origin lacks, as a bundle of
// HEAD; staged and unstaged changes, as binary diffs; and untracked files, or a
// whole directory outside a project, as a tar extracted into /workspace.
type Delta struct {
	Bundle   []byte `json:"bundle,omitempty"`
	Staged   []byte `json:"staged,omitempty"`
	Unstaged []byte `json:"unstaged,omitempty"`
	Files    []byte `json:"files,omitempty"`
}

func (d *Delta) Size() int {
	if d == nil {
		return 0
	}
	return len(d.Bundle) + len(d.Staged) + len(d.Unstaged) + len(d.Files)
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
