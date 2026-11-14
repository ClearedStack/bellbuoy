package plainforge

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ClearedStack/bellbuoy/internal/codes"
	"github.com/ClearedStack/bellbuoy/internal/sanitize"
)

const (
	// DefaultTimeout bounds one smart HTTP exchange with Plainforge.
	DefaultTimeout = 30 * time.Second

	// TasksRef is the cluster-private branch in each Plainforge repository
	// where Bellbuoy records open and closed tasks. Plainforge neither
	// announces this branch to Keelworks nor mirrors it to GitHub.
	TasksRef = "refs/heads/bellbuoy/tasks"

	defaultBranch = "refs/heads/main"
	tasksFile     = "tasks.json"
	gitIdentity   = "bellbuoy <bellbuoy@clearedstack.internal>"
	gitAgent      = "agent=bellbuoy/v0.1.0-draft"

	serviceUpload  = "git-upload-pack"
	serviceReceive = "git-receive-pack"

	maxRefsBody    = 1 << 20
	maxPackBody    = 16 << 20
	maxObject      = 8 << 20
	maxPackObjects = 65536
	maxHeaderLines = 64
	maxCASAttempts = 4
)

type oid [20]byte

var zeroOID oid

func (o oid) String() string { return hex.EncodeToString(o[:]) }

func parseOID(s string) (oid, bool) {
	var o oid
	if len(s) != 40 {
		return o, false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return o, false
		}
	}
	if _, err := hex.Decode(o[:], []byte(s)); err != nil {
		return o, false
	}
	return o, true
}

type objType uint8

const (
	objCommit   objType = 1
	objTree     objType = 2
	objBlob     objType = 3
	objTag      objType = 4
	objOfsDelta objType = 6
	objRefDelta objType = 7
)

func (t objType) name() (string, bool) {
	switch t {
	case objCommit:
		return "commit", true
	case objTree:
		return "tree", true
	case objBlob:
		return "blob", true
	case objTag:
		return "tag", true
	default:
		return "", false
	}
}

type rawObj struct {
	id   oid
	typ  objType
	data []byte
}

func makeObj(typ objType, data []byte) rawObj {
	return rawObj{id: hashObject(typ, data), typ: typ, data: data}
}

func hashObject(typ objType, data []byte) oid {
	name, _ := typ.name()
	h := sha1.New()
	h.Write([]byte(name + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	var out oid
	h.Sum(out[:0])
	return out
}

type cachedTasks struct {
	tip   oid
	tasks []Task
	seed  map[oid]rawObj
}

// Client speaks Git's smart HTTP protocol to Plainforge using only the Go
// standard library. Proposed file edits go to refs/heads/<branch>, and tasks
// are kept in tasks.json on refs/heads/bellbuoy/tasks in the owning repository.
//
// Redirects are not followed and no proxy is used. Neither Plainforge's
// address nor any text Plainforge sends back appears in returned errors.
type Client struct {
	base string
	http *http.Client
	now  func() time.Time

	mu        sync.Mutex
	taskCache map[string]cachedTasks
}

var _ Forge = (*Client)(nil)

// NewClient checks address and returns a smart HTTP client for Plainforge.
// address must be an http or https URL with no path, user, query or fragment.
func NewClient(address string, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, codes.Broken("plainforge address is not usable")
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Client{
		base: u.Scheme + "://" + u.Host,
		now:  time.Now,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:               nil,
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		taskCache: map[string]cachedTasks{},
	}, nil
}

// FindOpenTask returns the open task in repo with key, reading
// refs/heads/bellbuoy/tasks from Plainforge when the branch has moved.
func (c *Client) FindOpenTask(ctx context.Context, repo, key string) (Task, bool, error) {
	if !sanitize.IsRepo(repo) {
		return Task{}, false, codes.Broken("repository name is missing or malformed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	_, tasks, _, err := c.syncTasksLocked(ctx, repo)
	if err != nil {
		return Task{}, false, err
	}
	for _, t := range tasks {
		if t.Open && t.Repo == repo && t.Key == key {
			return t, true, nil
		}
	}
	return Task{}, false, nil
}

// OpenTask records a new open task with occurrence count 1 on
// refs/heads/bellbuoy/tasks in repo.
func (c *Client) OpenTask(ctx context.Context, repo string, draft TaskDraft) (Task, error) {
	if !sanitize.IsRepo(repo) {
		return Task{}, codes.Broken("repository name is missing or malformed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		tip, tasks, _, err := c.syncTasksLocked(ctx, repo)
		if err != nil {
			return Task{}, err
		}
		if attempt > 0 {
			for _, existing := range tasks {
				if existing.Open && existing.Repo == repo && existing.Key == draft.Key &&
					existing.Title == draft.Title && existing.Body == draft.Body {
					return existing, nil
				}
			}
		}
		task := Task{
			ID:          repo + "#" + strconv.Itoa(len(tasks)+1),
			Repo:        repo,
			Key:         draft.Key,
			Title:       draft.Title,
			Body:        draft.Body,
			Occurrences: 1,
			Open:        true,
		}
		next := append(tasks, task)
		newTip, seed, conflict, err := c.writeTasksLocked(ctx, repo, tip, next, "bellbuoy: open task "+task.ID)
		if err != nil {
			return Task{}, err
		}
		if conflict {
			delete(c.taskCache, repo)
			continue
		}
		c.taskCache[repo] = cachedTasks{tip: newTip, tasks: cloneTasks(next), seed: seed}
		return task, nil
	}
	return Task{}, codes.TimedOut("plainforge was busy updating tasks")
}

// AddOccurrence adds one to the occurrence count of an open task on
// refs/heads/bellbuoy/tasks in repo.
func (c *Client) AddOccurrence(ctx context.Context, repo, taskID string) (Task, error) {
	if !sanitize.IsRepo(repo) {
		return Task{}, codes.Broken("repository name is missing or malformed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		tip, tasks, _, err := c.syncTasksLocked(ctx, repo)
		if err != nil {
			return Task{}, err
		}
		idx := -1
		for i := range tasks {
			if tasks[i].Repo == repo && tasks[i].ID == taskID && tasks[i].Open {
				idx = i
				break
			}
		}
		if idx < 0 {
			return Task{}, codes.Broken("the task to add to is not open")
		}
		tasks[idx].Occurrences++
		updated := tasks[idx]
		newTip, seed, conflict, err := c.writeTasksLocked(ctx, repo, tip, tasks, "bellbuoy: count occurrence on "+taskID)
		if err != nil {
			return Task{}, err
		}
		if conflict {
			delete(c.taskCache, repo)
			continue
		}
		c.taskCache[repo] = cachedTasks{tip: newTip, tasks: cloneTasks(tasks), seed: seed}
		return updated, nil
	}
	return Task{}, codes.TimedOut("plainforge was busy updating tasks")
}

// CloseTask marks an open task as closed on refs/heads/bellbuoy/tasks in repo.
func (c *Client) CloseTask(ctx context.Context, repo, taskID string) (bool, error) {
	if !sanitize.IsRepo(repo) {
		return false, codes.Broken("repository name is missing or malformed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		tip, tasks, _, err := c.syncTasksLocked(ctx, repo)
		if err != nil {
			return false, err
		}
		idx := -1
		for i := range tasks {
			if tasks[i].Repo == repo && tasks[i].ID == taskID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return false, nil
		}
		if !tasks[idx].Open {
			return true, nil
		}
		tasks[idx].Open = false
		newTip, seed, conflict, err := c.writeTasksLocked(ctx, repo, tip, tasks, "bellbuoy: close task "+taskID)
		if err != nil {
			return false, err
		}
		if conflict {
			delete(c.taskCache, repo)
			continue
		}
		c.taskCache[repo] = cachedTasks{tip: newTip, tasks: cloneTasks(tasks), seed: seed}
		return true, nil
	}
	return false, codes.TimedOut("plainforge was busy updating tasks")
}

// ProposeChange commits change.Content at change.Path on refs/heads/<branch>
// in change.Repo, starting from refs/heads/main when main exists. Asking again
// with the same file content reports created as false; a branch that already
// holds different content at change.Path fails with E_PARSE. It never writes
// to refs/heads/main or refs/heads/bellbuoy/tasks.
func (c *Client) ProposeChange(ctx context.Context, change Change) (bool, error) {
	if !sanitize.IsRepo(change.Repo) {
		return false, codes.Broken("repository name is missing or malformed")
	}
	if !validProposalBranch(change.Branch) {
		return false, codes.Broken("proposal branch name is missing or not allowed")
	}
	if !sanitize.IsPath(change.Path) {
		return false, codes.Broken("proposal file path is missing or malformed")
	}
	if len(change.Content) > maxObject {
		return false, codes.Broken("proposal content is larger than plainforge accepts")
	}

	branchRef := "refs/heads/" + change.Branch
	wantBlob := makeObj(objBlob, []byte(change.Content))

	refs, err := c.infoRefs(ctx, change.Repo, serviceUpload)
	if err != nil {
		return false, err
	}
	if existingTip, exists := refs[branchRef]; exists {
		return c.checkExistingBranch(ctx, change.Repo, existingTip, change.Path, wantBlob.id)
	}

	var (
		parents  []oid
		rootTree oid
		hasRoot  bool
		objs     map[oid]rawObj
	)
	if mainTip, ok := refs[defaultBranch]; ok {
		objs, err = c.fetchTip(ctx, change.Repo, mainTip, nil, nil)
		if err != nil {
			return false, err
		}
		commitObj, ok := objs[mainTip]
		if !ok || commitObj.typ != objCommit {
			return false, codes.Broken("plainforge gave an incomplete commit")
		}
		rootTree, err = parseCommitTree(commitObj.data)
		if err != nil {
			return false, err
		}
		hasRoot = true
		parents = []oid{mainTip}
	}

	newTree, writeObjs, err := upsertPathInTree(objs, rootTree, hasRoot, change.Path, wantBlob)
	if err != nil {
		return false, err
	}
	msg := strings.TrimSpace(change.Message)
	if msg == "" {
		msg = strings.TrimSpace(change.Title)
	}
	if msg == "" {
		msg = "Propose " + change.Path
	}
	commitObj := makeObj(objCommit, encodeCommit(newTree, parents, msg, c.now()))
	writeObjs = append(writeObjs, commitObj)

	conflict, err := c.pushRef(ctx, change.Repo, branchRef, zeroOID, commitObj.id, writeObjs)
	if err != nil {
		return false, err
	}
	if conflict {
		refsAfter, err := c.infoRefs(ctx, change.Repo, serviceUpload)
		if err != nil {
			return false, err
		}
		if existingTip, exists := refsAfter[branchRef]; exists {
			return c.checkExistingBranch(ctx, change.Repo, existingTip, change.Path, wantBlob.id)
		}
		return false, codes.TimedOut("plainforge refused the branch update")
	}
	return true, nil
}

// SyncMirror reports E_NEEDS_SUBTOOL: Plainforge mirrors refs/heads/main and
// refs/tags/* automatically when configured and exposes no on-demand HTTP call.
func (c *Client) SyncMirror(context.Context, string, []string) error {
	return codes.Unfamiliar("plainforge has no on-demand mirror call")
}

func (c *Client) checkExistingBranch(ctx context.Context, repo string, branchTip oid, path string, wantBlob oid) (bool, error) {
	objs, err := c.fetchTip(ctx, repo, branchTip, nil, nil)
	if err != nil {
		return false, err
	}
	commitObj, ok := objs[branchTip]
	if !ok || commitObj.typ != objCommit {
		return false, codes.Broken("plainforge gave an incomplete branch")
	}
	rootTree, err := parseCommitTree(commitObj.data)
	if err != nil {
		return false, err
	}
	gotBlob, found, err := lookupBlobInTree(objs, rootTree, path)
	if err != nil {
		return false, err
	}
	if found && gotBlob == wantBlob {
		return false, nil
	}
	return false, codes.Broken("the branch already holds a different change")
}

type taskDisk struct {
	Tasks []Task `json:"tasks"`
}

func (c *Client) syncTasksLocked(ctx context.Context, repo string) (oid, []Task, map[oid]rawObj, error) {
	refs, err := c.infoRefs(ctx, repo, serviceUpload)
	if err != nil {
		return zeroOID, nil, nil, err
	}
	tip, exists := refs[TasksRef]
	if !exists {
		delete(c.taskCache, repo)
		return zeroOID, nil, nil, nil
	}
	cached, hasCached := c.taskCache[repo]
	if hasCached && cached.tip == tip {
		return tip, cloneTasks(cached.tasks), cached.seed, nil
	}

	var have *oid
	var seed map[oid]rawObj
	if hasCached && cached.tip != zeroOID {
		prev := cached.tip
		have = &prev
		seed = cached.seed
	}
	objs, err := c.fetchTip(ctx, repo, tip, have, seed)
	if err != nil {
		return zeroOID, nil, nil, err
	}
	commitObj, ok := objs[tip]
	if !ok || commitObj.typ != objCommit {
		return zeroOID, nil, nil, codes.Broken("plainforge gave an incomplete task commit")
	}
	treeOID, err := parseCommitTree(commitObj.data)
	if err != nil {
		return zeroOID, nil, nil, err
	}
	treeObj, ok := objs[treeOID]
	if !ok || treeObj.typ != objTree {
		return zeroOID, nil, nil, codes.Broken("plainforge gave an incomplete task tree")
	}
	blobOID, found, err := lookupBlobInTree(objs, treeOID, tasksFile)
	if err != nil || !found {
		return zeroOID, nil, nil, codes.Broken("task branch does not hold tasks.json")
	}
	blobObj, ok := objs[blobOID]
	if !ok || blobObj.typ != objBlob {
		return zeroOID, nil, nil, codes.Broken("plainforge gave an incomplete task file")
	}

	dec := json.NewDecoder(bytes.NewReader(blobObj.data))
	dec.DisallowUnknownFields()
	var tf taskDisk
	if err := dec.Decode(&tf); err != nil {
		return zeroOID, nil, nil, codes.Broken("task file is not in the expected form")
	}
	if _, err := dec.Token(); err != io.EOF {
		return zeroOID, nil, nil, codes.Broken("task file is not in the expected form")
	}
	for _, t := range tf.Tasks {
		if t.Repo != repo || t.ID == "" || t.Occurrences < 1 {
			return zeroOID, nil, nil, codes.Broken("task file holds a malformed task")
		}
	}

	newSeed := map[oid]rawObj{
		treeObj.id: treeObj,
		blobObj.id: blobObj,
	}
	c.taskCache[repo] = cachedTasks{tip: tip, tasks: cloneTasks(tf.Tasks), seed: newSeed}
	return tip, cloneTasks(tf.Tasks), newSeed, nil
}

func (c *Client) writeTasksLocked(ctx context.Context, repo string, oldTip oid, tasks []Task, message string) (oid, map[oid]rawObj, bool, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(taskDisk{Tasks: tasks}); err != nil {
		return zeroOID, nil, false, codes.Broken("task file could not be encoded")
	}
	if buf.Len() > maxObject {
		return zeroOID, nil, false, codes.Broken("task file is larger than plainforge accepts")
	}

	blobObj := makeObj(objBlob, buf.Bytes())
	treeObj := makeObj(objTree, encodeTree([]treeEntry{{mode: "100644", name: tasksFile, id: blobObj.id}}))
	var parents []oid
	if oldTip != zeroOID {
		parents = []oid{oldTip}
	}
	commitObj := makeObj(objCommit, encodeCommit(treeObj.id, parents, message, c.now()))

	conflict, err := c.pushRef(ctx, repo, TasksRef, oldTip, commitObj.id, []rawObj{blobObj, treeObj, commitObj})
	if err != nil {
		return zeroOID, nil, false, err
	}
	if conflict {
		return zeroOID, nil, true, nil
	}
	seed := map[oid]rawObj{
		treeObj.id: treeObj,
		blobObj.id: blobObj,
	}
	return commitObj.id, seed, false, nil
}

func cloneTasks(in []Task) []Task {
	return append([]Task(nil), in...)
}

func validProposalBranch(branch string) bool {
	if branch == "" || len(branch) > 200 {
		return false
	}
	if branch == "main" || branch == "HEAD" || branch == "bellbuoy/tasks" || strings.HasPrefix(branch, "refs/") {
		return false
	}
	if strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") ||
		strings.HasPrefix(branch, ".") || strings.HasSuffix(branch, ".") ||
		strings.Contains(branch, "..") || strings.Contains(branch, "//") ||
		strings.HasSuffix(branch, ".lock") {
		return false
	}
	for _, seg := range strings.Split(branch, "/") {
		if seg == "" || strings.HasPrefix(seg, ".") || strings.HasSuffix(seg, ".lock") {
			return false
		}
		for i := 0; i < len(seg); i++ {
			ch := seg[i]
			ok := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
				(ch >= '0' && ch <= '9') || ch == '.' || ch == '_' || ch == '-'
			if !ok {
				return false
			}
		}
	}
	return true
}

func (c *Client) infoRefs(ctx context.Context, repo, service string) (map[string]oid, error) {
	endpoint := c.base + "/" + repo + ".git/info/refs?service=" + service
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, codes.Broken("plainforge request could not be built")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, codes.TimedOut("plainforge did not answer in time")
	}
	defer resp.Body.Close()

	if err := checkHTTPStatus(resp.StatusCode); err != nil {
		return nil, err
	}
	wantCT := "application/x-" + service + "-advertisement"
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), wantCT) {
		return nil, codes.Broken("plainforge gave an unexpected content type")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRefsBody+1))
	if err != nil {
		return nil, codes.TimedOut("plainforge answer could not be read")
	}
	if len(body) > maxRefsBody {
		return nil, codes.Broken("plainforge ref list is too large")
	}

	pr := &pktReader{r: bytes.NewReader(body)}
	first, flush, err := pr.next()
	if err != nil || flush || strings.TrimSuffix(string(first), "\n") != "# service="+service {
		return nil, codes.Broken("plainforge ref header was not understood")
	}
	_, flush, err = pr.next()
	if err != nil || !flush {
		return nil, codes.Broken("plainforge ref header did not end cleanly")
	}

	refs := map[string]oid{}
	for lineNum := 0; ; lineNum++ {
		line, flush, err := pr.next()
		if err != nil {
			return nil, codes.Broken("plainforge ref list was cut short")
		}
		if flush {
			break
		}
		text := string(line)
		if lineNum == 0 {
			text, _, _ = strings.Cut(text, "\x00")
		}
		text = strings.TrimSuffix(text, "\n")
		hexPart, name, ok := strings.Cut(text, " ")
		if !ok {
			return nil, codes.Broken("plainforge ref line was malformed")
		}
		id, ok := parseOID(hexPart)
		if !ok {
			return nil, codes.Broken("plainforge ref object id was malformed")
		}
		if lineNum == 0 && id == zeroOID && name == "capabilities^{}" {
			continue
		}
		if strings.HasSuffix(name, "^{}") || name == "HEAD" {
			continue
		}
		refs[name] = id
	}
	return refs, nil
}

func (c *Client) fetchTip(ctx context.Context, repo string, want oid, have *oid, seed map[oid]rawObj) (map[oid]rawObj, error) {
	var reqBody bytes.Buffer
	caps := gitAgent
	if have != nil && *have != zeroOID {
		caps = "multi_ack_detailed " + gitAgent
	}
	writePktString(&reqBody, "want "+want.String()+" "+caps+"\n")
	writePktFlush(&reqBody)
	if have != nil && *have != zeroOID {
		writePktString(&reqBody, "have "+have.String()+"\n")
	}
	writePktString(&reqBody, "done\n")

	endpoint := c.base + "/" + repo + ".git/" + serviceUpload
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &reqBody)
	if err != nil {
		return nil, codes.Broken("plainforge fetch request could not be built")
	}
	req.Header.Set("Content-Type", "application/x-"+serviceUpload+"-request")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, codes.TimedOut("plainforge did not answer in time")
	}
	defer resp.Body.Close()

	if err := checkHTTPStatus(resp.StatusCode); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-"+serviceUpload+"-result") {
		return nil, codes.Broken("plainforge gave an unexpected fetch content type")
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPackBody+1))
	if err != nil {
		return nil, codes.TimedOut("plainforge pack could not be read")
	}
	if len(raw) > maxPackBody {
		return nil, codes.Broken("plainforge pack is larger than accepted")
	}

	rd := bytes.NewReader(raw)
	for i := 0; ; i++ {
		if i >= maxHeaderLines {
			return nil, codes.Broken("plainforge fetch header had too many lines")
		}
		var hdr [4]byte
		if _, err := io.ReadFull(rd, hdr[:]); err != nil {
			return nil, codes.Broken("plainforge fetch ended before pack")
		}
		if string(hdr[:]) == "PACK" {
			packStream := io.MultiReader(bytes.NewReader(hdr[:]), rd)
			return unpackPack(packStream, seed)
		}
		n, ok := parsePktLen(hdr)
		if !ok {
			return nil, codes.Broken("plainforge fetch packet length was malformed")
		}
		if n == 0 {
			continue
		}
		if n < 4 {
			return nil, codes.Broken("plainforge fetch packet length was too short")
		}
		payload := make([]byte, n-4)
		if _, err := io.ReadFull(rd, payload); err != nil {
			return nil, codes.Broken("plainforge fetch packet was cut short")
		}
		line := strings.TrimSuffix(string(payload), "\n")
		if line != "NAK" && !strings.HasPrefix(line, "ACK ") {
			return nil, codes.Broken("plainforge refused the fetch")
		}
	}
}

func (c *Client) pushRef(ctx context.Context, repo, ref string, oldOID, newOID oid, objs []rawObj) (bool, error) {
	packBytes, err := buildPack(objs)
	if err != nil {
		return false, err
	}
	var reqBody bytes.Buffer
	cmd := oldOID.String() + " " + newOID.String() + " " + ref + "\x00report-status " + gitAgent + "\n"
	writePktString(&reqBody, cmd)
	writePktFlush(&reqBody)
	reqBody.Write(packBytes)

	endpoint := c.base + "/" + repo + ".git/" + serviceReceive
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &reqBody)
	if err != nil {
		return false, codes.Broken("plainforge push request could not be built")
	}
	req.Header.Set("Content-Type", "application/x-"+serviceReceive+"-request")

	resp, err := c.http.Do(req)
	if err != nil {
		return false, codes.TimedOut("plainforge did not answer in time")
	}
	defer resp.Body.Close()

	if err := checkHTTPStatus(resp.StatusCode); err != nil {
		return false, err
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/x-"+serviceReceive+"-result") {
		return false, codes.Broken("plainforge gave an unexpected push content type")
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRefsBody+1))
	if err != nil {
		return false, codes.TimedOut("plainforge push status could not be read")
	}
	if len(body) > maxRefsBody {
		return false, codes.Broken("plainforge push status is too large")
	}

	pr := &pktReader{r: bytes.NewReader(body)}
	unpackLine, flush, err := pr.next()
	if err != nil || flush {
		return false, codes.Broken("plainforge push unpack line was missing")
	}
	if strings.TrimSuffix(string(unpackLine), "\n") != "unpack ok" {
		return false, codes.Broken("plainforge refused the pushed pack")
	}

	refStatus := ""
	for {
		line, flush, err := pr.next()
		if err == io.EOF || flush {
			break
		}
		if err != nil {
			return false, codes.Broken("plainforge push status was malformed")
		}
		refStatus = strings.TrimSuffix(string(line), "\n")
	}
	if refStatus == "ok "+ref || strings.HasPrefix(refStatus, "ok "+ref+" ") {
		return false, nil
	}
	if strings.HasPrefix(refStatus, "ng "+ref+" ") {
		return true, nil
	}
	return false, codes.Broken("plainforge did not report the pushed ref")
}

func checkHTTPStatus(status int) error {
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return codes.TimedOut("plainforge asked to be tried again")
	default:
		return codes.Broken("plainforge gave an unexpected status")
	}
}

type pktReader struct {
	r io.Reader
}

func (p *pktReader) next() ([]byte, bool, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(p.r, hdr[:]); err != nil {
		return nil, false, err
	}
	n, ok := parsePktLen(hdr)
	if !ok {
		return nil, false, errors.New("bad pkt-line header")
	}
	if n == 0 {
		return nil, true, nil
	}
	if n < 4 {
		return nil, false, errors.New("short pkt-line header")
	}
	buf := make([]byte, n-4)
	if _, err := io.ReadFull(p.r, buf); err != nil {
		return nil, false, err
	}
	return buf, false, nil
}

func parsePktLen(hdr [4]byte) (int, bool) {
	n := 0
	for i := 0; i < 4; i++ {
		c := hdr[i]
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return 0, false
		}
		n = (n << 4) | v
	}
	return n, true
}

func writePktString(w *bytes.Buffer, s string) {
	const hexDigits = "0123456789abcdef"
	n := len(s) + 4
	w.WriteByte(hexDigits[(n>>12)&0xf])
	w.WriteByte(hexDigits[(n>>8)&0xf])
	w.WriteByte(hexDigits[(n>>4)&0xf])
	w.WriteByte(hexDigits[n&0xf])
	w.WriteString(s)
}

func writePktFlush(w *bytes.Buffer) {
	w.WriteString("0000")
}

// packReader counts bytes read from the start of a PACKv2 stream, hashes them,
// and implements io.ByteReader so compress/zlib never buffers past an entry.
type packReader struct {
	r   io.Reader
	h   hash.Hash
	off uint64
}

func (p *packReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.h.Write(b[:n])
		p.off += uint64(n)
	}
	return n, err
}

func (p *packReader) ReadByte() (byte, error) {
	var one [1]byte
	for {
		n, err := p.Read(one[:])
		if n == 1 {
			return one[0], nil
		}
		if err != nil {
			return 0, err
		}
	}
}

type pendingDelta struct {
	kind    objType
	baseOff uint64
	baseOID oid
	off     uint64
	delta   []byte
}

func unpackPack(r io.Reader, seed map[oid]rawObj) (map[oid]rawObj, error) {
	pr := &packReader{r: r, h: sha1.New()}
	var hdr [12]byte
	if _, err := io.ReadFull(pr, hdr[:]); err != nil {
		return nil, codes.Broken("pack header was cut short")
	}
	if string(hdr[:4]) != "PACK" {
		return nil, codes.Broken("pack signature was missing")
	}
	if binary.BigEndian.Uint32(hdr[4:8]) != 2 {
		return nil, codes.Broken("pack version was not 2")
	}
	count := binary.BigEndian.Uint32(hdr[8:12])
	if count > maxPackObjects {
		return nil, codes.Broken("pack holds too many objects")
	}

	objs := make(map[oid]rawObj, int(count)+len(seed))
	for k, v := range seed {
		objs[k] = v
	}
	byOff := make(map[uint64]oid, count)
	var deltas []pendingDelta

	for i := uint32(0); i < count; i++ {
		entryOff := pr.off
		b, err := pr.ReadByte()
		if err != nil {
			return nil, codes.Broken("pack entry header was cut short")
		}
		typ := objType((b >> 4) & 0x07)
		size := uint64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			if shift > 60 {
				return nil, codes.Broken("pack entry size overflowed")
			}
			b, err = pr.ReadByte()
			if err != nil {
				return nil, codes.Broken("pack entry header was cut short")
			}
			size |= uint64(b&0x7f) << shift
			shift += 7
		}
		if size > maxObject {
			return nil, codes.Broken("pack object is larger than accepted")
		}

		var baseOff uint64
		var baseID oid
		switch typ {
		case objCommit, objTree, objBlob, objTag:
		case objOfsDelta:
			c, err := pr.ReadByte()
			if err != nil {
				return nil, codes.Broken("ofs-delta offset was cut short")
			}
			neg := uint64(c & 0x7f)
			for c&0x80 != 0 {
				c, err = pr.ReadByte()
				if err != nil {
					return nil, codes.Broken("ofs-delta offset was cut short")
				}
				neg = ((neg + 1) << 7) | uint64(c&0x7f)
			}
			if neg == 0 || neg > entryOff {
				return nil, codes.Broken("ofs-delta offset is out of range")
			}
			baseOff = entryOff - neg
		case objRefDelta:
			if _, err := io.ReadFull(pr, baseID[:]); err != nil {
				return nil, codes.Broken("ref-delta base id was cut short")
			}
		default:
			return nil, codes.Broken("pack entry had an unknown object kind")
		}

		zr, err := zlib.NewReader(pr)
		if err != nil {
			return nil, codes.Broken("pack zlib stream could not be opened")
		}
		data, err := io.ReadAll(io.LimitReader(zr, int64(size)+1))
		closeErr := zr.Close()
		if err != nil || closeErr != nil || uint64(len(data)) != size {
			return nil, codes.Broken("pack zlib stream did not match its header size")
		}

		if typ == objOfsDelta || typ == objRefDelta {
			deltas = append(deltas, pendingDelta{
				kind:    typ,
				baseOff: baseOff,
				baseOID: baseID,
				off:     entryOff,
				delta:   data,
			})
			continue
		}
		obj := makeObj(typ, data)
		objs[obj.id] = obj
		byOff[entryOff] = obj.id
	}

	var wantSum [20]byte
	pr.h.Sum(wantSum[:0])
	var gotSum [20]byte
	if _, err := io.ReadFull(r, gotSum[:]); err != nil || wantSum != gotSum {
		return nil, codes.Broken("pack checksum did not match")
	}

	for len(deltas) > 0 {
		progressed := false
		rem := deltas[:0]
		for _, d := range deltas {
			baseID := d.baseOID
			if d.kind == objOfsDelta {
				var ok bool
				baseID, ok = byOff[d.baseOff]
				if !ok {
					rem = append(rem, d)
					continue
				}
			}
			baseObj, ok := objs[baseID]
			if !ok {
				rem = append(rem, d)
				continue
			}
			out, err := applyDelta(baseObj.data, d.delta)
			if err != nil {
				return nil, err
			}
			resolved := makeObj(baseObj.typ, out)
			objs[resolved.id] = resolved
			byOff[d.off] = resolved.id
			progressed = true
		}
		if !progressed {
			return nil, codes.Broken("pack delta named a missing base object")
		}
		deltas = rem
	}
	return objs, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	srcSize, n := readDeltaVarint(delta)
	if n == 0 || srcSize != uint64(len(base)) {
		return nil, codes.Broken("pack delta source size did not match base")
	}
	delta = delta[n:]
	dstSize, n := readDeltaVarint(delta)
	if n == 0 || dstSize > maxObject {
		return nil, codes.Broken("pack delta target size is out of range")
	}
	delta = delta[n:]

	out := make([]byte, 0, dstSize)
	for len(delta) > 0 {
		op := delta[0]
		delta = delta[1:]
		if op&0x80 != 0 {
			var cpOff, cpSize uint64
			for bit := uint(0); bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if len(delta) == 0 {
						return nil, codes.Broken("pack delta copy offset was cut short")
					}
					cpOff |= uint64(delta[0]) << (bit * 8)
					delta = delta[1:]
				}
			}
			for bit := uint(0); bit < 3; bit++ {
				if op&(1<<(4+bit)) != 0 {
					if len(delta) == 0 {
						return nil, codes.Broken("pack delta copy size was cut short")
					}
					cpSize |= uint64(delta[0]) << (bit * 8)
					delta = delta[1:]
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > uint64(len(base)) || uint64(len(out))+cpSize > dstSize {
				return nil, codes.Broken("pack delta copy exceeded bounds")
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
			continue
		}
		if op == 0 || int(op) > len(delta) || uint64(len(out))+uint64(op) > dstSize {
			return nil, codes.Broken("pack delta insert exceeded bounds")
		}
		out = append(out, delta[:op]...)
		delta = delta[op:]
	}
	if uint64(len(out)) != dstSize {
		return nil, codes.Broken("pack delta did not produce expected size")
	}
	return out, nil
}

func readDeltaVarint(b []byte) (uint64, int) {
	var val uint64
	var shift uint
	for i := 0; i < len(b); i++ {
		if shift > 60 {
			return 0, 0
		}
		val |= uint64(b[i]&0x7f) << shift
		if b[i]&0x80 == 0 {
			return val, i + 1
		}
		shift += 7
	}
	return 0, 0
}

func buildPack(objs []rawObj) ([]byte, error) {
	var buf bytes.Buffer
	var hdr [12]byte
	copy(hdr[:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(objs)))
	buf.Write(hdr[:])

	for _, obj := range objs {
		size := uint64(len(obj.data))
		first := byte((obj.typ&0x07)<<4) | byte(size&0x0f)
		size >>= 4
		for size > 0 {
			buf.WriteByte(first | 0x80)
			first = byte(size & 0x7f)
			size >>= 7
		}
		buf.WriteByte(first)

		zw := zlib.NewWriter(&buf)
		if _, err := zw.Write(obj.data); err != nil {
			return nil, codes.Broken("pack object could not be compressed")
		}
		if err := zw.Close(); err != nil {
			return nil, codes.Broken("pack object stream could not be closed")
		}
	}

	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes(), nil
}

type treeEntry struct {
	mode string
	name string
	id   oid
}

func parseCommitTree(data []byte) (oid, error) {
	nl := bytes.IndexByte(data, '\n')
	if nl < 0 {
		return zeroOID, codes.Broken("commit object had no header newline")
	}
	rest, ok := strings.CutPrefix(string(data[:nl]), "tree ")
	if !ok {
		return zeroOID, codes.Broken("commit object did not start with tree")
	}
	id, ok := parseOID(rest)
	if !ok {
		return zeroOID, codes.Broken("commit tree id was malformed")
	}
	return id, nil
}

func parseTreeEntries(data []byte) ([]treeEntry, error) {
	var entries []treeEntry
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp < 1 {
			return nil, codes.Broken("tree entry had no mode")
		}
		mode := string(data[:sp])
		data = data[sp+1:]
		nul := bytes.IndexByte(data, 0)
		if nul < 1 || len(data) < nul+21 {
			return nil, codes.Broken("tree entry was cut short")
		}
		name := string(data[:nul])
		if strings.Contains(name, "/") {
			return nil, codes.Broken("tree entry name contained a slash")
		}
		var id oid
		copy(id[:], data[nul+1:nul+21])
		data = data[nul+21:]
		switch mode {
		case "40000", "100644", "100755", "120000", "160000":
		default:
			return nil, codes.Broken("tree entry had an unknown mode")
		}
		entries = append(entries, treeEntry{mode: mode, name: name, id: id})
	}
	return entries, nil
}

func encodeTree(entries []treeEntry) []byte {
	sorted := append([]treeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i]) < treeSortKey(sorted[j])
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		buf.Write(e.id[:])
	}
	return buf.Bytes()
}

func treeSortKey(e treeEntry) string {
	if e.mode == "40000" {
		return e.name + "/"
	}
	return e.name
}

func lookupBlobInTree(objs map[oid]rawObj, rootTree oid, path string) (oid, bool, error) {
	parts := strings.Split(path, "/")
	cur := rootTree
	for i, seg := range parts {
		treeObj, ok := objs[cur]
		if !ok || treeObj.typ != objTree {
			return zeroOID, false, codes.Broken("tree object was missing from pack")
		}
		entries, err := parseTreeEntries(treeObj.data)
		if err != nil {
			return zeroOID, false, err
		}
		found := false
		for _, e := range entries {
			if e.name != seg {
				continue
			}
			if i == len(parts)-1 {
				if e.mode == "40000" || e.mode == "160000" {
					return zeroOID, false, nil
				}
				return e.id, true, nil
			}
			if e.mode != "40000" {
				return zeroOID, false, nil
			}
			cur = e.id
			found = true
			break
		}
		if !found {
			return zeroOID, false, nil
		}
	}
	return zeroOID, false, nil
}

func upsertPathInTree(objs map[oid]rawObj, rootTree oid, hasRoot bool, path string, blob rawObj) (oid, []rawObj, error) {
	parts := strings.Split(path, "/")
	seen := map[oid]bool{blob.id: true}
	out := []rawObj{blob}

	var walk func(treeID oid, present bool, depth int) (oid, error)
	walk = func(treeID oid, present bool, depth int) (oid, error) {
		var entries []treeEntry
		if present {
			treeObj, ok := objs[treeID]
			if !ok || treeObj.typ != objTree {
				return zeroOID, codes.Broken("tree object was missing from pack")
			}
			var err error
			entries, err = parseTreeEntries(treeObj.data)
			if err != nil {
				return zeroOID, err
			}
		}

		seg := parts[depth]
		idx := -1
		for i, e := range entries {
			if e.name == seg {
				idx = i
				break
			}
		}

		if depth == len(parts)-1 {
			mode := "100644"
			if idx >= 0 {
				if entries[idx].mode == "40000" {
					return zeroOID, codes.Broken("proposal file path names a directory")
				}
				if entries[idx].mode == "100755" {
					mode = "100755"
				}
				entries[idx] = treeEntry{mode: mode, name: seg, id: blob.id}
			} else {
				entries = append(entries, treeEntry{mode: mode, name: seg, id: blob.id})
			}
		} else {
			var subID oid
			subPresent := false
			if idx >= 0 {
				if entries[idx].mode != "40000" {
					return zeroOID, codes.Broken("proposal file path crosses an existing file")
				}
				subID = entries[idx].id
				subPresent = true
			}
			newSub, err := walk(subID, subPresent, depth+1)
			if err != nil {
				return zeroOID, err
			}
			if idx >= 0 {
				entries[idx] = treeEntry{mode: "40000", name: seg, id: newSub}
			} else {
				entries = append(entries, treeEntry{mode: "40000", name: seg, id: newSub})
			}
		}

		newTreeObj := makeObj(objTree, encodeTree(entries))
		if !seen[newTreeObj.id] {
			seen[newTreeObj.id] = true
			out = append(out, newTreeObj)
		}
		return newTreeObj.id, nil
	}

	newRoot, err := walk(rootTree, hasRoot, 0)
	if err != nil {
		return zeroOID, nil, err
	}
	return newRoot, out, nil
}

func encodeCommit(tree oid, parents []oid, message string, when time.Time) []byte {
	stamp := strconv.FormatInt(when.UTC().Unix(), 10) + " +0000"
	var buf bytes.Buffer
	buf.WriteString("tree " + tree.String() + "\n")
	for _, p := range parents {
		buf.WriteString("parent " + p.String() + "\n")
	}
	buf.WriteString("author " + gitIdentity + " " + stamp + "\n")
	buf.WriteString("committer " + gitIdentity + " " + stamp + "\n\n")
	buf.WriteString(strings.TrimRight(message, "\n") + "\n")
	return buf.Bytes()
}
