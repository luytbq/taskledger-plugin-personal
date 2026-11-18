// taskledger-plugin-personal is the taskledger plugin for work outside the
// company: study, personal projects, personal goals. A task is a directory
// <PERSONAL_ROOT>/<id>/ holding an append-only history.md and a notes.md whose
// first heading carries the title. There are no stages and no gates.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	protocol = 1
	idPrefix = "ME_"
	noteMax  = 160
)

// Events and the state each one leaves the task in. Any event not listed here
// leaves the task active.
var events = []string{"start", "note", "next", "wait", "pause", "resume", "close"}

var eventState = map[string]string{"wait": "waiting", "pause": "paused", "close": "closed"}

var vn = time.FixedZone("ICT", 7*3600)

const stampLayout = "2006-01-02 15:04"

type taskRef struct {
	ID      string `json:"id"`
	Locator string `json:"locator"`
}

type request struct {
	Protocol int             `json:"protocol"`
	Verb     string          `json:"verb"`
	Task     *taskRef        `json:"task"`
	Args     []string        `json:"args"`
	Input    json.RawMessage `json:"input"`
}

type entry struct {
	Time  string  `json:"time"`
	Stage *string `json:"stage"`
	Event string  `json:"event"`
	Note  string  `json:"note"`
}

type status struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	State        string   `json:"state"`
	Stage        string   `json:"stage"`
	LastActivity *string  `json:"last_activity"`
	Next         string   `json:"next"`
	Due          *string  `json:"due"`
	Flags        []string `json:"flags"`
	Tags         []string `json:"tags"`
}

type show struct {
	status
	Summary string   `json:"summary"`
	Pending []string `json:"pending"`
	Recent  []entry  `json:"recent"`
}

func main() {
	if len(os.Args) != 2 {
		fatal("usage: taskledger-plugin-personal <verb> < request.json")
	}
	var req request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		fatal("read request: " + err.Error())
	}
	reply, err := handle(os.Args[1], req, rootDir(), time.Now().In(vn))
	if err != nil {
		fatal(err.Error())
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(reply); err != nil {
		fatal(err.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func rootDir() string {
	root := os.Getenv("PERSONAL_ROOT")
	if strings.HasPrefix(root, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			root = filepath.Join(home, root[2:])
		}
	}
	return root
}

func handle(verb string, req request, root string, now time.Time) (any, error) {
	if verb == "hello" {
		return map[string]any{
			"protocol": protocol,
			"verbs":    []string{"hello", "new", "status", "show", "log", "guide", "locate"},
			"children": true,
		}, nil
	}
	if root == "" {
		return nil, errors.New("PERSONAL_ROOT is not set; set it in the plugin env of the tlg config")
	}
	switch verb {
	case "new":
		return newTask(req, root, now)
	case "locate":
		var in struct{ ID string `json:"id"` }
		if err := json.Unmarshal(req.Input, &in); err != nil {
			return nil, err
		}
		return locate(root, in.ID)
	}
	if req.Task == nil {
		return nil, fmt.Errorf("%s needs a task", verb)
	}
	dir := filepath.Join(root, req.Task.Locator)
	switch verb {
	case "status":
		return readStatus(dir, req.Task.ID, now)
	case "show":
		return readShow(dir, req.Task.ID, now)
	case "log":
		return appendLog(dir, req.Args, now)
	case "guide":
		var in struct{ Topic *string `json:"topic"` }
		if len(req.Input) > 0 {
			if err := json.Unmarshal(req.Input, &in); err != nil {
				return nil, err
			}
		}
		return guide(in.Topic)
	}
	return nil, fmt.Errorf("unknown verb %q", verb)
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func newTask(req request, root string, now time.Time) (any, error) {
	var in struct {
		ID    *string `json:"id"`
		Title string  `json:"title"`
	}
	if err := json.Unmarshal(req.Input, &in); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, errors.New("title is empty")
	}
	if len(req.Args) > 0 {
		return nil, fmt.Errorf("new takes no plugin arguments, got %q", req.Args)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	id := ""
	if in.ID != nil {
		id = *in.ID
	} else {
		var err error
		if id, err = proposeID(root, now); err != nil {
			return nil, err
		}
	}
	if !idPattern.MatchString(id) {
		return nil, fmt.Errorf("task id %q has characters a directory name should not", id)
	}
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("task directory %s already exists", dir)
		}
		return nil, err
	}
	notes := fmt.Sprintf("# %s: %s\n", id, in.Title)
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte(notes), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "history.md"), []byte(historyHeader), 0o644); err != nil {
		return nil, err
	}
	if _, err := appendLog(dir, []string{"start", "created"}, now); err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "locator": id, "title": in.Title}, nil
}

// proposeID picks ME_<date>_<n>, n one past the highest number used today.
func proposeID(root string, now time.Time) (string, error) {
	prefix := idPrefix + now.Format("20060102") + "_"
	ents, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	n := 0
	for _, e := range ents {
		var k int
		if rest, ok := strings.CutPrefix(e.Name(), prefix); ok {
			if _, err := fmt.Sscanf(rest, "%d", &k); err == nil && k > n {
				n = k
			}
		}
	}
	return fmt.Sprintf("%s%03d", prefix, n+1), nil
}

func locate(root, id string) (any, error) {
	dir := filepath.Join(root, id)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return map[string]any{"found": false, "locator": nil, "title": nil}, nil
	}
	title := readTitle(dir)
	return map[string]any{"found": true, "locator": id, "title": title}, nil
}

const historyHeader = "| Time | Event | Note |\n|---|---|---|\n"

func readTitle(dir string) string {
	f, err := os.Open(filepath.Join(dir, "notes.md"))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if h, ok := strings.CutPrefix(line, "# "); ok {
			if _, title, ok := strings.Cut(h, ": "); ok {
				return title
			}
			return h
		}
	}
	return ""
}

func readHistory(dir string) ([]entry, error) {
	data, err := os.ReadFile(filepath.Join(dir, "history.md"))
	if err != nil {
		return nil, err
	}
	var out []entry
	for _, line := range strings.Split(string(data), "\n") {
		cells := splitRow(line)
		if len(cells) != 3 || cells[0] == "Time" || strings.HasPrefix(cells[0], "---") {
			continue
		}
		t, err := time.ParseInLocation(stampLayout, cells[0], vn)
		if err != nil {
			continue
		}
		out = append(out, entry{Time: t.Format(time.RFC3339), Event: cells[1], Note: cells[2]})
	}
	return out, nil
}

func splitRow(line string) []string {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}
	parts := strings.Split(line[1:len(line)-1], "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func readStatus(dir, id string, now time.Time) (status, error) {
	s := status{ID: id, Flags: []string{}, Tags: []string{}}
	if _, err := os.Stat(dir); err != nil {
		return s, fmt.Errorf("task directory %s not found", dir)
	}
	s.Title = readTitle(dir)
	rows, err := readHistory(dir)
	if err != nil {
		return s, err
	}
	if len(rows) == 0 {
		s.State = "needs-me"
		s.Flags = append(s.Flags, "history empty")
		s.Next = "log a start entry"
		return s, nil
	}
	last := rows[len(rows)-1]
	s.State = "active"
	if st, ok := eventState[last.Event]; ok {
		s.State = st
	}
	s.Stage = last.Event
	ts := last.Time
	s.LastActivity = &ts
	switch {
	case last.Event == "wait":
		s.Next = last.Note
	case s.State != "closed":
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i].Event == "next" {
				s.Next = rows[i].Note
				break
			}
		}
	}
	return s, nil
}

func readShow(dir, id string, now time.Time) (show, error) {
	s, err := readStatus(dir, id, now)
	if err != nil {
		return show{}, err
	}
	rows, _ := readHistory(dir)
	recent := rows[max(0, len(rows)-3):]
	if recent == nil {
		recent = []entry{}
	}
	summary := fmt.Sprintf("%s is %s.", s.ID, s.State)
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		summary += fmt.Sprintf(" Last entry: %s: %s.", last.Event, last.Note)
	}
	if s.Next != "" {
		summary += " Next: " + s.Next + "."
	}
	return show{status: s, Summary: summary, Pending: []string{}, Recent: recent}, nil
}

func appendLog(dir string, args []string, now time.Time) (any, error) {
	reject := func(reason string) (any, error) {
		return map[string]any{"ok": false, "reason": reason, "entry": nil}, nil
	}
	if _, err := os.Stat(filepath.Join(dir, "history.md")); err != nil {
		return nil, fmt.Errorf("history.md not found in %s", dir)
	}
	if len(args) < 2 {
		return reject("usage: log <event> <note>; events: " + strings.Join(events, ", "))
	}
	event, note := args[0], strings.TrimSpace(strings.Join(args[1:], " "))
	if !slices.Contains(events, event) {
		return reject(fmt.Sprintf("unknown event %q; use one of: %s", event, strings.Join(events, ", ")))
	}
	if note == "" {
		return reject("note is empty")
	}
	if n := utf8.RuneCountInString(note); n > noteMax {
		return reject(fmt.Sprintf("note has %d characters, max %d; put details in notes.md", n, noteMax))
	}
	if strings.ContainsAny(note, "|\n") {
		return reject("note may not contain '|' or a line break: it is a cell of a markdown table")
	}
	if event == "wait" && !strings.Contains(note, ": ") {
		return reject(`wait note must look like "<who>: <what>"`)
	}
	data, err := os.ReadFile(filepath.Join(dir, "history.md"))
	if err != nil {
		return nil, err
	}
	stamp := now.In(vn).Format(stampLayout)
	var buf bytes.Buffer
	buf.Write(data)
	if len(data) > 0 && data[len(data)-1] != '\n' {
		buf.WriteByte('\n')
	}
	fmt.Fprintf(&buf, "| %s | %s | %s |\n", stamp, event, note)
	if err := writeAtomic(filepath.Join(dir, "history.md"), buf.Bytes()); err != nil {
		return nil, err
	}
	t, _ := time.ParseInLocation(stampLayout, stamp, vn)
	return map[string]any{"ok": true, "reason": nil, "entry": entry{Time: t.Format(time.RFC3339), Event: event, Note: note}}, nil
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".history-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func guide(topic *string) (any, error) {
	g := map[string]any{
		"topic": topic,
		"rules": []string{
			"Log with: tlg <id> log <event> <note>. One line, at most 160 characters, no '|'. Time is added automatically.",
			"Log when it happens: starting, the next action changing, waiting on someone, pausing, resuming, closing.",
			`wait notes look like "<who>: <what>"; the next entry of any kind ends the wait.`,
			"Details go in notes.md in the task directory, not in the history.",
		},
		"sections": []any{},
		"refs":     []any{},
		"topics":   []map[string]string{{"name": "events", "when": "you need the meaning of each event and the state it leads to"}},
	}
	if topic == nil {
		return g, nil
	}
	if *topic != "events" {
		return nil, fmt.Errorf("unknown topic %q; topics: events", *topic)
	}
	g["sections"] = []map[string]any{{
		"title": "events",
		"items": []string{
			"start: work begins or begins again after close. State active.",
			"note: something worth remembering. State active.",
			"next: sets the next action shown on the board. State active.",
			"wait: blocked on someone else. State waiting.",
			"pause: you stop on purpose and will come back. State paused.",
			"resume: back from pause. State active.",
			"close: done. State closed.",
		},
	}}
	return g, nil
}
