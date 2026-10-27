package config

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Layer is where a value comes from, lowest precedence first.
type Layer int

const (
	Default Layer = iota
	Server
	User
	Project
	Flag
)

func (l Layer) String() string {
	return [...]string{"default", "server", "user", "project", "flag"}[l]
}

type Kind int

const (
	String Kind = iota
	Bool
	Int
	Duration
	Size
	Percent
	Enum
	List
	Float
)

// Key is one setting in the schema. Values are kept as TOML decodes them (string,
// int64, bool, []any) and parsed by Kind: Duration to time.Duration, Size to bytes
// (int64), Percent to float64, List to []string.
type Key struct {
	Path     string // dotted; a "*" segment stands for any name, as in grants.*.autonomy
	Kind     Kind
	Default  any
	Layers   []Layer
	Enum     []string // for Enum; least to most permissive for request keys
	Min, Max string   // inclusive bounds, written like values
	// Request keys from a project only take effect up to the user's own value plus
	// what the user granted that project.
	Request bool
	Doc     string
}

var (
	server      = []Layer{Server}
	user        = []Layer{User}
	project     = []Layer{Project}
	userProject = []Layer{User, Project}
	serverUser  = []Layer{Server, User}
)

// Keys is the one schema for every layer.
var Keys = []*Key{
	{Path: "server", Kind: String, Layers: user,
		Doc: "SSH target of the server, like me@home-server; empty means a server on this machine"},
	{Path: "default_agent", Kind: String, Default: "claude", Layers: userProject,
		Doc: "agent for new threads without --agent or --cmd"},

	{Path: "size.cpu", Kind: Int, Default: int64(4), Min: "1", Max: "64", Layers: project,
		Doc: "vCPUs of every new box"},
	{Path: "size.memory", Kind: Size, Default: "8G", Min: "1G", Max: "512G", Layers: project,
		Doc: "memory of every new box"},
	{Path: "disk.size", Kind: Size, Default: "50G", Min: "8G", Max: "16T", Layers: project,
		Doc: "disk of every new box (sparse)"},
	{Path: "idle_timeout", Kind: Duration, Default: "10m", Min: "1m", Max: "7d", Layers: project,
		Doc: "a box sleeps after this long without activity"},
	{Path: "idle.cpu_threshold", Kind: Percent, Default: "5%", Min: "0%", Max: "100%", Layers: project,
		Doc: "CPU use of a box's vCPUs above which it counts as active"},
	{Path: "base", Kind: String, Layers: project,
		Doc: "branch new boxes start from; empty means the repo's default branch"},

	{Path: "autonomy", Kind: Enum, Enum: []string{"ask", "full"}, Default: "full", Layers: userProject, Request: true,
		Doc: "full: agents run without asking for permission; ask: they ask"},
	{Path: "credentials", Kind: List, Default: []any{"*"}, Layers: userProject, Request: true,
		Doc: "logins boxes get; * means all of them"},
	{Path: "network.allow", Kind: List, Layers: project, Request: true,
		Doc: "private targets (host:port) boxes may reach"},
	{Path: "network.egress", Kind: Enum, Enum: []string{"allowlist", "open"}, Default: "open", Layers: userProject, Request: true,
		Doc: "open: any public address; allowlist: only network.domains"},
	{Path: "network.domains", Kind: List, Layers: project,
		Doc: "domains boxes may reach when network.egress is allowlist"},

	{Path: "git.push_interval", Kind: Duration, Default: "1m", Min: "10s", Max: "1h", Layers: project,
		Doc: "a box pushes its branch at most this often after new commits"},
	{Path: "template.build_timeout", Kind: Duration, Default: "1h", Min: "1m", Max: "24h", Layers: project,
		Doc: "a template build that runs longer fails"},
	{Path: "template.quiet_timeout", Kind: Duration, Default: "2m", Min: "1s", Max: "1h", Layers: project,
		Doc: "how long a snapshot waits for queued systemd jobs and transient units to finish"},

	{Path: "retention.template_versions", Kind: Int, Default: int64(2), Min: "1", Max: "100", Layers: server,
		Doc: "template versions kept per project, besides those boxes were forked from"},
	{Path: "worker.memory_margin", Kind: Percent, Default: "10%", Min: "0%", Max: "50%", Layers: server,
		Doc: "RAM held back for the host on every worker"},
	{Path: "worker.overcommit", Kind: Float, Default: 1.0, Min: "1", Max: "4", Layers: server,
		Doc: "1 means every box reserves its full size, and nothing is ever slept or moved for memory; above 1, boxes are placed by measured peak use, with their full sizes allowed up to this multiple of a worker's memory, and boxes may be slept or moved under pressure"},
	{Path: "worker.memory_high", Kind: Percent, Default: "80%", Min: "50%", Max: "95%", Layers: server,
		Doc: "with overcommit: past this share of a worker's memory, it takes no new boxes, reclaims guest memory, sleeps idle boxes, then moves working ones"},
	{Path: "worker.memory_critical", Kind: Percent, Default: "90%", Min: "60%", Max: "98%", Layers: server,
		Doc: "with overcommit: past this, with nowhere to move, the least recently active working box sleeps and you're notified"},
	{Path: "worker.peak_window", Kind: Duration, Default: "1h", Min: "5m", Max: "7d", Layers: server,
		Doc: "with overcommit: a box counts at its highest measured memory use over this window"},
	{Path: "worker.disk_reserve", Kind: Percent, Default: "10%", Min: "0%", Max: "50%", Layers: server,
		Doc: "free disk below which a worker takes no new boxes"},
	{Path: "idle.check_interval", Kind: Duration, Default: "15s", Min: "1s", Max: "5m", Layers: server,
		Doc: "how often workers check boxes for activity"},
	{Path: "threads.working_window", Kind: Duration, Default: "15s", Min: "1s", Max: "10m", Layers: server,
		Doc: "a thread with output this recent counts as working"},

	{Path: "checkpoint.interval", Kind: Duration, Default: "15m", Min: "1m", Max: "24h", Layers: server,
		Doc: "awake, active boxes are snapshotted to the bucket this often: the most work losing a worker loses"},
	{Path: "archive_after", Kind: Duration, Default: "30d", Min: "1d", Max: "3650d", Layers: user,
		Doc: "a box asleep this long drops its memory and keeps its disk; waking it is a cold boot"},
	{Path: "worker.chunk_cache", Kind: Percent, Default: "30%", Min: "0%", Max: "90%", Layers: server,
		Doc: "share of a worker's disk for chunks downloaded from the bucket"},
	{Path: "worker.lost_after", Kind: Duration, Default: "5m", Min: "30s", Max: "24h", Layers: server,
		Doc: "a box whose worker has been offline this long is restored on another worker when you use it"},
	{Path: "retention.box_checkpoints", Kind: Int, Default: int64(3), Min: "1", Max: "100", Layers: server,
		Doc: "snapshots kept in the bucket per box"},
	{Path: "backup.interval", Kind: Duration, Default: "1m", Min: "10s", Max: "1h", Layers: server,
		Doc: "how often the server backs up its database to the bucket, when it changed"},
	{Path: "backup.warn_after", Kind: Duration, Default: "5m", Min: "1m", Max: "24h", Layers: server,
		Doc: "hutch doctor warns when changes have waited this long for a backup"},
	{Path: "retention.backups", Kind: Duration, Default: "30d", Min: "1d", Max: "3650d", Layers: server,
		Doc: "server backups are kept this long: all from the last hour, then one per hour for a day, then one per day"},
	{Path: "gc.interval", Kind: Duration, Default: "24h", Min: "1h", Max: "30d", Layers: server,
		Doc: "how often the server deletes bucket chunks nothing uses"},
	{Path: "providers.aws.worker_type", Kind: String, Default: "m8i.2xlarge", Layers: server,
		Doc: "instance type of every AWS worker (nested virtualization: c8i, m8i, r8i)"},
	{Path: "worker_disk.size", Kind: Size, Default: "100G", Min: "20G", Max: "16T", Layers: server,
		Doc: "data disk of every elastic worker; billed while it is stopped too"},
	{Path: "providers.teardown_after", Kind: Duration, Default: "20m", Min: "1m", Max: "7d", Layers: server,
		Doc: "an elastic worker with no awake boxes for this long is stopped"},
	{Path: "providers.empty_worker", Kind: Enum, Enum: []string{"stop", "terminate"}, Default: "stop", Layers: server,
		Doc: "stop: keep its disk so it starts again fast; terminate: delete it"},
	{Path: "providers.prestart", Kind: Bool, Default: false, Layers: serverUser,
		Doc: "start an elastic worker in the background when you open the TUI and none has room, so it boots while you type; off, opening Hutch never starts anything billable"},
	{Path: "providers.prestart_timeout", Kind: Duration, Default: "5m", Min: "1m", Max: "1h", Layers: server,
		Doc: "a worker started early that nothing lands on is stopped after this long"},
	{Path: "providers.stopped_pool", Kind: Int, Default: int64(2), Min: "0", Max: "100", Layers: server,
		Doc: "stopped elastic workers kept for fast starts; extras are terminated"},
	{Path: "providers.stopped_ttl", Kind: Duration, Default: "7d", Min: "1h", Max: "365d", Layers: server,
		Doc: "an elastic worker stopped this long is terminated"},
	{Path: "limits.cloud_workers", Kind: Int, Default: int64(2), Min: "0", Max: "1000", Layers: serverUser,
		Doc: "elastic workers running at once; past it, boxes queue (the server's value caps yours)"},
	{Path: "limits.awake_boxes", Kind: Int, Default: int64(8), Min: "0", Max: "10000", Layers: serverUser,
		Doc: "boxes awake at once across all workers; past it, boxes queue (the server's value caps yours)"},
	{Path: "limits.box_max_awake", Kind: Duration, Default: "8h", Min: "1m", Max: "365d", Layers: serverUser,
		Doc: "a box awake this long without a break is put to sleep, even mid-task (the server's value caps yours)"},
	{Path: "limits.cloud_spend_per_day", Kind: Int, Default: int64(15), Min: "0", Max: "1000000", Layers: serverUser,
		Doc: "USD a day of estimated cloud cost (workers, disks, addresses, bucket); past it, boxes queue; 0 means no limit"},
	{Path: "costs.reconcile_tolerance", Kind: Percent, Default: "10%", Min: "1%", Max: "100%", Layers: server,
		Doc: "a day's bill differing from the estimate by more than this notifies you"},
	{Path: "notify", Kind: String, Layers: user,
		Doc: "command the server runs on events, with HUTCH_EVENT, HUTCH_BOX, HUTCH_TITLE, HUTCH_MESSAGE set"},
	{Path: "gc.grace", Kind: Duration, Default: "24h", Min: "1h", Max: "30d", Layers: server,
		Doc: "chunks younger than this are never deleted, so a sweep can't race an upload"},
}

func init() {
	for _, k := range Keys {
		if !k.Request {
			continue
		}
		Keys = append(Keys, &Key{Path: "grants.*." + k.Path, Kind: k.Kind, Enum: k.Enum, Layers: user,
			Doc: "granted to this project: " + k.Doc})
	}
	for _, k := range Keys {
		if k.Default != nil {
			if _, err := k.Parse(k.Default); err != nil {
				panic(fmt.Sprintf("config: default of %s: %v", k.Path, err))
			}
		}
	}
}

// Lookup finds the key for a path. Paths match keys segment by segment.
func Lookup(path []string) *Key {
	for _, k := range Keys {
		segs := strings.Split(k.Path, ".")
		if len(segs) != len(path) {
			continue
		}
		match := true
		for i, s := range segs {
			if s != "*" && s != path[i] {
				match = false
				break
			}
		}
		if match {
			return k
		}
	}
	return nil
}

func (k *Key) allowed(l Layer) bool { return slices.Contains(k.Layers, l) }

// Parse checks a raw value against the key and returns its parsed form.
func (k *Key) Parse(raw any) (any, error) {
	v, err := k.parse(raw)
	if err != nil {
		return nil, err
	}
	if k.Min != "" && k.compare(v, k.Min) < 0 {
		return nil, fmt.Errorf("below the minimum %s", k.Min)
	}
	if k.Max != "" && k.compare(v, k.Max) > 0 {
		return nil, fmt.Errorf("above the maximum %s", k.Max)
	}
	return v, nil
}

func (k *Key) parse(raw any) (any, error) {
	switch k.Kind {
	case Bool:
		if b, ok := raw.(bool); ok {
			return b, nil
		}
		return nil, fmt.Errorf("want true or false, got %s", Display(raw))
	case Int:
		if n, ok := raw.(int64); ok {
			return n, nil
		}
		return nil, fmt.Errorf("want a whole number, got %s", Display(raw))
	case Float:
		switch n := raw.(type) {
		case float64:
			return n, nil
		case int64:
			return float64(n), nil
		case string:
			if f, err := strconv.ParseFloat(n, 64); err == nil {
				return f, nil
			}
		}
		return nil, fmt.Errorf("want a number, got %s", Display(raw))
	case List:
		items, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("want a list of strings, got %s", Display(raw))
		}
		out := make([]string, len(items))
		for i, it := range items {
			s, ok := it.(string)
			if !ok {
				return nil, fmt.Errorf("want a list of strings, got %s", Display(raw))
			}
			out[i] = s
		}
		return out, nil
	}
	s, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("want a string, got %s", Display(raw))
	}
	switch k.Kind {
	case Duration:
		return ParseDuration(s)
	case Size:
		return ParseSize(s)
	case Percent:
		return parsePercent(s)
	case Enum:
		if !slices.Contains(k.Enum, s) {
			return nil, fmt.Errorf("want one of %s, got %q", strings.Join(k.Enum, ", "), s)
		}
	}
	return s, nil
}

func (k *Key) compare(v any, bound string) int {
	var b any
	if k.Kind == Int {
		n, _ := strconv.ParseInt(bound, 10, 64)
		b = n
	} else {
		b, _ = k.parse(bound)
	}
	switch v := v.(type) {
	case int64:
		return cmp(v, b.(int64))
	case time.Duration:
		return cmp(v, b.(time.Duration))
	case float64:
		return cmp(v, b.(float64))
	}
	return 0
}

func cmp[T int64 | time.Duration | float64](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// FromString turns a command-line value into the raw form a TOML file would hold.
// Lists are comma-separated.
func (k *Key) FromString(s string) (any, error) {
	switch k.Kind {
	case Bool:
		return strconv.ParseBool(s)
	case Int:
		return strconv.ParseInt(s, 10, 64)
	case Float:
		return strconv.ParseFloat(s, 64)
	case List:
		items := []any{}
		for _, it := range strings.Split(s, ",") {
			if it = strings.TrimSpace(it); it != "" {
				items = append(items, it)
			}
		}
		return items, nil
	}
	return s, nil
}

var units = []struct {
	suffix string
	d      time.Duration
}{
	{"mo", 30 * 24 * time.Hour}, {"ms", time.Millisecond}, {"s", time.Second}, {"m", time.Minute},
	{"h", time.Hour}, {"d", 24 * time.Hour}, {"w", 7 * 24 * time.Hour},
}

// ParseDuration reads durations like "90s", "10m", "1h30m", "30d", "13mo".
func ParseDuration(s string) (time.Duration, error) {
	in := s
	var total time.Duration
	for s != "" {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 {
			return 0, fmt.Errorf("invalid duration %q", in)
		}
		n, _ := strconv.ParseInt(s[:i], 10, 64)
		s = s[i:]
		found := false
		for _, u := range units {
			if strings.HasPrefix(s, u.suffix) {
				total += time.Duration(n) * u.d
				s = s[len(u.suffix):]
				found = true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("invalid duration %q (use s, m, h, d, w, mo)", in)
		}
	}
	if in == "" {
		return 0, fmt.Errorf("empty duration")
	}
	return total, nil
}

// ParseSize reads sizes like "512M", "8G", "1T" (powers of 1024) and returns bytes.
func ParseSize(s string) (int64, error) {
	mult := map[byte]float64{'K': 1 << 10, 'M': 1 << 20, 'G': 1 << 30, 'T': 1 << 40}
	t := strings.TrimSuffix(strings.ToUpper(s), "B")
	if t == "" {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	m, ok := mult[t[len(t)-1]]
	if !ok {
		return 0, fmt.Errorf("invalid size %q (use K, M, G, T)", s)
	}
	n, err := strconv.ParseFloat(t[:len(t)-1], 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(math.Round(n * m)), nil
}

func parsePercent(s string) (float64, error) {
	n, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
	if err != nil || !strings.HasSuffix(s, "%") {
		return 0, fmt.Errorf("invalid percentage %q (like 5%%)", s)
	}
	return n, nil
}

// Display formats a raw value for people.
func Display(raw any) string {
	switch v := raw.(type) {
	case nil:
		return "(unset)"
	case string:
		return v
	case []any:
		items := make([]string, len(v))
		for i, it := range v {
			items[i] = Display(it)
		}
		return "[" + strings.Join(items, ", ") + "]"
	}
	return fmt.Sprint(raw)
}

// literal formats a raw value as TOML.
func literal(raw any) string {
	switch v := raw.(type) {
	case string:
		return quote(v)
	case []any:
		items := make([]string, len(v))
		for i, it := range v {
			items[i] = literal(it)
		}
		return "[" + strings.Join(items, ", ") + "]"
	}
	return fmt.Sprint(raw)
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\u%04X", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
