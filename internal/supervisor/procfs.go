package supervisor

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Process enumeration, read from /proc.
//
// This exists twice over. The obvious consumer is the console's explorer, which
// shows an operator what is actually running inside a task. The other is section
// 2.4's idle detector, whose fourth condition -- "no tool executing" -- is the
// one that stops a one-hour build being reaped, because a build produces no PTY
// output and no model calls while it runs. Both need the same walk, so it lives
// in one place rather than being written twice and drifting.
//
// The trap section 2.4 records: MCP servers are long-lived children too, so a
// naive "has any child means busy" test reads as permanently busy and disables
// idle detection altogether. Start time is what separates them -- MCP servers
// come up with Claude Code, tool invocations start later -- which is why
// StartTicks is captured rather than only the process set.
//
// Deliberately absent: /proc/<pid>/environ. It carries the gateway credential in
// the supervisor's own process and provider credentials in others, and nothing
// an operator needs is worth publishing that. Cmdline IS included, because a
// process list without it answers none of the questions worth asking -- but it
// is a real exposure, since a user's own `mysql -pSECRET` shows up there.

// ProcInfo is one process inside the task.
type ProcInfo struct {
	PID  int    `json:"pid"`
	PPID int    `json:"ppid"`
	Name string `json:"name"`
	// State is the single-letter /proc state: R running, S sleeping, D
	// uninterruptible, Z zombie.
	State   string  `json:"state"`
	RSSKB   int64   `json:"rss_kb"`
	CPUSecs float64 `json:"cpu_secs"`
	// StartedAt is absolute wall-clock, derived from boot time plus the
	// process's start ticks, so the console does not have to know about jiffies.
	StartedAt string `json:"started_at,omitempty"`
	Cmdline   string `json:"cmdline,omitempty"`
	// SessionID is the session this process descends from, empty for anything
	// outside a session's tree (the supervisor itself, the entrypoint).
	SessionID string `json:"session_id,omitempty"`
}

// procRoot is a variable so tests can point the walk at a fixture tree instead
// of the live /proc, which is neither reproducible nor writable.
var procRoot = "/proc"

const clockTicks = 100 // _SC_CLK_TCK; 100 on every Linux target we run on.

// readProcesses walks /proc once and attributes each process to the session it
// descends from.
//
// sessionPIDs maps a session id to its own process id. Attribution walks the
// ppid chain upward, so a Bash tool three levels deep under Claude Code still
// lands on the right conversation.
func readProcesses(sessionPIDs map[string]int) ([]ProcInfo, bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, false // not Linux, or no procfs: "unknown", not "empty"
	}

	boot := bootTime()
	byPID := make(map[int]*ProcInfo, len(entries))
	var out []ProcInfo

	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // /proc holds plenty that is not a process
		}
		p, ok := readProcess(pid, boot)
		if !ok {
			continue // exited between the readdir and the read; normal
		}
		out = append(out, p)
	}
	for i := range out {
		byPID[out[i].PID] = &out[i]
	}

	owner := make(map[int]string, len(sessionPIDs))
	for sid, pid := range sessionPIDs {
		owner[pid] = sid
	}
	for i := range out {
		out[i].SessionID = attribute(&out[i], byPID, owner)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, true
}

// attribute walks up the parent chain until it finds a session's own pid.
//
// Bounded rather than looping until pid 1: /proc is read without a lock, so a
// process can be reparented mid-walk and produce a cycle. A depth cap is
// cheaper than cycle detection and just as correct here.
func attribute(p *ProcInfo, byPID map[int]*ProcInfo, owner map[int]string) string {
	for cur, depth := p, 0; cur != nil && depth < 64; depth++ {
		if sid, ok := owner[cur.PID]; ok {
			return sid
		}
		if cur.PPID <= 0 {
			return ""
		}
		cur = byPID[cur.PPID]
	}
	return ""
}

func readProcess(pid int, boot time.Time) (ProcInfo, bool) {
	dir := filepath.Join(procRoot, strconv.Itoa(pid))
	statBytes, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return ProcInfo{}, false
	}
	p, ok := parseStat(string(statBytes))
	if !ok {
		return ProcInfo{}, false
	}
	if !boot.IsZero() && p.startTicks > 0 {
		started := boot.Add(time.Duration(float64(p.startTicks) / clockTicks * float64(time.Second)))
		p.info.StartedAt = started.UTC().Format(time.RFC3339)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		p.info.Cmdline = redactSecrets(cmdline(b))
	}
	return p.info, true
}

// secretFlags name arguments whose VALUE is a credential. The supervisor's own
// argv carries -token, the workspace tunnel credential, and it was rendered in
// full in the console the first time this panel was pointed at a real task.
//
// That is the same hole /proc/<pid>/environ was excluded to avoid, arriving by a
// different door -- so the exclusion has to cover argv too, or it only protects
// against the mistake that was anticipated.
var secretFlags = map[string]bool{
	"-token": true, "--token": true,
	"-gateway-key": true, "--gateway-key": true,
	"-key": true, "--key": true,
	"-secret": true, "--secret": true,
	"-password": true, "--password": true,
	"-api-key": true, "--api-key": true,
	"-credential": true, "--credential": true,
}

// redactSecrets blanks the value after any flag known to carry one, and blanks
// the "=" form too.
//
// Best-effort by nature: it cannot catch a credential a user passes in a shape
// it does not know, and `curl -H "Authorization: ..."` will still show. What it
// does guarantee is that the credentials WE put on a command line do not leak
// through a panel we added, which is the part that is ours to get right.
func redactSecrets(s string) string {
	if s == "" {
		return s
	}
	fields := strings.Split(s, " ")
	for i, f := range fields {
		if k, _, ok := strings.Cut(f, "="); ok && secretFlags[k] {
			fields[i] = k + "=<redacted>"
			continue
		}
		if secretFlags[f] && i+1 < len(fields) {
			fields[i+1] = "<redacted>"
		}
	}
	return strings.Join(fields, " ")
}

type statFields struct {
	info       ProcInfo
	startTicks int64
}

// parseStat reads /proc/<pid>/stat.
//
// The comm field is parenthesised and may itself contain spaces and brackets --
// a process can name itself "(evil) 1 2 3" -- so the fields after it are located
// from the LAST ')' rather than by splitting the whole line. Splitting naively
// is the classic /proc parsing bug and it is trivially attacker-controlled.
func parseStat(s string) (statFields, bool) {
	open := strings.IndexByte(s, '(')
	close := strings.LastIndexByte(s, ')')
	if open < 0 || close < 0 || close < open {
		return statFields{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(s[:open]))
	if err != nil {
		return statFields{}, false
	}
	name := s[open+1 : close]

	rest := strings.Fields(s[close+1:])
	// Indices are relative to the field after comm: 0 state, 1 ppid, 11 utime,
	// 12 stime, 19 starttime, 21 rss (in pages).
	if len(rest) < 22 {
		return statFields{}, false
	}
	num := func(i int) int64 {
		v, _ := strconv.ParseInt(rest[i], 10, 64)
		return v
	}
	utime, stime := num(11), num(12)
	return statFields{
		info: ProcInfo{
			PID:     pid,
			PPID:    int(num(1)),
			Name:    name,
			State:   rest[0],
			RSSKB:   num(21) * int64(os.Getpagesize()) / 1024,
			CPUSecs: float64(utime+stime) / clockTicks,
		},
		startTicks: num(19),
	}, true
}

// cmdline joins the NUL-separated argv, capped so one pathological command
// cannot dominate a listing or push the reply past the tunnel's 1 MiB frame.
func cmdline(b []byte) string {
	const max = 512
	s := strings.TrimRight(string(b), "\x00")
	s = strings.ReplaceAll(s, "\x00", " ")
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// bootTime converts the monotonic-ish start ticks in /proc/<pid>/stat into wall
// clock. Returns the zero time when unavailable, and callers then omit
// StartedAt rather than reporting an epoch date.
func bootTime() time.Time {
	b, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
		if err != nil {
			return time.Time{}
		}
		return time.Unix(secs, 0)
	}
	return time.Time{}
}
