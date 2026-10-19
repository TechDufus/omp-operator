/*
Copyright 2026 TechDufus.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cloudapi

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/TechDufus/omp-operator/api/v1alpha1"
	"github.com/TechDufus/omp-operator/internal/agentapi"
	"github.com/TechDufus/omp-operator/internal/agentspec"
	"github.com/TechDufus/omp-operator/internal/cloudspec"
	"github.com/TechDufus/omp-operator/internal/dashboard"
	"github.com/TechDufus/omp-operator/internal/store"
	"github.com/TechDufus/omp-operator/internal/threads"
)

const (
	// DefaultMaxUploadBytes is the repository bundle limit when Server.MaxUploadBytes is unset.
	DefaultMaxUploadBytes int64 = 1 << 30

	// multipartOverhead is what the request body may exceed the largest bundle by: the
	// request part (at most maxRequestPartBytes), the conversation and the setup tarball
	// are counted against their own limits, so the bound only needs headroom for framing.
	multipartOverhead   = store.MaxContinuityBytes + store.MaxSetupBytes + maxRequestPartBytes + 1<<20
	maxRequestPartBytes = 2 << 20

	maxPromptBytes = threads.MaxTextBytes
	maxTitleRunes  = 80

	maxSecretFiles = 32
	maxSecretEnv   = 64
	// maxSecretBytes bounds the Secret <thread>-secrets: the Kubernetes limit on keys plus
	// values, which the shipped files and variables must fit in.
	maxSecretBytes = 1 << 20

	maxFieldBytes = 512
	maxURLBytes   = 2048

	maxCreateAttempts = 5
	nameSuffixLen     = 6
	nameAlphabet      = "abcdefghijklmnopqrstuvwxyz0123456789"

	partRequest      = "request"
	partSource       = "source"
	partConversation = "conversation"
	partSetup        = "setup"

	fileSource = "source.bundle"
	fileSetup  = "setup.tar.gz"

	// cleanupTimeout bounds removing a half-created thread.
	cleanupTimeout = 30 * time.Second
)

var (
	commitPattern           = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	conversationFilePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+\.jsonl$`)

	thinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
)

// CreateRequest is the JSON of the "request" part of POST …/threads.
type CreateRequest struct {
	Agent    string `json:"agent"`
	Prompt   string `json:"prompt"`
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
	Repo     struct {
		Name          string `json:"name"`
		RemoteURL     string `json:"remoteURL"`
		BaseRef       string `json:"baseRef"`
		BaseCommit    string `json:"baseCommit"`
		HandoffCommit string `json:"handoffCommit"`
		LocalBranch   string `json:"localBranch"`
	} `json:"repo"`
	Paths struct {
		Home      string `json:"home"`
		Workspace string `json:"workspace"`
	} `json:"paths"`
	Author struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"author"`
	SecretFiles      []SecretFile      `json:"secretFiles"`
	SecretEnv        map[string]string `json:"secretEnv"`
	ConversationFile string            `json:"conversationFile"`
}

// SecretFile is a workspace file shipped as a secret; Content is base64.
type SecretFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// newThread is a validated request: the thread to create and the secret data to store.
type newThread struct {
	spec     v1alpha1.OmpThreadSpec
	prompt   string
	warnings []string
	// secrets is the data of Secret <thread>-secrets: file-<i> and env.<NAME>.
	secrets map[string][]byte
}

func (s *Server) maxUpload() int64 {
	if s.MaxUploadBytes > 0 {
		return s.MaxUploadBytes
	}
	return DefaultMaxUploadBytes
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	if err := s.authorize(r, ns, dashboard.RoleOperator); err != nil {
		writeError(w, err)
		return
	}
	t, nt, err := s.create(w, r, ns)
	if err != nil {
		writeError(w, err)
		return
	}
	sum := s.summary(t, nil)
	sum.Phase, sum.Turns, sum.Queued, sum.Warnings = v1alpha1.ThreadPhaseWorking, 1, 1, nt.warnings
	writeJSON(w, http.StatusCreated, sum)
}

// create reads the upload and creates the thread, its files, its secret and its first turn.
func (s *Server) create(w http.ResponseWriter, r *http.Request, ns string) (*v1alpha1.OmpThread, *newThread, error) {
	limit := s.maxUpload() + multipartOverhead
	if r.ContentLength > limit {
		return nil, nil, fail(http.StatusRequestEntityTooLarge, "upload is too large")
	}
	need := uint64(max(r.ContentLength, 0))
	if r.ContentLength < 0 {
		need = uint64(limit)
	}
	if free, _, err := s.Store.Statfs(); err == nil && free < 2*need {
		return nil, nil, fail(http.StatusInsufficientStorage, "insufficient storage on the operator data volume")
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, fail(http.StatusBadRequest, "expected a multipart/form-data body")
	}
	first, err := mr.NextPart()
	if err != nil || first.FormName() != partRequest {
		return nil, nil, fail(http.StatusBadRequest, `the first part must be "request"`)
	}
	var req CreateRequest
	if err := decodePart(first, maxRequestPartBytes, &req); err != nil {
		return nil, nil, err
	}
	agent, err := s.cloudAgent(r.Context(), ns, req.Agent)
	if err != nil {
		return nil, nil, err
	}
	nt, err := s.validate(&req, agent)
	if err != nil {
		return nil, nil, err
	}
	nt.spec.Owner = principalOf(r)

	t, err := s.createObject(r.Context(), agent, nt)
	if err != nil {
		return nil, nil, err
	}
	if err := s.fill(r.Context(), mr, t, &req, nt); err != nil {
		s.discard(r.Context(), t)
		return nil, nil, err
	}
	return t, nt, nil
}

// fill stores the parts of the upload, the secrets and the first turn of a created thread.
func (s *Server) fill(ctx context.Context, mr *multipart.Reader, t *v1alpha1.OmpThread, req *CreateRequest, nt *newThread) error {
	seen, err := s.storeParts(mr, t, &req.ConversationFile)
	if err != nil {
		return err
	}
	repo := &nt.spec.Repo
	switch {
	case repo.BaseCommit == "" && !seen[partSource]:
		return fail(http.StatusBadRequest, "a full-history handoff needs the source part")
	case repo.BaseCommit != "" && repo.HandoffCommit != repo.BaseCommit && !seen[partSource]:
		return fail(http.StatusBadRequest, "the source part is required when the handoff has commits beyond the base")
	case req.ConversationFile != "" && !seen[partConversation]:
		return fail(http.StatusBadRequest, "conversationFile is set but there is no conversation part")
	}
	if len(nt.secrets) > 0 {
		if err := s.createSecret(ctx, t, nt.secrets); err != nil {
			return err
		}
	}
	if _, err := s.Threads.Enqueue(t.Namespace, t.Name, nt.prompt, nt.spec.Owner); err != nil {
		return err
	}
	s.Threads.Signal(t.Namespace, t.Name)
	return nil
}

// storeParts streams the parts after "request" into the thread's files.
func (s *Server) storeParts(mr *multipart.Reader, t *v1alpha1.OmpThread, conversationFile *string) (map[string]bool, error) {
	seen := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return seen, nil
		}
		if err != nil {
			return nil, uploadError("upload", err)
		}
		name := part.FormName()
		if seen[name] {
			return nil, fail(http.StatusBadRequest, fmt.Sprintf("part %q is repeated", name))
		}
		seen[name] = true
		switch name {
		case partSource:
			err = s.Store.PutThreadFile(t.Namespace, t.Name, fileSource, part, s.maxUpload())
		case partSetup:
			err = s.Store.PutThreadFile(t.Namespace, t.Name, fileSetup, part, store.MaxSetupBytes)
		case partConversation:
			if *conversationFile == "" {
				return nil, fail(http.StatusBadRequest, "a conversation part needs conversationFile in the request")
			}
			err = s.putConversation(t, *conversationFile, part)
		default:
			return nil, fail(http.StatusBadRequest, fmt.Sprintf("unknown part %q", name))
		}
		if err != nil {
			return nil, uploadError(name, err)
		}
	}
}

// uploadError maps a failure while storing an upload part to a response.
func uploadError(part string, err error) error {
	if ae, ok := errors.AsType[*apiError](err); ok {
		return ae
	}
	_, tooLarge := errors.AsType[*http.MaxBytesError](err)
	switch {
	case errors.Is(err, store.ErrTooLarge), tooLarge:
		return fail(http.StatusRequestEntityTooLarge, fmt.Sprintf("part %q is too large", part))
	case errors.Is(err, io.ErrUnexpectedEOF), strings.Contains(err.Error(), "multipart"):
		return fail(http.StatusBadRequest, "the upload ended early or is malformed")
	}
	return err
}

// putConversation stores the conversation as the thread's continuity tarball:
// one entry named file, which the first sitting restores into omp's session directory.
func (s *Server) putConversation(t *v1alpha1.OmpThread, file string, part io.Reader) error {
	tmp, err := os.CreateTemp("", "omp-conversation-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()); _ = tmp.Close() }()
	n, err := io.Copy(tmp, io.LimitReader(part, store.MaxContinuityBytes+1))
	if err != nil {
		return err
	}
	if n > store.MaxContinuityBytes {
		return store.ErrTooLarge
	}
	if n == 0 {
		return fail(http.StatusBadRequest, "the conversation part is empty")
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTarGz(pw, file, tmp, n)) }()
	defer func() { _ = pr.Close() }()
	return s.Store.PutContinuity(t.Namespace, t.Spec.AgentRef.Name, store.ContinuityID(cloudspec.ContinuityKey(t.Namespace, t.Name)), pr)
}

func writeTarGz(w io.Writer, name string, body io.Reader, size int64) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := io.Copy(tw, body); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func decodePart(part io.Reader, limit int64, v any) error {
	raw, err := io.ReadAll(io.LimitReader(part, limit+1))
	if err != nil {
		return uploadError(partRequest, err)
	}
	if int64(len(raw)) > limit {
		return fail(http.StatusRequestEntityTooLarge, "the request part is too large")
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fail(http.StatusBadRequest, "the request part is not valid JSON")
	}
	return nil
}

// cloudAgent loads the agent a thread runs on and checks it can take threads.
func (s *Server) cloudAgent(ctx context.Context, ns, name string) (*v1alpha1.OmpAgent, error) {
	if name == "" {
		return nil, fail(http.StatusBadRequest, "agent is required")
	}
	var a v1alpha1.OmpAgent
	if err := s.reader().Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &a); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fail(http.StatusNotFound, fmt.Sprintf("agent %s not found", name))
		}
		return nil, err
	}
	if a.Spec.Cloud == nil {
		return nil, fail(http.StatusNotFound, fmt.Sprintf("agent %s has no cloud configuration", name))
	}
	if !meta.IsStatusConditionTrue(a.Status.Conditions, v1alpha1.ConditionReady) {
		reason := "no status yet"
		if c := meta.FindStatusCondition(a.Status.Conditions, v1alpha1.ConditionReady); c != nil && c.Reason != "" {
			reason = c.Reason
		}
		return nil, fail(http.StatusConflict, fmt.Sprintf("agent %s is not ready: %s", name, reason))
	}
	return &a, nil
}

// validate checks the request against the agent and builds the thread spec.
func (s *Server) validate(req *CreateRequest, agent *v1alpha1.OmpAgent) (*newThread, error) {
	bad := func(format string, a ...any) (*newThread, error) {
		return nil, fail(http.StatusBadRequest, fmt.Sprintf(format, a...))
	}
	if n := len(req.Prompt); n == 0 || n > maxPromptBytes || !utf8.ValidString(req.Prompt) {
		return bad("prompt must be 1 to %d bytes of UTF-8", maxPromptBytes)
	}
	repo := &req.Repo
	if !commitPattern.MatchString(repo.HandoffCommit) {
		return bad("repo.handoffCommit must be a 40 or 64 character hex commit id")
	}
	if repo.BaseCommit != "" && !commitPattern.MatchString(repo.BaseCommit) {
		return bad("repo.baseCommit must be empty or a 40 or 64 character hex commit id")
	}
	for _, f := range []struct{ field, value string }{
		{"repo.name", repo.Name}, {"repo.baseRef", repo.BaseRef}, {"repo.localBranch", repo.LocalBranch},
		{"author.name", req.Author.Name}, {"author.email", req.Author.Email},
	} {
		if !plainText(f.value, maxFieldBytes) {
			return bad("%s must be at most %d bytes without control characters", f.field, maxFieldBytes)
		}
	}
	if repo.Name == "" {
		return bad("repo.name is required")
	}
	remote, err := stripUserinfo(repo.RemoteURL)
	if err != nil {
		return bad("repo.remoteURL is not a valid git URL")
	}
	if repo.BaseCommit != "" {
		if remote == "" {
			return bad("a delta handoff (repo.baseCommit) needs repo.remoteURL")
		}
		if repo.BaseRef == "" {
			return bad("a delta handoff (repo.baseCommit) needs repo.baseRef")
		}
		if _, fetch, _ := cloudspec.MatchGitHost(agent.Spec.Cloud.Git, remote); fetch == "" {
			return bad("the remote of %s is not served by agent %s; hand off the whole repository instead", repo.Name, agent.Name)
		}
	}
	if !plainText(remote, maxURLBytes) {
		return bad("repo.remoteURL must be at most %d bytes without control characters", maxURLBytes)
	}
	if req.Thinking != "" && !slices.Contains(thinkingLevels, req.Thinking) {
		return bad("thinking must be one of %s", strings.Join(thinkingLevels, ", "))
	}
	if req.ConversationFile != "" && !conversationFilePattern.MatchString(req.ConversationFile) {
		return bad("conversationFile must match %s", conversationFilePattern)
	}
	secrets, files, envs, err := secretData(req)
	if err != nil {
		return nil, err
	}

	nt := &newThread{prompt: req.Prompt, secrets: secrets}
	nt.spec = v1alpha1.OmpThreadSpec{
		AgentRef: v1alpha1.LocalObjectRef{Name: agent.Name},
		Title:    titleOf(req.Prompt),
		Thinking: req.Thinking,
		Repo: v1alpha1.ThreadRepo{
			Name: repo.Name, RemoteURL: remote, BaseRef: repo.BaseRef, BaseCommit: repo.BaseCommit,
			HandoffCommit: repo.HandoffCommit, LocalBranch: repo.LocalBranch,
		},
		Author:      v1alpha1.GitAuthor{Name: req.Author.Name, Email: req.Author.Email},
		SecretFiles: files,
		SecretEnv:   envs,
	}
	nt.spec.Model, nt.warnings = pickModel(agent, req.Model)
	nt.spec.Paths, nt.warnings = pickPaths(req.Paths.Home, req.Paths.Workspace, nt.warnings)
	return nt, nil
}

// pickModel keeps the requested model when the agent allows it, else falls back to the agent's.
func pickModel(agent *v1alpha1.OmpAgent, requested string) (string, []string) {
	if requested == "" || modelAllowed(modelPatterns(agent), requested) {
		return requested, nil
	}
	return agent.Spec.Model.Default, []string{fmt.Sprintf("model %s is not available on %s; using %s", requested, agent.Name, agent.Spec.Model.Default)}
}

// pickPaths keeps the requested home and workspace paths when the pod can mirror them, else stores none.
func pickPaths(home, workspace string, warnings []string) (v1alpha1.ThreadPaths, []string) {
	if home == "" && workspace == "" {
		return v1alpha1.ThreadPaths{}, warnings
	}
	req := v1alpha1.ThreadPaths{Home: home, Workspace: workspace}
	if h, w := cloudspec.PodPaths(req); h == home && w == workspace {
		return req, warnings
	}
	return v1alpha1.ThreadPaths{}, append(warnings, fmt.Sprintf("paths home %q and workspace %q cannot be mirrored; the cloud agent uses %s and %s",
		cut(home, maxFieldBytes), cut(workspace, maxFieldBytes), cloudspec.DefaultHome, cloudspec.DefaultWorkspace))
}

// secretData validates the shipped files and variables and returns the Secret data with the
// path and name lists, in the order of the keys file-<i> and env.<NAME>.
func secretData(req *CreateRequest) (data map[string][]byte, files, envs []string, err error) {
	bad := func(format string, a ...any) error { return fail(http.StatusBadRequest, fmt.Sprintf(format, a...)) }
	if len(req.SecretFiles) > maxSecretFiles {
		return nil, nil, nil, bad("secretFiles has %d entries; at most %d", len(req.SecretFiles), maxSecretFiles)
	}
	if len(req.SecretEnv) > maxSecretEnv {
		return nil, nil, nil, bad("secretEnv has %d entries; at most %d", len(req.SecretEnv), maxSecretEnv)
	}
	data = map[string][]byte{}
	total := 0
	for i, f := range req.SecretFiles {
		if !validSecretPath(f.Path) {
			return nil, nil, nil, bad("secretFiles[%d]: path %q must be relative, clean and outside .git", i, cut(f.Path, 100))
		}
		if slices.Contains(files, f.Path) {
			return nil, nil, nil, bad("secretFiles[%d]: path %q is listed twice", i, f.Path)
		}
		content, err := base64.StdEncoding.DecodeString(f.Content)
		if err != nil {
			return nil, nil, nil, bad("secretFiles[%d]: content is not base64", i)
		}
		key := agentapi.ThreadSecretFileKey(i)
		data[key] = content
		files = append(files, f.Path)
		total += len(key) + len(content)
	}
	for _, name := range sortedKeys(req.SecretEnv) {
		if msg := agentspec.CheckEnvName(name); msg != "" {
			return nil, nil, nil, bad("secretEnv %s %s", cut(name, 100), msg)
		}
		key := agentapi.ThreadSecretEnvKey(name)
		data[key] = []byte(req.SecretEnv[name])
		envs = append(envs, name)
		total += len(key) + len(req.SecretEnv[name])
	}
	if total > maxSecretBytes {
		return nil, nil, nil, bad("secretFiles and secretEnv are %d bytes; at most %d", total, maxSecretBytes)
	}
	if len(data) == 0 {
		data = nil
	}
	return data, files, envs, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// validSecretPath accepts a workspace-relative, clean path that is not inside .git.
func validSecretPath(p string) bool {
	if p == "" || len(p) > maxFieldBytes || !plainText(p, maxFieldBytes) || path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	return p != "." && p != ".." && !strings.HasPrefix(p, "../") && p != ".git" && !strings.HasPrefix(p, ".git/")
}

// plainText reports whether s is at most limit bytes of valid UTF-8 without control characters.
func plainText(s string, limit int) bool {
	if len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// titleOf is the first non-empty line of prompt, cut to 80 characters.
func titleOf(prompt string) string {
	for line := range strings.SplitSeq(prompt, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if utf8.RuneCountInString(line) > maxTitleRunes {
				return string([]rune(line)[:maxTitleRunes])
			}
			return line
		}
	}
	return ""
}

// stripUserinfo drops credentials from a git remote URL before it is stored: the userinfo of a URL, or
// the "user@" of scp-like syntax.
func stripUserinfo(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return "", errors.New("invalid URL")
		}
		u.User = nil
		return u.String(), nil
	}
	if host, rest, ok := strings.Cut(raw, ":"); ok {
		if _, bare, found := strings.CutLast(host, "@"); found {
			return bare + ":" + rest, nil
		}
	}
	return raw, nil
}

// ---- objects ----

// createObject creates the OmpThread, retrying a few random names on collision.
func (s *Server) createObject(ctx context.Context, agent *v1alpha1.OmpAgent, nt *newThread) (*v1alpha1.OmpThread, error) {
	for range maxCreateAttempts {
		suffix, err := randomSuffix()
		if err != nil {
			return nil, err
		}
		t := &v1alpha1.OmpThread{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:  agent.Namespace,
				Name:       agent.Name + "-" + suffix,
				Labels:     map[string]string{v1alpha1.LabelAgent: agent.Name},
				Finalizers: []string{v1alpha1.FinalizerCleanup},
			},
			Spec: nt.spec,
		}
		if err := controllerutil.SetOwnerReference(agent, t, s.Client.Scheme()); err != nil {
			return nil, err
		}
		err = s.Client.Create(ctx, t)
		if apierrors.IsAlreadyExists(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return t, nil
	}
	return nil, errors.New("could not find a free thread name")
}

func randomSuffix() (string, error) {
	b := make([]byte, nameSuffixLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = nameAlphabet[int(b[i])%len(nameAlphabet)]
	}
	return string(b), nil
}

func (s *Server) createSecret(ctx context.Context, t *v1alpha1.OmpThread, data map[string][]byte) error {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: t.Namespace, Name: agentapi.ThreadSecretsName(t.Name),
			Labels: map[string]string{v1alpha1.LabelManagedBy: v1alpha1.ManagedByValue, v1alpha1.LabelAgent: t.Spec.AgentRef.Name},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if err := controllerutil.SetControllerReference(t, sec, s.Client.Scheme()); err != nil {
		return err
	}
	return s.Client.Create(ctx, sec)
}

// discard removes a thread whose creation failed, with everything stored for it. The thread
// reconciler has seen no turn of it, so the files and the finalizer are ours to remove.
func (s *Server) discard(ctx context.Context, t *v1alpha1.OmpThread) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := s.Store.RemoveThread(t.Namespace, t.Name); err != nil {
		log.Error(err, "remove files of a failed thread", "thread", t.Name)
	}
	if err := s.Store.RemoveContinuity(t.Namespace, t.Spec.AgentRef.Name, store.ContinuityID(cloudspec.ContinuityKey(t.Namespace, t.Name))); err != nil {
		log.Error(err, "remove conversation of a failed thread", "thread", t.Name)
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: t.Namespace, Name: agentapi.ThreadSecretsName(t.Name)}}
	if err := s.Client.Delete(ctx, sec); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "delete secret of a failed thread", "thread", t.Name)
	}
	if err := s.Client.Delete(ctx, t); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "delete a failed thread", "thread", t.Name)
		return
	}
	base := t.DeepCopy()
	t.Finalizers = nil
	if err := s.Client.Patch(ctx, t, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		log.Error(err, "release a failed thread", "thread", t.Name)
	}
}
