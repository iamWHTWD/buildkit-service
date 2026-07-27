package main

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/inclusionAI/buildkit-service/pkg/dockerfilepreprocess"

	containerddocker "github.com/containerd/containerd/v2/core/remotes/docker"
	containerderrdefs "github.com/containerd/errdefs"
	"github.com/distribution/reference"
	dockerconfig "github.com/docker/cli/cli/config"
	buildkit "github.com/moby/buildkit/client"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	digest "github.com/opencontainers/go-digest"

	"github.com/labstack/echo/v4"
	cli "github.com/urfave/cli/v2"
)

const (
	defaultListenAddr        = ":8080"
	defaultBuildkitdAddr     = "tcp://buildkit-service.buildkit-service.svc:9094"
	defaultWorkDir           = "/tmp/buildctl-daemon"
	defaultMaxLogBytes       = int64(1 << 20)
	defaultMaxRequestBytes   = int64(512 << 20)
	defaultMaxExtractedBytes = int64(4 << 30)
	defaultMaxArchiveFiles   = 100000
	defaultMaxRetainedTasks  = 100
	defaultMaxWorkDirBytes   = int64(8 << 30)
	defaultUploadReadTimeout = 5 * time.Minute
	defaultAddrConcurrency   = 5
	defaultAddrRefresh       = 15 * time.Second
	defaultKeepTTL           = 2 * time.Hour
	defaultCleanupInterval   = time.Minute
	defaultBuildRetry        = 3
	defaultRetryInterval     = 10 * time.Second
	defaultRegistryTimeout   = 30 * time.Second
	defaultRegistryChecks    = 32
	nydusV3TargetSuffix      = "_nydus_v3"
	buildStatusQueued        = "queued"
	buildStatusRunning       = "running"
	buildStatusSucceeded     = "succeeded"
	buildStatusFailed        = "failed"
	buildStatusCanceled      = "canceled"
	imageTypeNydus           = "nydus"
	imageTypeOCI             = "oci"
	imageTypeBoth            = "both"
	exporterImageDigestKey   = "containerimage.digest"
	buildkitDockerFrontend   = "dockerfile.v0"
	zipMaxExtractedFileMode  = 0o755
)

type config struct {
	Listen             string
	BuildkitdAddrs     string
	AuthToken          string
	AuthTokenFile      string
	WorkDir            string
	DefaultMode        string
	ModesJSON          string
	RoutingTargetsJSON string
	KeepTTL            time.Duration
	PprofListen        string
	MaxLogBytes        int64
	MaxRequestBytes    int64
	MaxExtractedBytes  int64
	MaxArchiveFiles    int
	MaxRetainedTasks   int
	MaxWorkDirBytes    int64
	UploadReadTimeout  time.Duration
	AddrConcurrency    int
	MaxConcurrency     int
	RLimitNoFile       uint64
	RegistryTimeout    time.Duration
	RegistryRules      string
	RegistryChecks     int
	TLS                tlsConfig
}

type tlsConfig struct {
	CACert     string
	Cert       string
	Key        string
	Dir        string
	ServerName string
}

type buildServer struct {
	cfg       config
	store     *taskStore
	pool      *addrPool
	runner    buildRunner
	images    imageChecker
	modes     *buildModes
	scheduler *buildModeScheduler
	router    targetRouter
	metrics   daemonMetrics
	// globalSem caps builds running concurrently across all buildkitd
	// addresses. nil disables the global limit (per-address concurrency
	// still applies). Tasks over the limit stay queued until a slot frees.
	globalSem chan struct{}
	// admissionMu serializes upload/extraction accounting against work-dir.
	admissionMu sync.Mutex
}

// daemonMetrics holds monotonic build outcome counters. The running gauge is
// derived from the task store at scrape time, so it stays accurate even after
// terminal tasks are reaped.
type daemonMetrics struct {
	succeeded int64
	failed    int64
}

type buildRunner interface {
	Build(ctx context.Context, req buildRunRequest, log io.Writer) (map[string]string, error)
	Close() error
}

type imageChecker interface {
	Check(ctx context.Context, image string) (imageCheckResult, error)
}

type imageCheckResult struct {
	Digest    string
	MediaType string
	Size      int64
}

type registryImageChecker struct {
	timeout   time.Duration
	configDir string
	hostRules map[string]map[string]struct{}
	slots     chan struct{}
	transport http.RoundTripper
}

type buildRunRequest struct {
	ContextDir   string
	BuildkitAddr string
	Image        string
	Format       string
	Target       string
	NoCache      bool
	BuildArgs    map[string]string
}

type buildTask struct {
	ID               string            `json:"id"`
	Status           string            `json:"status"`
	Image            string            `json:"image"`
	Mode             string            `json:"mode"`
	RoutedTarget     string            `json:"routed_target"`
	ImageType        string            `json:"image_type"`
	Target           string            `json:"target,omitempty"`
	BuildkitAddr     string            `json:"buildkitd_addr,omitempty"`
	NodeIP           string            `json:"node_ip,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	StartedAt        *time.Time        `json:"started_at,omitempty"`
	FinishedAt       *time.Time        `json:"finished_at,omitempty"`
	Error            string            `json:"error,omitempty"`
	LogPath          string            `json:"log_path,omitempty"`
	WorkDir          string            `json:"work_dir,omitempty"`
	ExporterResponse map[string]string `json:"exporter_response,omitempty"`

	contextDir    string
	scheduleKey   string
	routingDigest string
	noCache       bool
	retry         int
	retryInterval time.Duration
	buildArgs     map[string]string
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
}

type buildTaskView struct {
	ID                   string     `json:"id"`
	Status               string     `json:"status"`
	Image                string     `json:"image"`
	Mode                 string     `json:"mode"`
	RoutedTarget         string     `json:"routed_target"`
	ImageType            string     `json:"image_type"`
	Target               string     `json:"target,omitempty"`
	BuildkitAddr         string     `json:"buildkitd_addr,omitempty"`
	NodeIP               string     `json:"node_ip,omitempty"`
	Retry                int        `json:"retry"`
	RetryIntervalSeconds int        `json:"retry_interval_seconds"`
	CreatedAt            time.Time  `json:"created_at"`
	StartedAt            *time.Time `json:"started_at,omitempty"`
	FinishedAt           *time.Time `json:"finished_at,omitempty"`
	Error                string     `json:"error,omitempty"`
}

type imageMetadata struct {
	Target string `json:"target"`
}

type taskStore struct {
	mu    sync.RWMutex
	tasks map[string]*buildTask
}

func newTaskStore() *taskStore {
	return &taskStore{tasks: make(map[string]*buildTask)}
}

func (s *taskStore) add(task *buildTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[task.ID] = task
}

func (s *taskStore) len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tasks)
}

func (s *taskStore) get(id string) (*buildTask, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	return cloneTask(task), true
}

func (s *taskStore) countByStatus(status string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, task := range s.tasks {
		if task.Status == status {
			n++
		}
	}
	return n
}

func (s *taskStore) countByModeAndStatus(mode, status string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, task := range s.tasks {
		if task.Mode == mode && task.Status == status {
			n++
		}
	}
	return n
}

func (s *taskStore) list() []*buildTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*buildTask, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, cloneTask(task))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

func (s *taskStore) update(id string, fn func(*buildTask)) (*buildTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	fn(task)
	return cloneTask(task), true
}

func (s *taskStore) cancel(id string) (*buildTask, bool) {
	var cancel context.CancelFunc
	snapshot, ok := s.update(id, func(task *buildTask) {
		if task.Status != buildStatusQueued && task.Status != buildStatusRunning {
			return
		}
		now := time.Now()
		task.Status = buildStatusCanceled
		task.FinishedAt = &now
		cancel = task.cancel
	})
	if cancel != nil {
		cancel()
	}
	return snapshot, ok
}

func (s *taskStore) delete(id string) (*buildTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.tasks[id]
	if !ok {
		return nil, false
	}
	snapshot := cloneTask(task)
	delete(s.tasks, id)
	return snapshot, true
}

func (s *taskStore) deleteExpired(now time.Time, keepTTL time.Duration) []*buildTask {
	s.mu.Lock()
	defer s.mu.Unlock()

	var expired []*buildTask
	for id, task := range s.tasks {
		if task == nil || task.FinishedAt == nil {
			continue
		}
		if now.Before(task.FinishedAt.Add(keepTTL)) {
			continue
		}
		expired = append(expired, cloneTask(task))
		delete(s.tasks, id)
	}
	return expired
}

func cloneTask(task *buildTask) *buildTask {
	clone := *task
	clone.buildArgs = cloneStringMap(task.buildArgs)
	clone.ExporterResponse = cloneStringMap(task.ExporterResponse)
	return &clone
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func directorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func viewTask(task *buildTask) buildTaskView {
	if task == nil {
		return buildTaskView{}
	}
	return buildTaskView{
		ID:                   task.ID,
		Status:               task.Status,
		Image:                task.Image,
		Mode:                 task.Mode,
		RoutedTarget:         task.RoutedTarget,
		ImageType:            task.ImageType,
		Target:               task.Target,
		BuildkitAddr:         task.BuildkitAddr,
		NodeIP:               task.NodeIP,
		Retry:                task.retry,
		RetryIntervalSeconds: int(task.retryInterval / time.Second),
		CreatedAt:            task.CreatedAt,
		StartedAt:            task.StartedAt,
		FinishedAt:           task.FinishedAt,
		Error:                task.Error,
	}
}

func viewTasks(tasks []*buildTask) []buildTaskView {
	views := make([]buildTaskView, 0, len(tasks))
	for _, task := range tasks {
		views = append(views, viewTask(task))
	}
	return views
}

func main() {
	app := &cli.App{
		Name:  "buildctl-daemon",
		Usage: "HTTP API daemon for direct BuildKit image builds",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "listen", Value: defaultListenAddr, Usage: "HTTP listen address"},
			&cli.StringFlag{Name: "buildkitd-addr", Value: defaultBuildkitdAddr, Usage: "comma-separated buildkitd addresses"},
			&cli.StringFlag{Name: "auth-token", Usage: "Bearer token required for /v1 APIs; empty disables auth"},
			&cli.StringFlag{Name: "auth-token-file", Usage: "read the Bearer token from a file; mutually exclusive with --auth-token"},
			&cli.StringFlag{Name: "work-dir", Value: defaultWorkDir, Usage: "directory for uploaded archives, extracted contexts, and logs"},
			&cli.StringFlag{Name: "default-mode", Value: defaultBuildMode, Usage: "build mode used when requests omit the mode form field"},
			&cli.StringFlag{Name: "modes-json", Usage: "JSON object mapping build mode names to concurrency and routingEnabled settings; empty preserves legacy scheduling"},
			&cli.StringFlag{Name: "routing-targets-json", Usage: "JSON array of 2-32 registry prefixes available to routing-enabled build modes"},
			&cli.DurationFlag{Name: "keep-ttl", Value: defaultKeepTTL, Usage: "duration to keep finished task status and logs before deleting them from memory and disk"},
			&cli.StringFlag{Name: "pprof-listen", Usage: "optional pprof listen address, for example 0.0.0.0:6060", EnvVars: []string{"BUILDCTL_DAEMON_PPROF_SERVER"}},
			&cli.Int64Flag{Name: "max-log-bytes", Value: defaultMaxLogBytes, Usage: "maximum bytes stored per build log and returned by status APIs"},
			&cli.Int64Flag{Name: "max-request-bytes", Value: defaultMaxRequestBytes, Usage: "maximum multipart build request size"},
			&cli.Int64Flag{Name: "max-extracted-bytes", Value: defaultMaxExtractedBytes, Usage: "maximum total uncompressed size of an uploaded build context"},
			&cli.IntFlag{Name: "max-archive-files", Value: defaultMaxArchiveFiles, Usage: "maximum number of entries in an uploaded build context"},
			&cli.IntFlag{Name: "max-retained-tasks", Value: defaultMaxRetainedTasks, Usage: "maximum queued, running, and retained tasks"},
			&cli.Int64Flag{Name: "max-work-dir-bytes", Value: defaultMaxWorkDirBytes, Usage: "maximum total bytes retained under work-dir"},
			&cli.DurationFlag{Name: "upload-read-timeout", Value: defaultUploadReadTimeout, Usage: "deadline for reading a multipart build request body"},
			&cli.IntFlag{Name: "addr-concurrency", Value: defaultAddrConcurrency, Usage: "maximum concurrent builds per buildkitd address"},
			&cli.IntFlag{Name: "max-concurrency", Usage: "maximum concurrent builds across all buildkitd addresses; extra tasks wait in queue; 0 disables the global limit"},
			&cli.Uint64Flag{Name: "rlimit-nofile", Usage: "raise RLIMIT_NOFILE to this value before serving; 0 keeps the runtime default"},
			&cli.DurationFlag{Name: "registry-check-timeout", Value: defaultRegistryTimeout, Usage: "timeout for HEAD image existence checks"},
			&cli.StringFlag{Name: "registry-check-host-rules", Usage: "semicolon-separated registry=network-host|token-host rules; empty disables checks"},
			&cli.IntFlag{Name: "registry-check-concurrency", Value: defaultRegistryChecks, Usage: "maximum concurrent HEAD image checks"},
			&cli.StringFlag{Name: "tlscacert", Usage: "CA certificate for buildkitd TLS"},
			&cli.StringFlag{Name: "tlscert", Usage: "client certificate for buildkitd TLS"},
			&cli.StringFlag{Name: "tlskey", Usage: "client key for buildkitd TLS"},
			&cli.StringFlag{Name: "tlsdir", Usage: "directory containing ca.pem, cert.pem, key.pem for buildkitd TLS"},
			&cli.StringFlag{Name: "tlsservername", Usage: "server name for buildkitd TLS verification"},
		},
		Action: func(c *cli.Context) error {
			cfg := config{
				Listen:             c.String("listen"),
				BuildkitdAddrs:     c.String("buildkitd-addr"),
				AuthToken:          c.String("auth-token"),
				AuthTokenFile:      c.String("auth-token-file"),
				WorkDir:            c.String("work-dir"),
				DefaultMode:        c.String("default-mode"),
				ModesJSON:          c.String("modes-json"),
				RoutingTargetsJSON: c.String("routing-targets-json"),
				KeepTTL:            c.Duration("keep-ttl"),
				PprofListen:        c.String("pprof-listen"),
				MaxLogBytes:        c.Int64("max-log-bytes"),
				MaxRequestBytes:    c.Int64("max-request-bytes"),
				MaxExtractedBytes:  c.Int64("max-extracted-bytes"),
				MaxArchiveFiles:    c.Int("max-archive-files"),
				MaxRetainedTasks:   c.Int("max-retained-tasks"),
				MaxWorkDirBytes:    c.Int64("max-work-dir-bytes"),
				UploadReadTimeout:  c.Duration("upload-read-timeout"),
				AddrConcurrency:    c.Int("addr-concurrency"),
				MaxConcurrency:     c.Int("max-concurrency"),
				RLimitNoFile:       c.Uint64("rlimit-nofile"),
				RegistryTimeout:    c.Duration("registry-check-timeout"),
				RegistryRules:      c.String("registry-check-host-rules"),
				RegistryChecks:     c.Int("registry-check-concurrency"),
				TLS: tlsConfig{
					CACert:     c.String("tlscacert"),
					Cert:       c.String("tlscert"),
					Key:        c.String("tlskey"),
					Dir:        c.String("tlsdir"),
					ServerName: c.String("tlsservername"),
				},
			}
			return run(cfg)
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "buildctl-daemon: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	if err := loadAuthToken(&cfg); err != nil {
		return err
	}
	if cfg.MaxLogBytes <= 0 {
		cfg.MaxLogBytes = defaultMaxLogBytes
	}
	if cfg.MaxRequestBytes <= 0 {
		cfg.MaxRequestBytes = defaultMaxRequestBytes
	}
	if cfg.MaxExtractedBytes <= 0 {
		cfg.MaxExtractedBytes = defaultMaxExtractedBytes
	}
	if cfg.MaxArchiveFiles <= 0 {
		cfg.MaxArchiveFiles = defaultMaxArchiveFiles
	}
	if cfg.MaxRetainedTasks <= 0 {
		cfg.MaxRetainedTasks = defaultMaxRetainedTasks
	}
	if cfg.MaxWorkDirBytes <= 0 {
		cfg.MaxWorkDirBytes = defaultMaxWorkDirBytes
	}
	if cfg.UploadReadTimeout <= 0 {
		cfg.UploadReadTimeout = defaultUploadReadTimeout
	}
	if cfg.AddrConcurrency <= 0 {
		cfg.AddrConcurrency = defaultAddrConcurrency
	}
	if cfg.MaxConcurrency < 0 {
		cfg.MaxConcurrency = 0
	}
	if cfg.KeepTTL < 0 {
		return fmt.Errorf("--keep-ttl must not be negative")
	}
	if cfg.RegistryTimeout <= 0 {
		cfg.RegistryTimeout = defaultRegistryTimeout
	}
	if cfg.RegistryChecks <= 0 {
		cfg.RegistryChecks = defaultRegistryChecks
	}
	if err := applyNoFileLimit(cfg.RLimitNoFile); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	startPprofServer(cfg.PprofListen)

	addrs, err := parseBuildkitAddrs(cfg.BuildkitdAddrs)
	if err != nil {
		return err
	}
	pool := newAddrPool(addrs, cfg.AddrConcurrency)
	modes, err := parseBuildModes(cfg.ModesJSON, cfg.DefaultMode, cfg.MaxConcurrency)
	if err != nil {
		return err
	}
	router, err := parseRoutingTargets(cfg.RoutingTargetsJSON)
	if err != nil {
		return err
	}
	if modes.routingRequired() && router == nil {
		return fmt.Errorf("at least one routing target is required when a build mode enables routing")
	}
	runner, err := newRealBuildRunner(cfg.TLS)
	if err != nil {
		return err
	}
	defer runner.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startBuildkitAddrRefresher(ctx, pool, cfg.BuildkitdAddrs)

	server := &buildServer{
		cfg:    cfg,
		store:  newTaskStore(),
		pool:   pool,
		runner: runner,
		modes:  modes,
		router: router,
		images: &registryImageChecker{
			timeout:   cfg.RegistryTimeout,
			hostRules: parseRegistryHostRules(cfg.RegistryRules),
			slots:     make(chan struct{}, cfg.RegistryChecks),
		},
		globalSem: newGlobalSem(cfg.MaxConcurrency),
	}
	server.scheduler, err = newBuildModeScheduler(modes, server.runBuildTask)
	if err != nil {
		return err
	}
	defer server.scheduler.close()
	server.startTaskReaper(ctx)
	e := server.routes()

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           e,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func loadAuthToken(cfg *config) error {
	if cfg.AuthToken != "" && cfg.AuthTokenFile != "" {
		return errors.New("--auth-token and --auth-token-file are mutually exclusive")
	}
	if cfg.AuthTokenFile == "" {
		return nil
	}
	token, err := os.ReadFile(cfg.AuthTokenFile)
	if err != nil {
		return fmt.Errorf("read auth token file: %w", err)
	}
	cfg.AuthToken = strings.TrimSpace(string(token))
	if cfg.AuthToken == "" {
		return errors.New("auth token file is empty")
	}
	return nil
}

func startPprofServer(addr string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return
	}
	go func() {
		fmt.Fprintf(os.Stderr, "buildctl-daemon pprof listening on %s\n", addr)
		if err := http.ListenAndServe(addr, nil); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "buildctl-daemon pprof server failed: %v\n", err)
		}
	}()
}

func (s *buildServer) routes() *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/healthz", func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	e.GET("/metrics", s.handleMetrics)

	v1 := e.Group("/v1")
	if s.cfg.AuthToken != "" {
		v1.Use(bearerAuthMiddleware(s.cfg.AuthToken))
	}
	v1.POST("/builds", s.handleCreateBuild)
	v1.GET("/builds", s.handleListBuilds)
	v1.GET("/builds/:id", s.handleGetBuild)
	v1.GET("/builds/:id/logs", s.handleGetBuildLogs)
	v1.DELETE("/builds/:id", s.handleCancelBuild)
	v1.POST("/builds/:id/cancel", s.handleCancelBuild)
	v1.HEAD("/images", s.handleHeadImage)
	return e
}

func (s *buildServer) handleHeadImage(c echo.Context) error {
	image := strings.TrimSpace(c.QueryParam("image"))
	if image == "" {
		return c.NoContent(http.StatusBadRequest)
	}
	if s.images == nil {
		return c.NoContent(http.StatusServiceUnavailable)
	}

	result, err := s.images.Check(c.Request().Context(), image)
	if err != nil {
		var notAllowed *registryNotAllowedError
		if errors.As(err, &notAllowed) {
			return c.NoContent(http.StatusForbidden)
		}
		if errors.Is(err, errRegistryCheckBusy) {
			return c.NoContent(http.StatusTooManyRequests)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return c.NoContent(http.StatusGatewayTimeout)
		}
		if errors.Is(err, containerderrdefs.ErrNotFound) {
			return c.NoContent(http.StatusNotFound)
		}
		var invalidRef *invalidImageReferenceError
		if errors.As(err, &invalidRef) {
			return c.NoContent(http.StatusBadRequest)
		}
		return c.NoContent(http.StatusBadGateway)
	}

	if result.Digest != "" {
		c.Response().Header().Set("Docker-Content-Digest", result.Digest)
	}
	if result.MediaType != "" {
		c.Response().Header().Set(echo.HeaderContentType, result.MediaType)
	}
	if result.Size >= 0 {
		c.Response().Header().Set(echo.HeaderContentLength, strconv.FormatInt(result.Size, 10))
	}
	return c.NoContent(http.StatusOK)
}

type invalidImageReferenceError struct {
	image string
	err   error
}

func (e *invalidImageReferenceError) Error() string {
	return fmt.Sprintf("invalid image reference %q: %v", e.image, e.err)
}

func (e *invalidImageReferenceError) Unwrap() error { return e.err }

type registryNotAllowedError struct {
	host string
}

func (e *registryNotAllowedError) Error() string {
	return fmt.Sprintf("registry host %q is not allowed", e.host)
}

var errRegistryCheckBusy = errors.New("too many concurrent registry checks")

func normalizeRegistryImageReference(image string) (string, string, error) {
	trimmed := strings.TrimSpace(image)
	firstSlash := strings.IndexByte(trimmed, '/')
	if firstSlash <= 0 {
		return "", "", &invalidImageReferenceError{image: image, err: errors.New("a fully qualified registry/repository reference is required")}
	}
	registry := trimmed[:firstSlash]
	if !strings.ContainsAny(registry, ".:") && registry != "localhost" {
		return "", "", &invalidImageReferenceError{image: image, err: errors.New("a fully qualified registry hostname is required")}
	}

	named, err := reference.ParseNormalizedNamed(trimmed)
	if err != nil {
		return "", "", &invalidImageReferenceError{image: image, err: err}
	}
	if _, ok := named.(reference.Digested); ok {
		return "", "", &invalidImageReferenceError{image: image, err: errors.New("digest references are not supported")}
	}
	named = reference.TagNameOnly(named)
	return named.String(), reference.Domain(named), nil
}

func (r *registryImageChecker) Check(ctx context.Context, image string) (imageCheckResult, error) {
	normalized, registry, err := normalizeRegistryImageReference(image)
	if err != nil {
		return imageCheckResult{}, err
	}
	allowedNetworkHosts, ok := r.hostRules[normalizeRegistryHost(registry)]
	if !ok {
		return imageCheckResult{}, &registryNotAllowedError{host: registry}
	}
	if r.slots != nil {
		select {
		case r.slots <- struct{}{}:
			defer func() { <-r.slots }()
		default:
			return imageCheckResult{}, errRegistryCheckBusy
		}
	}

	dockerConfig, err := dockerconfig.Load(r.configDir)
	if err != nil {
		return imageCheckResult{}, fmt.Errorf("load docker config: %w", err)
	}
	credentials := func(host string) (string, string, error) {
		for _, candidate := range registryCredentialHosts(host) {
			auth, err := dockerConfig.GetAuthConfig(candidate)
			if err != nil {
				return "", "", err
			}
			if auth.RegistryToken != "" {
				return "", auth.RegistryToken, nil
			}
			if auth.IdentityToken != "" {
				return "", auth.IdentityToken, nil
			}
			if auth.Username != "" || auth.Password != "" {
				return auth.Username, auth.Password, nil
			}
		}
		return "", "", nil
	}

	timeout := r.timeout
	if timeout <= 0 {
		timeout = defaultRegistryTimeout
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	baseTransport := r.transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	transport := &registryCheckTransport{
		base:           baseTransport,
		allowedHosts:   allowedNetworkHosts,
		registryHost:   normalizeRegistryHost(registry),
		allowPlainHTTP: registryUsesPlainHTTP(registry),
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resolver := containerddocker.NewResolver(containerddocker.ResolverOptions{
		Client:      client,
		Credentials: credentials,
		PlainHTTP:   registryUsesPlainHTTP(registry),
	})
	_, descriptor, err := resolver.Resolve(checkCtx, normalized)
	if err != nil {
		return imageCheckResult{}, err
	}
	return imageCheckResult{
		Digest:    descriptor.Digest.String(),
		MediaType: descriptor.MediaType,
		Size:      descriptor.Size,
	}, nil
}

func registryCredentialHosts(host string) []string {
	candidates := []string{host}
	if host == "registry-1.docker.io" || host == "docker.io" {
		candidates = append(candidates, "docker.io", "https://index.docker.io/v1/")
	}
	seen := make(map[string]struct{}, len(candidates))
	unique := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		unique = append(unique, candidate)
	}
	return unique
}

type registryCheckTransport struct {
	base           http.RoundTripper
	allowedHosts   map[string]struct{}
	registryHost   string
	allowPlainHTTP bool
}

func (t *registryCheckTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	requestHost := normalizeRegistryHost(req.URL.Host)
	if _, ok := t.allowedHosts[requestHost]; !ok {
		return nil, fmt.Errorf("registry check refuses request to untrusted host %q", requestHost)
	}
	scheme := strings.ToLower(req.URL.Scheme)
	if scheme == "https" {
		return t.base.RoundTrip(req)
	}
	if scheme == "http" && t.allowPlainHTTP && normalizeRegistryHost(req.URL.Host) == t.registryHost {
		return t.base.RoundTrip(req)
	}
	return nil, fmt.Errorf("registry check refuses insecure request to %s", req.URL.Redacted())
}

func parseRegistryHostRules(raw string) map[string]map[string]struct{} {
	rules := make(map[string]map[string]struct{})
	for _, item := range strings.Split(raw, ";") {
		registry, hostsRaw, ok := strings.Cut(item, "=")
		registry = normalizeRegistryHost(registry)
		if !ok || registry == "" {
			continue
		}
		hosts := make(map[string]struct{})
		for _, host := range strings.Split(hostsRaw, "|") {
			host = normalizeRegistryHost(host)
			if host != "" {
				hosts[host] = struct{}{}
			}
		}
		if len(hosts) > 0 {
			rules[registry] = hosts
		}
	}
	return rules
}

func normalizeRegistryHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

func registryUsesPlainHTTP(registry string) bool {
	host := registry
	if parsedHost, _, err := net.SplitHostPort(registry); err == nil {
		host = parsedHost
	}
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func bearerAuthMiddleware(token string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			header := c.Request().Header.Get(echo.HeaderAuthorization)
			if header != "Bearer "+token {
				return c.JSON(http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			}
			return next(c)
		}
	}
}

func (s *buildServer) handleCreateBuild(c echo.Context) error {
	s.admissionMu.Lock()
	admissionLocked := true
	defer func() {
		if admissionLocked {
			s.admissionMu.Unlock()
		}
	}()
	maxRetainedTasks := s.cfg.MaxRetainedTasks
	if maxRetainedTasks <= 0 {
		maxRetainedTasks = defaultMaxRetainedTasks
	}
	if s.store.len() >= maxRetainedTasks {
		return c.JSON(http.StatusTooManyRequests, map[string]string{"error": "maximum retained task count reached"})
	}
	maxWorkDirBytes := s.cfg.MaxWorkDirBytes
	if maxWorkDirBytes <= 0 {
		maxWorkDirBytes = defaultMaxWorkDirBytes
	}
	workDirBytes, err := directorySize(s.cfg.WorkDir)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	remainingWorkDirBytes := maxWorkDirBytes - workDirBytes
	if remainingWorkDirBytes <= 0 {
		return c.JSON(http.StatusInsufficientStorage, map[string]string{"error": "work directory byte budget exhausted"})
	}
	maxRequestBytes := s.cfg.MaxRequestBytes
	if maxRequestBytes <= 0 {
		maxRequestBytes = defaultMaxRequestBytes
	}
	if remainingWorkDirBytes < maxRequestBytes {
		maxRequestBytes = remainingWorkDirBytes
	}
	uploadReadTimeout := s.cfg.UploadReadTimeout
	if uploadReadTimeout <= 0 {
		uploadReadTimeout = defaultUploadReadTimeout
	}
	controller := http.NewResponseController(c.Response().Writer)
	deadlineSet := controller.SetReadDeadline(time.Now().Add(uploadReadTimeout)) == nil
	c.Request().Body = http.MaxBytesReader(c.Response().Writer, c.Request().Body, maxRequestBytes)
	parseErr := c.Request().ParseMultipartForm(32 << 20)
	if deadlineSet {
		_ = controller.SetReadDeadline(time.Time{})
	}
	if c.Request().MultipartForm != nil {
		defer c.Request().MultipartForm.RemoveAll()
	}
	if parseErr != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(parseErr, &maxBytesErr) {
			return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "build request exceeds max-request-bytes"})
		}
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid multipart build request"})
	}
	imageType, err := parseImageType(firstNonEmpty(c.FormValue("image_type"), c.FormValue("compression")))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	mode, modeCfg, err := s.modes.resolve(c.FormValue("mode"))
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	upload, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "file is required"})
	}

	id, err := newTaskID()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	taskDir := filepath.Join(s.cfg.WorkDir, id)
	contextParentDir := filepath.Join(taskDir, "context")
	if err := os.MkdirAll(contextParentDir, 0o755); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	zipPath := filepath.Join(taskDir, "source.zip")
	if err := saveUploadedFile(upload, zipPath, maxRequestBytes); err != nil {
		_ = os.RemoveAll(taskDir)
		if errors.Is(err, errArchiveLimit) {
			return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	zipInfo, err := os.Stat(zipPath)
	if err != nil {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	remainingWorkDirBytes -= zipInfo.Size()
	if remainingWorkDirBytes <= 0 {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusInsufficientStorage, map[string]string{"error": "work directory byte budget exhausted"})
	}
	maxExtractedBytes := s.cfg.MaxExtractedBytes
	if maxExtractedBytes <= 0 {
		maxExtractedBytes = defaultMaxExtractedBytes
	}
	if remainingWorkDirBytes < maxExtractedBytes {
		maxExtractedBytes = remainingWorkDirBytes
	}
	maxArchiveFiles := s.cfg.MaxArchiveFiles
	if maxArchiveFiles <= 0 {
		maxArchiveFiles = defaultMaxArchiveFiles
	}
	if err := extractZip(zipPath, contextParentDir, maxExtractedBytes, maxArchiveFiles); err != nil {
		_ = os.RemoveAll(taskDir)
		if errors.Is(err, errArchiveLimit) {
			return c.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		}
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	contextDir, err := findBuildContext(contextParentDir)
	if err != nil {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	hashStartedAt := time.Now()
	fmt.Fprintf(os.Stderr, "buildctl-daemon: hashing source context for task %s at %s\n", id, contextDir)
	scheduleKey, err := sourceContextContentHash(contextDir)
	if err != nil {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	fmt.Fprintf(os.Stderr, "buildctl-daemon: source context hash for task %s done in %s key=%s\n", id, time.Since(hashStartedAt).Round(100*time.Millisecond), shortScheduleKey(scheduleKey))
	if _, err := dockerfilepreprocess.PreprocessDockerfile(filepath.Join(contextDir, "Dockerfile")); err != nil {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	image, err := resolveBuildImage(contextDir, c.FormValue("image"))
	if err != nil {
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	routedTarget := image
	routingDigest := strings.TrimPrefix(scheduleKey, "source:")
	if modeCfg.RoutingEnabled {
		if s.router == nil {
			_ = os.RemoveAll(taskDir)
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": "registry routing is not configured"})
		}
		routedTarget, err = s.router.Route(image, routingDigest)
		if err != nil {
			_ = os.RemoveAll(taskDir)
			return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
	}
	syncBuild := queryBool(firstNonEmpty(c.FormValue("sync"), c.QueryParam("sync")))
	buildParentCtx := context.Background()
	if syncBuild {
		buildParentCtx = c.Request().Context()
	}
	buildCtx, cancel := context.WithCancel(buildParentCtx)
	if timeout, err := parseOptionalPositiveInt(c.FormValue("timeout_seconds")); err != nil {
		cancel()
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	} else if timeout > 0 {
		cancel()
		buildCtx, cancel = context.WithTimeout(buildParentCtx, time.Duration(timeout)*time.Second)
	}
	retry, err := parseOptionalNonNegativeInt(firstNonEmpty(c.FormValue("retry"), c.QueryParam("retry")), "retry", defaultBuildRetry)
	if err != nil {
		cancel()
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	retryIntervalSeconds, err := parseOptionalNonNegativeInt(firstNonEmpty(c.FormValue("retry-interval"), c.QueryParam("retry-interval")), "retry-interval", int(defaultRetryInterval/time.Second))
	if err != nil {
		cancel()
		_ = os.RemoveAll(taskDir)
		return c.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	task := &buildTask{
		ID:            id,
		Status:        buildStatusQueued,
		Image:         image,
		Mode:          mode,
		RoutedTarget:  routedTarget,
		ImageType:     imageType,
		Target:        strings.TrimSpace(c.FormValue("target")),
		CreatedAt:     time.Now(),
		LogPath:       filepath.Join(taskDir, "build.log"),
		WorkDir:       taskDir,
		contextDir:    contextDir,
		scheduleKey:   scheduleKey,
		routingDigest: routingDigest,
		noCache:       queryBool(c.FormValue("no_cache")),
		retry:         retry,
		retryInterval: time.Duration(retryIntervalSeconds) * time.Second,
		buildArgs:     parseBuildArgs(c.Request()),
		ctx:           buildCtx,
		cancel:        cancel,
		done:          make(chan struct{}),
	}
	s.store.add(task)
	done := task.done
	if s.scheduler == nil {
		go s.runBuildTask(task.ID)
	} else if err := s.scheduler.enqueue(task.Mode, task.ID); err != nil {
		task.cancel()
		close(task.done)
		s.cleanupTask(task.ID)
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
	}
	s.admissionMu.Unlock()
	admissionLocked = false
	if syncBuild {
		return s.handleSyncBuild(c, task.ID, done)
	}

	snapshot, _ := s.store.get(task.ID)
	return c.JSON(http.StatusAccepted, viewTask(snapshot))
}

func (s *buildServer) handleListBuilds(c echo.Context) error {
	return c.JSON(http.StatusOK, viewTasks(s.store.list()))
}

func (s *buildServer) handleGetBuild(c echo.Context) error {
	task, ok := s.store.get(c.Param("id"))
	if !ok {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "build not found"})
	}
	return c.JSON(http.StatusOK, viewTask(task))
}

func (s *buildServer) handleGetBuildLogs(c echo.Context) error {
	task, ok := s.store.get(c.Param("id"))
	if !ok {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "build not found"})
	}
	tailBytes := s.cfg.MaxLogBytes
	if raw := strings.TrimSpace(c.QueryParam("tail_bytes")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed <= 0 {
			return c.JSON(http.StatusBadRequest, map[string]string{"error": "tail_bytes must be a positive integer"})
		}
		tailBytes = parsed
	}
	maxLogBytes := s.cfg.MaxLogBytes
	if maxLogBytes <= 0 {
		maxLogBytes = defaultMaxLogBytes
	}
	if tailBytes > maxLogBytes {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "tail_bytes exceeds max-log-bytes"})
	}
	logs, err := tailFile(task.LogPath, tailBytes)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return c.String(http.StatusOK, logs)
}

func (s *buildServer) handleCancelBuild(c echo.Context) error {
	task, ok := s.store.cancel(c.Param("id"))
	if !ok {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "build not found"})
	}
	return c.JSON(http.StatusAccepted, viewTask(task))
}

func (s *buildServer) runBuildTask(id string) {
	defer func() {
		s.store.update(id, func(task *buildTask) {
			if task.done != nil {
				close(task.done)
				task.done = nil
			}
		})
	}()

	snapshot, ok := s.store.get(id)
	if !ok {
		return
	}
	if snapshot.Status == buildStatusCanceled {
		return
	}

	logFile, err := os.OpenFile(snapshot.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.finishTask(id, buildStatusFailed, nil, err)
		return
	}
	defer logFile.Close()
	maxLogBytes := s.cfg.MaxLogBytes
	if maxLogBytes <= 0 {
		maxLogBytes = defaultMaxLogBytes
	}
	logWriter := &limitedLogWriter{writer: logFile, remaining: maxLogBytes}
	if snapshot.RoutedTarget != "" && snapshot.RoutedTarget != snapshot.Image {
		fmt.Fprintf(logWriter, "route algorithm=rendezvous_hash context_sha256=%s source=%s routed_target=%s\n", snapshot.routingDigest, snapshot.Image, snapshot.RoutedTarget)
	}

	buildImage := snapshot.RoutedTarget
	if buildImage == "" {
		buildImage = snapshot.Image
	}
	steps := buildStepsForImageType(buildImage, snapshot.ImageType)
	var exporter map[string]string
	for _, step := range steps {
		if step.BaseImage != "" {
			contextDir, baseImage, err := createNydusFromOCIContext(snapshot.WorkDir, step.BaseImage, exporter)
			if err != nil {
				s.finishTask(id, buildStatusFailed, exporter, err)
				return
			}
			step.ContextDir = contextDir
			fmt.Fprintf(logWriter, "build nydus from OCI image %s\n", baseImage)
		}
		var err error
		for attempt := 0; attempt <= snapshot.retry; attempt++ {
			if attempt > 0 {
				s.markTaskQueued(id)
				fmt.Fprintf(logWriter, "retry build %s image %s attempt %d/%d after error: %v\n", step.Format, step.Image, attempt, snapshot.retry, err)
				if !waitRetryInterval(snapshot.ctx, snapshot.retryInterval) {
					s.finishTask(id, buildStatusCanceled, exporter, snapshot.ctx.Err())
					return
				}
			}
			exporter, err = s.runBuildStepAttempt(id, snapshot, step, logWriter)
			if err == nil {
				break
			}
			if snapshot.ctx.Err() != nil {
				s.finishTask(id, buildStatusCanceled, exporter, snapshot.ctx.Err())
				return
			}
			if isDeterministicBuildError(err) {
				break
			}
		}
		if err != nil {
			s.finishTask(id, buildStatusFailed, exporter, err)
			return
		}
	}
	s.finishTask(id, buildStatusSucceeded, exporter, nil)
}

func (s *buildServer) runBuildStepAttempt(id string, snapshot *buildTask, step buildStep, logFile io.Writer) (map[string]string, error) {
	contextDir := snapshot.contextDir
	target := snapshot.Target
	buildArgs := snapshot.buildArgs
	if step.ContextDir != "" {
		contextDir = step.ContextDir
		target = ""
		buildArgs = nil
	}

	excludedAddrs := make(map[string]bool)
	for {
		s.markTaskQueued(id)
		releaseGlobal, err := s.acquireGlobalSlot(snapshot.ctx)
		if err != nil {
			return nil, err
		}
		slot, err := s.acquireAddrSlot(snapshot.ctx, snapshot.scheduleKey, excludedAddrs)
		if err != nil {
			releaseGlobal()
			return nil, err
		}

		started := time.Now()
		s.store.update(id, func(task *buildTask) {
			if task.StartedAt == nil {
				task.StartedAt = &started
			}
			task.Status = buildStatusRunning
			task.BuildkitAddr = slot.addr.addr
			task.NodeIP = slot.addr.nodeIP
		})
		fmt.Fprintf(logFile, "build %s image %s on %s\n", step.Format, step.Image, slot.addr.addr)
		exporter, err := s.runner.Build(snapshot.ctx, buildRunRequest{
			ContextDir:   contextDir,
			BuildkitAddr: slot.addr.addr,
			Image:        step.Image,
			Format:       step.Format,
			Target:       target,
			NoCache:      snapshot.noCache,
			BuildArgs:    buildArgs,
		}, logFile)
		<-slot.sem
		releaseGlobal()
		if err == nil || snapshot.ctx.Err() != nil {
			return exporter, err
		}
		if !isRetryableBuildkitAddrError(err, slot.addr.addr) {
			return exporter, err
		}
		excludedAddrs[slot.addr.addr] = true
		fmt.Fprintf(logFile, "retryable buildkit address error on %s: %v; refreshing addresses and trying another worker\n", slot.addr.addr, err)
		if refreshErr := s.refreshBuildkitAddrs(); refreshErr != nil {
			fmt.Fprintf(logFile, "refresh buildkit addresses failed: %v\n", refreshErr)
		}
	}
}

func (s *buildServer) markTaskQueued(id string) {
	s.store.update(id, func(task *buildTask) {
		if task.Status != buildStatusQueued && task.Status != buildStatusRunning {
			return
		}
		task.Status = buildStatusQueued
		task.BuildkitAddr = ""
		task.NodeIP = ""
	})
}

func waitRetryInterval(ctx context.Context, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *buildServer) finishTask(id, status string, exporter map[string]string, err error) {
	finished := time.Now()
	var applied string
	s.store.update(id, func(task *buildTask) {
		if task.Status == buildStatusCanceled && status != buildStatusCanceled {
			return
		}
		task.Status = status
		task.FinishedAt = &finished
		task.ExporterResponse = cloneStringMap(exporter)
		if err != nil {
			task.Error = err.Error()
		}
		applied = status
	})
	switch applied {
	case buildStatusSucceeded:
		atomic.AddInt64(&s.metrics.succeeded, 1)
	case buildStatusFailed:
		atomic.AddInt64(&s.metrics.failed, 1)
	}
}

// handleMetrics exposes Prometheus text metrics. The active gauge comes from
// the global semaphore, while queued/running describe task states.
func (s *buildServer) handleMetrics(c echo.Context) error {
	queued := s.store.countByStatus(buildStatusQueued)
	running := s.store.countByStatus(buildStatusRunning)
	active := running
	if s.globalSem != nil {
		active = len(s.globalSem)
	}
	succeeded := atomic.LoadInt64(&s.metrics.succeeded)
	failed := atomic.LoadInt64(&s.metrics.failed)

	var b strings.Builder
	b.WriteString("# HELP buildctl_daemon_builds_queued Builds waiting for a buildkit worker slot.\n")
	b.WriteString("# TYPE buildctl_daemon_builds_queued gauge\n")
	fmt.Fprintf(&b, "buildctl_daemon_builds_queued %d\n", queued)
	b.WriteString("# HELP buildctl_daemon_builds_running Builds currently running.\n")
	b.WriteString("# TYPE buildctl_daemon_builds_running gauge\n")
	fmt.Fprintf(&b, "buildctl_daemon_builds_running %d\n", running)
	b.WriteString("# HELP buildctl_daemon_builds_active Builds currently holding a global execution slot.\n")
	b.WriteString("# TYPE buildctl_daemon_builds_active gauge\n")
	fmt.Fprintf(&b, "buildctl_daemon_builds_active %d\n", active)
	b.WriteString("# HELP buildctl_daemon_builds_succeeded_total Builds that completed successfully.\n")
	b.WriteString("# TYPE buildctl_daemon_builds_succeeded_total counter\n")
	fmt.Fprintf(&b, "buildctl_daemon_builds_succeeded_total %d\n", succeeded)
	b.WriteString("# HELP buildctl_daemon_builds_failed_total Builds that ended in failure.\n")
	b.WriteString("# TYPE buildctl_daemon_builds_failed_total counter\n")
	fmt.Fprintf(&b, "buildctl_daemon_builds_failed_total %d\n", failed)
	if s.modes != nil {
		b.WriteString("# HELP buildctl_daemon_mode_builds_queued Builds waiting for a worker slot, partitioned by mode.\n")
		b.WriteString("# TYPE buildctl_daemon_mode_builds_queued gauge\n")
		b.WriteString("# HELP buildctl_daemon_mode_builds_running Builds currently running, partitioned by mode.\n")
		b.WriteString("# TYPE buildctl_daemon_mode_builds_running gauge\n")
		for _, mode := range s.modes.names() {
			fmt.Fprintf(&b, "buildctl_daemon_mode_builds_queued{mode=%q} %d\n", mode, s.store.countByModeAndStatus(mode, buildStatusQueued))
			fmt.Fprintf(&b, "buildctl_daemon_mode_builds_running{mode=%q} %d\n", mode, s.store.countByModeAndStatus(mode, buildStatusRunning))
		}
	}
	return c.Blob(http.StatusOK, "text/plain; version=0.0.4", []byte(b.String()))
}

func (s *buildServer) startTaskReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(defaultCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				s.cleanupExpiredTasks(now)
			}
		}
	}()
}

func (s *buildServer) cleanupExpiredTasks(now time.Time) {
	expired := s.store.deleteExpired(now, s.cfg.KeepTTL)
	for _, task := range expired {
		s.cleanupTaskWorkDir(task)
	}
}

func (s *buildServer) cleanupTask(id string) {
	task, ok := s.store.delete(id)
	if !ok {
		return
	}
	s.cleanupTaskWorkDir(task)
}

func (s *buildServer) cleanupTaskWorkDir(task *buildTask) {
	if task == nil || strings.TrimSpace(task.WorkDir) == "" {
		return
	}
	if err := os.RemoveAll(task.WorkDir); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup task %s work dir %s: %v\n", task.ID, task.WorkDir, err)
	}
}

func (s *buildServer) handleSyncBuild(c echo.Context, id string, done <-chan struct{}) error {
	select {
	case <-done:
	case <-c.Request().Context().Done():
		return nil
	}
	task, ok := s.store.get(id)
	if !ok {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "build not found"})
	}
	return c.JSON(http.StatusOK, viewTask(task))
}

// newGlobalSem builds the global build-concurrency semaphore; a limit of 0
// (or below) returns nil, which disables the global cap.
func newGlobalSem(limit int) chan struct{} {
	if limit <= 0 {
		return nil
	}
	return make(chan struct{}, limit)
}

// acquireGlobalSlot blocks until a global build slot frees up (keeping the
// task in queued status) or ctx is done. The returned release func must be
// called exactly once; it is a no-op when no global limit is configured.
func (s *buildServer) acquireGlobalSlot(ctx context.Context) (func(), error) {
	if s.globalSem == nil {
		return func() {}, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s.globalSem <- struct{}{}:
		return func() { <-s.globalSem }, nil
	}
}

func (s *buildServer) acquireAddrSlot(ctx context.Context, key string, excluded map[string]bool) (*addrSlot, error) {
	snapshot := s.pool.snapshot()
	slot := pickAvailableAddrSlot(snapshot, key, excluded)
	if slot != nil {
		return slot, nil
	}

	slots := filterExcludedAddrSlots(orderedAddrSlots(snapshot, key), excluded)
	if len(slots) == 0 {
		return nil, fmt.Errorf("no buildkitd addresses available")
	}
	if len(slots) == 1 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case slots[0].sem <- struct{}{}:
			return slots[0], nil
		}
	}
	selectCases := make([]reflect.SelectCase, 0, len(slots)+1)
	selectCases = append(selectCases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})
	for _, slot := range slots {
		selectCases = append(selectCases, reflect.SelectCase{
			Dir:  reflect.SelectSend,
			Chan: reflect.ValueOf(slot.sem),
			Send: reflect.ValueOf(struct{}{}),
		})
	}
	chosen, _, _ := reflect.Select(selectCases)
	if chosen == 0 {
		return nil, ctx.Err()
	}
	return slots[chosen-1], nil
}

func (s *buildServer) refreshBuildkitAddrs() error {
	refreshed, err := parseBuildkitAddrs(s.cfg.BuildkitdAddrs)
	if err != nil {
		return err
	}
	before := s.pool.addresses()
	s.pool.replace(refreshed)
	after := s.pool.addresses()
	if !sameStringSlice(before, after) {
		fmt.Fprintf(os.Stderr, "refreshed buildkit addresses: [%s] -> [%s]\n", strings.Join(before, ","), strings.Join(after, ","))
	}
	return nil
}

func isRetryableBuildkitAddrError(err error, buildkitAddr string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"connect: no route to host",
		"connect: connection refused",
		"failed to dial",
		"i/o timeout",
	} {
		if strings.Contains(msg, needle) {
			return errorMentionsBuildkitAddr(msg, buildkitAddr)
		}
	}
	for _, needle := range []string{
		"connection reset by peer",
		"connection error",
		"error reading server preface",
		"transport is closing",
		"unavailable",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func isDeterministicBuildError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"did not complete successfully: exit code:",
		"dockerfile parse error",
		"failed to parse dockerfile",
		"syntax error",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func errorMentionsBuildkitAddr(msg, buildkitAddr string) bool {
	addr := strings.ToLower(strings.TrimSpace(buildkitAddr))
	if addr == "" {
		return false
	}
	hostPort := strings.TrimPrefix(strings.TrimPrefix(addr, "tcp://"), "http://")
	hostPort = strings.TrimPrefix(hostPort, "https://")
	hostPort = strings.Trim(hostPort, "/")
	if hostPort == "" {
		return false
	}
	return strings.Contains(msg, hostPort)
}

type buildStep struct {
	Image      string
	Format     string
	BaseImage  string
	ContextDir string
}

func buildStepsForImageType(image, imageType string) []buildStep {
	switch imageType {
	case imageTypeOCI:
		return []buildStep{{Image: stripNydusV3Suffix(image), Format: imageTypeOCI}}
	case imageTypeBoth:
		ociImage := stripNydusV3Suffix(image)
		return []buildStep{
			{Image: ociImage, Format: imageTypeOCI},
			{Image: ensureNydusV3Suffix(ociImage), Format: imageTypeNydus, BaseImage: ociImage},
		}
	default:
		return []buildStep{{Image: ensureNydusV3Suffix(image), Format: imageTypeNydus}}
	}
}

func createNydusFromOCIContext(workDir, image string, exporter map[string]string) (string, string, error) {
	digestValue := strings.TrimSpace(exporter[exporterImageDigestKey])
	if digestValue == "" {
		return "", "", fmt.Errorf("OCI build did not return %s", exporterImageDigestKey)
	}
	parsedDigest, err := digest.Parse(digestValue)
	if err != nil {
		return "", "", fmt.Errorf("parse OCI image digest %q: %w", digestValue, err)
	}
	named, err := reference.ParseNormalizedNamed(strings.TrimSpace(image))
	if err != nil {
		return "", "", fmt.Errorf("parse OCI image reference %q: %w", image, err)
	}
	canonical, err := reference.WithDigest(named, parsedDigest)
	if err != nil {
		return "", "", fmt.Errorf("pin OCI image reference %q: %w", image, err)
	}

	baseImage := reference.FamiliarString(canonical)
	contextDir := filepath.Join(workDir, "nydus-from-oci")
	if err := os.MkdirAll(contextDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create nydus conversion context: %w", err)
	}
	dockerfile := []byte("FROM " + baseImage + "\n")
	if err := os.WriteFile(filepath.Join(contextDir, "Dockerfile"), dockerfile, 0o644); err != nil {
		return "", "", fmt.Errorf("write nydus conversion Dockerfile: %w", err)
	}
	return contextDir, baseImage, nil
}

func parseImageType(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", imageTypeNydus:
		return imageTypeNydus, nil
	case imageTypeOCI, "gzip":
		return imageTypeOCI, nil
	case imageTypeBoth:
		return imageTypeBoth, nil
	default:
		return "", fmt.Errorf("image_type must be one of nydus, oci, both")
	}
}

func outputAttrs(image, format string) map[string]string {
	attrs := map[string]string{
		"name":              image,
		"push":              "true",
		"force-compression": "true",
		"oci-mediatypes":    "true",
	}
	if format == imageTypeOCI {
		attrs["compression"] = "gzip"
	} else {
		attrs["compression"] = "nydus"
		attrs["fs-version"] = "5"
	}
	return attrs
}

func frontendAttrs(target string, noCache bool, buildArgs map[string]string) map[string]string {
	attrs := make(map[string]string, len(buildArgs)+2)
	if strings.TrimSpace(target) != "" {
		attrs["target"] = strings.TrimSpace(target)
	}
	if noCache {
		attrs["no-cache"] = ""
	}
	for k, v := range buildArgs {
		attrs["build-arg:"+k] = v
	}
	return attrs
}

func hasNydusV3Suffix(target string) bool {
	return strings.HasSuffix(strings.TrimSpace(target), nydusV3TargetSuffix)
}

func ensureNydusV3Suffix(target string) string {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" || hasNydusV3Suffix(trimmed) {
		return trimmed
	}
	return trimmed + nydusV3TargetSuffix
}

func stripNydusV3Suffix(target string) string {
	return strings.TrimSuffix(strings.TrimSpace(target), nydusV3TargetSuffix)
}

type realBuildRunner struct {
	tls     tlsConfig
	clients map[string]*buildkit.Client
	mu      sync.Mutex
}

func newRealBuildRunner(tls tlsConfig) (*realBuildRunner, error) {
	resolved, err := resolveTLSConfig(tls)
	if err != nil {
		return nil, err
	}
	return &realBuildRunner{tls: resolved, clients: make(map[string]*buildkit.Client)}, nil
}

func (r *realBuildRunner) Build(ctx context.Context, req buildRunRequest, log io.Writer) (map[string]string, error) {
	client, err := r.getClient(ctx, req)
	if err != nil {
		return nil, err
	}
	dockerConfig, err := dockerconfig.Load("")
	if err != nil {
		return nil, fmt.Errorf("load docker config: %w", err)
	}
	statusCh := make(chan *buildkit.SolveStatus)
	done := make(chan struct{})
	go func() {
		defer close(done)
		writeSolveStatus(log, statusCh)
	}()

	resp, err := client.Solve(ctx, nil, buildkit.SolveOpt{
		Exports: []buildkit.ExportEntry{{
			Type:  buildkit.ExporterImage,
			Attrs: outputAttrs(req.Image, req.Format),
		}},
		LocalDirs:     map[string]string{"context": req.ContextDir, "dockerfile": req.ContextDir},
		Frontend:      buildkitDockerFrontend,
		FrontendAttrs: frontendAttrs(req.Target, req.NoCache, req.BuildArgs),
		Session:       []session.Attachable{authprovider.NewDockerAuthProvider(authprovider.DockerAuthProviderConfig{ConfigFile: dockerConfig})},
	}, statusCh)
	<-done
	if err != nil {
		return nil, err
	}
	return resp.ExporterResponse, nil
}

func (r *realBuildRunner) getClient(ctx context.Context, req buildRunRequest) (*buildkit.Client, error) {
	addr := strings.TrimSpace(req.BuildkitAddr)
	if addr == "" {
		return nil, errors.New("internal error: buildkit address was not set")
	}
	return r.clientForAddr(ctx, addr)
}

func (r *realBuildRunner) clientForAddr(ctx context.Context, addr string) (*buildkit.Client, error) {
	r.mu.Lock()
	if c, ok := r.clients[addr]; ok {
		r.mu.Unlock()
		return c, nil
	}
	r.mu.Unlock()

	var opts []buildkit.ClientOpt
	if r.tls.CACert != "" || r.tls.Cert != "" || r.tls.Key != "" {
		serverName := r.tls.ServerName
		if serverName == "" {
			if parsed, err := url.Parse(addr); err == nil {
				serverName = parsed.Hostname()
			}
		}
		if r.tls.CACert != "" || serverName != "" {
			opts = append(opts, buildkit.WithServerConfig(serverName, r.tls.CACert))
		}
		if r.tls.Cert != "" || r.tls.Key != "" {
			opts = append(opts, buildkit.WithCredentials(r.tls.Cert, r.tls.Key))
		}
	}

	c, err := buildkit.New(ctx, addr, opts...)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if existing, ok := r.clients[addr]; ok {
		r.mu.Unlock()
		_ = c.Close()
		return existing, nil
	}
	r.clients[addr] = c
	r.mu.Unlock()
	return c, nil
}

func (r *realBuildRunner) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var errs []string
	for addr, client := range r.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", addr, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func resolveTLSConfig(tls tlsConfig) (tlsConfig, error) {
	if tls.Dir == "" {
		return tls, nil
	}
	if tls.CACert != "" || tls.Cert != "" || tls.Key != "" {
		return tlsConfig{}, errors.New("cannot specify tlsdir and tlscacert/tlscert/tlskey at the same time")
	}
	return tlsConfig{
		CACert:     filepath.Join(tls.Dir, "ca.pem"),
		Cert:       filepath.Join(tls.Dir, "cert.pem"),
		Key:        filepath.Join(tls.Dir, "key.pem"),
		Dir:        tls.Dir,
		ServerName: tls.ServerName,
	}, nil
}

func applyNoFileLimit(limit uint64) error {
	if limit == 0 {
		return nil
	}
	var current syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &current); err != nil {
		return fmt.Errorf("get RLIMIT_NOFILE: %w", err)
	}
	if current.Cur >= limit && current.Max >= limit {
		return nil
	}
	requested := syscall.Rlimit{Cur: limit, Max: limit}
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &requested); err != nil {
		return fmt.Errorf("set RLIMIT_NOFILE to %d (current soft=%d hard=%d): %w", limit, current.Cur, current.Max, err)
	}
	return nil
}
func writeSolveStatus(w io.Writer, statusCh <-chan *buildkit.SolveStatus) {
	encoder := json.NewEncoder(w)
	for status := range statusCh {
		for _, vertex := range status.Vertexes {
			_ = encoder.Encode(map[string]any{
				"type":      "vertex",
				"digest":    vertex.Digest.String(),
				"name":      vertex.Name,
				"cached":    vertex.Cached,
				"error":     vertex.Error,
				"started":   vertex.Started,
				"completed": vertex.Completed,
			})
		}
		for _, progress := range status.Statuses {
			_ = encoder.Encode(map[string]any{
				"type":      "status",
				"id":        progress.ID,
				"name":      progress.Name,
				"current":   progress.Current,
				"total":     progress.Total,
				"timestamp": progress.Timestamp,
			})
		}
		for _, log := range status.Logs {
			_ = encoder.Encode(map[string]any{
				"type":      "log",
				"stream":    log.Stream,
				"data":      string(log.Data),
				"timestamp": log.Timestamp,
			})
		}
		for _, warning := range status.Warnings {
			_ = encoder.Encode(map[string]any{
				"type":   "warning",
				"level":  warning.Level,
				"short":  string(warning.Short),
				"detail": warning.Detail,
				"url":    warning.URL,
			})
		}
	}
}

var errArchiveLimit = errors.New("archive exceeds configured limit")

func saveUploadedFile(upload *multipart.FileHeader, dst string, maxBytes int64) error {
	if upload.Size > maxBytes {
		return fmt.Errorf("%w: compressed upload exceeds %d bytes", errArchiveLimit, maxBytes)
	}
	src, err := upload.Open()
	if err != nil {
		return fmt.Errorf("open upload: %w", err)
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create upload file: %w", err)
	}
	defer out.Close()
	_, exceeded, err := copyWithLimit(out, src, maxBytes)
	if err != nil {
		return fmt.Errorf("save upload: %w", err)
	}
	if exceeded {
		return fmt.Errorf("%w: compressed upload exceeds %d bytes", errArchiveLimit, maxBytes)
	}
	return nil
}

func extractZip(src, dst string, maxExtractedBytes int64, maxArchiveFiles int) error {
	reader, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	defer reader.Close()
	if len(reader.File) > maxArchiveFiles {
		return fmt.Errorf("%w: archive has %d entries, maximum is %d", errArchiveLimit, len(reader.File), maxArchiveFiles)
	}

	var extractedBytes int64
	for _, file := range reader.File {
		if err := validateBuildArchivePath(file.Name); err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(file.Name))
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		remaining := maxExtractedBytes - extractedBytes
		if remaining < 0 || file.UncompressedSize64 > uint64(remaining) {
			return fmt.Errorf("%w: extracted content exceeds %d bytes", errArchiveLimit, maxExtractedBytes)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := file.Open()
		if err != nil {
			return err
		}
		mode := file.FileInfo().Mode().Perm()
		if mode == 0 || mode > zipMaxExtractedFileMode {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			in.Close()
			return err
		}
		written, exceeded, copyErr := copyWithLimit(out, in, remaining)
		closeErr := out.Close()
		in.Close()
		if copyErr != nil {
			return copyErr
		}
		if exceeded {
			_ = os.Remove(target)
			return fmt.Errorf("%w: extracted content exceeds %d bytes", errArchiveLimit, maxExtractedBytes)
		}
		extractedBytes += written
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func copyWithLimit(dst io.Writer, src io.Reader, limit int64) (int64, bool, error) {
	written, err := io.Copy(dst, io.LimitReader(src, limit))
	if err != nil {
		return written, false, err
	}
	var extra [1]byte
	n, err := src.Read(extra[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return written, false, err
	}
	return written, n > 0, nil
}

type limitedLogWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedLogWriter) Write(data []byte) (int, error) {
	originalLength := len(data)
	if w.remaining <= 0 || originalLength == 0 {
		return originalLength, nil
	}
	if int64(len(data)) > w.remaining {
		data = data[:w.remaining]
	}
	written, err := w.writer.Write(data)
	w.remaining -= int64(written)
	if err != nil {
		return written, err
	}
	if written != len(data) {
		return written, io.ErrShortWrite
	}
	return originalLength, nil
}

func validateBuildArchivePath(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || filepath.IsAbs(name) {
		return fmt.Errorf("invalid zip path %q", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid zip path %q", name)
	}
	return nil
}

func findBuildContext(root string) (string, error) {
	if fileExists(filepath.Join(root, "Dockerfile")) {
		return root, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(root, entry.Name())
		if fileExists(filepath.Join(candidate, "Dockerfile")) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("zip must contain Dockerfile at root or in one top-level directory")
	}
	return candidates[0], nil
}

func resolveBuildImage(contextDir, imageOverride string) (string, error) {
	image := strings.TrimSpace(imageOverride)
	if image != "" {
		return image, nil
	}

	metadata, err := readImageMetadata(contextDir)
	if err != nil {
		return "", err
	}
	image = strings.TrimSpace(metadata.Target)
	if image == "" {
		return "", fmt.Errorf("image is required when metadata.json target is empty")
	}
	return image, nil
}

func readImageMetadata(contextDir string) (imageMetadata, error) {
	path := filepath.Join(contextDir, "metadata.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return imageMetadata{}, fmt.Errorf("image is required when metadata.json is missing")
		}
		return imageMetadata{}, fmt.Errorf("read metadata.json: %w", err)
	}
	var metadata imageMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return imageMetadata{}, fmt.Errorf("parse metadata.json: %w", err)
	}
	return metadata, nil
}

func sourceContextContentHash(dir string) (string, error) {
	type contextFile struct {
		path string
		rel  string
	}
	var files []contextFile
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dir {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return fmt.Errorf("rel source context path: %w", err)
		}
		files = append(files, contextFile{path: path, rel: filepath.ToSlash(rel)})
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk source context for scheduling hash: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })

	h := sha256.New()
	for _, file := range files {
		_, _ = h.Write([]byte(file.rel))
		_, _ = h.Write([]byte{0})
		f, err := os.Open(file.path)
		if err != nil {
			return "", fmt.Errorf("open source context file %s: %w", file.rel, err)
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("hash source context file %s: %w", file.rel, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close source context file %s: %w", file.rel, closeErr)
		}
		_, _ = h.Write([]byte{0})
	}

	return fmt.Sprintf("source:%x", h.Sum(nil)), nil
}

func shortScheduleKey(key string) string {
	if len(key) <= len("source:")+12 || !strings.HasPrefix(key, "source:") {
		return key
	}
	return key[:len("source:")+12]
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func tailFile(path string, maxBytes int64) (string, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxLogBytes
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func newTaskID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func parseBuildArgs(req *http.Request) map[string]string {
	if req.MultipartForm == nil {
		return nil
	}
	args := make(map[string]string)
	for key, values := range req.MultipartForm.Value {
		if len(values) == 0 {
			continue
		}
		var name string
		switch {
		case strings.HasPrefix(key, "build_arg."):
			name = strings.TrimPrefix(key, "build_arg.")
		case strings.HasPrefix(key, "build_arg:"):
			name = strings.TrimPrefix(key, "build_arg:")
		default:
			continue
		}
		name = strings.TrimSpace(name)
		if name != "" {
			args[name] = values[0]
		}
	}
	if len(args) == 0 {
		return nil
	}
	return args
}

func queryBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func parseOptionalPositiveInt(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("timeout_seconds must be a positive integer")
	}
	return parsed, nil
}

func parseOptionalNonNegativeInt(raw, name string, def int) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return def, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return parsed, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

type buildkitAddr struct {
	original string
	addr     string
	nodeIP   string
}

type addrSlot struct {
	addr *buildkitAddr
	sem  chan struct{}
}

type addrPoolSnapshot struct {
	slots []*addrSlot
	hash  *consistentHash
}

type addrPool struct {
	mu          sync.RWMutex
	concurrency int
	slots       []*addrSlot
	hash        *consistentHash
}

func newAddrPool(addrs []*buildkitAddr, concurrency int) *addrPool {
	p := &addrPool{concurrency: concurrency}
	p.replace(addrs)
	return p
}

func (p *addrPool) snapshot() addrPoolSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	slots := append([]*addrSlot(nil), p.slots...)
	return addrPoolSnapshot{slots: slots, hash: p.hash}
}

func (p *addrPool) addresses() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slotAddressKeys(p.slots)
}

func (p *addrPool) replace(addrs []*buildkitAddr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	existing := make(map[string]*addrSlot, len(p.slots))
	for _, slot := range p.slots {
		if slot != nil && slot.addr != nil {
			existing[slot.addr.addr] = slot
		}
	}
	newSlots := make([]*addrSlot, 0, len(addrs))
	for _, addr := range addrs {
		if addr == nil {
			continue
		}
		if slot, ok := existing[addr.addr]; ok {
			newSlots = append(newSlots, slot)
			continue
		}
		newSlots = append(newSlots, &addrSlot{addr: addr, sem: make(chan struct{}, p.concurrency)})
	}
	sort.Slice(newSlots, func(i, j int) bool { return newSlots[i].addr.addr < newSlots[j].addr.addr })
	p.slots = newSlots
	if len(newSlots) == 0 {
		p.hash = nil
		return
	}
	p.hash = newConsistentHash(len(newSlots), 150)
}

type consistentHash struct {
	ring     []uint32
	nodeMap  map[uint32]int
	numNodes int
}

func newConsistentHash(numNodes, replicas int) *consistentHash {
	ch := &consistentHash{nodeMap: make(map[uint32]int), numNodes: numNodes}
	for i := 0; i < numNodes; i++ {
		for r := 0; r < replicas; r++ {
			h := fnv1a(fmt.Sprintf("node-%d-replica-%d", i, r))
			ch.ring = append(ch.ring, h)
			ch.nodeMap[h] = i
		}
	}
	sort.Slice(ch.ring, func(i, j int) bool { return ch.ring[i] < ch.ring[j] })
	return ch
}

func (ch *consistentHash) getSlotOrder(key string) []int {
	if len(ch.ring) == 0 {
		return nil
	}
	h := fnv1a(key)
	idx := sort.Search(len(ch.ring), func(i int) bool { return ch.ring[i] >= h })
	if idx >= len(ch.ring) {
		idx = 0
	}
	seen := make(map[int]bool)
	order := make([]int, 0, ch.numNodes)
	for len(order) < ch.numNodes {
		nodeIdx := ch.nodeMap[ch.ring[idx%len(ch.ring)]]
		if !seen[nodeIdx] {
			seen[nodeIdx] = true
			order = append(order, nodeIdx)
		}
		idx++
	}
	return order
}

func fnv1a(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func pickAvailableAddrSlot(snapshot addrPoolSnapshot, key string, excluded map[string]bool) *addrSlot {
	for _, slot := range filterExcludedAddrSlots(orderedAddrSlots(snapshot, key), excluded) {
		if !tryAcquireAddrSlot(slot.sem) {
			continue
		}
		return slot
	}
	return nil
}

func filterExcludedAddrSlots(slots []*addrSlot, excluded map[string]bool) []*addrSlot {
	if len(slots) == 0 || len(excluded) == 0 {
		return slots
	}
	filtered := make([]*addrSlot, 0, len(slots))
	for _, slot := range slots {
		if slot == nil || slot.addr == nil || excluded[slot.addr.addr] {
			continue
		}
		filtered = append(filtered, slot)
	}
	return filtered
}

func orderedAddrSlots(snapshot addrPoolSnapshot, key string) []*addrSlot {
	if len(snapshot.slots) == 0 || snapshot.hash == nil {
		return nil
	}
	order := snapshot.hash.getSlotOrder(key)
	slots := make([]*addrSlot, 0, len(order))
	for _, idx := range order {
		if idx < 0 || idx >= len(snapshot.slots) {
			continue
		}
		slot := snapshot.slots[idx]
		if slot == nil || slot.addr == nil {
			continue
		}
		slots = append(slots, slot)
	}
	return slots
}

func tryAcquireAddrSlot(sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func startBuildkitAddrRefresher(ctx context.Context, pool *addrPool, addrsRaw string) {
	if strings.TrimSpace(addrsRaw) == "" {
		return
	}
	go func() {
		ticker := time.NewTicker(defaultAddrRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			refreshed, err := parseBuildkitAddrs(addrsRaw)
			if err != nil {
				fmt.Fprintf(os.Stderr, "refresh buildkit addresses: %v\n", err)
				continue
			}
			before := pool.addresses()
			pool.replace(refreshed)
			after := pool.addresses()
			if !sameStringSlice(before, after) {
				fmt.Fprintf(os.Stderr, "refreshed buildkit addresses: [%s] -> [%s]\n", strings.Join(before, ","), strings.Join(after, ","))
			}
		}
	}()
}

func slotAddressKeys(slots []*addrSlot) []string {
	keys := make([]string, 0, len(slots))
	for _, slot := range slots {
		if slot != nil && slot.addr != nil {
			keys = append(keys, slot.addr.addr)
		}
	}
	return keys
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func parseBuildkitAddrs(raw string) ([]*buildkitAddr, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("--buildkitd-addr is required")
	}
	parts := strings.Split(raw, ",")
	addrs := make([]*buildkitAddr, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		addr := part
		if !strings.Contains(addr, "://") {
			addr = "tcp://" + addr
		}
		hostPort := strings.TrimPrefix(addr, "tcp://")
		host, port, err := net.SplitHostPort(hostPort)
		if err != nil {
			addrs = append(addrs, &buildkitAddr{original: part, addr: addr, nodeIP: nodeIPFromBuildkitAddr(addr)})
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			addrs = append(addrs, &buildkitAddr{original: part, addr: addr, nodeIP: ip.String()})
			continue
		}
		ips, err := net.LookupHost(host)
		if err != nil {
			addrs = append(addrs, &buildkitAddr{original: part, addr: addr, nodeIP: nodeIPFromBuildkitAddr(addr)})
			continue
		}
		for _, ip := range ips {
			resolved := fmt.Sprintf("tcp://%s", net.JoinHostPort(ip, port))
			addrs = append(addrs, &buildkitAddr{original: part, addr: resolved, nodeIP: ip})
		}
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no valid buildkitd addresses")
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].addr < addrs[j].addr })
	return addrs, nil
}

func nodeIPFromBuildkitAddr(addr string) string {
	hostPort := strings.TrimPrefix(strings.TrimSpace(addr), "tcp://")
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return ""
}
