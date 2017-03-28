package gps

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sdboyer/constext"
	"github.com/sdboyer/gps/pkgtree"
)

// Used to compute a friendly filepath from a URL-shaped input.
var sanitizer = strings.NewReplacer("-", "--", ":", "-", "/", "-", "+", "-")

// A SourceManager is responsible for retrieving, managing, and interrogating
// source repositories. Its primary purpose is to serve the needs of a Solver,
// but it is handy for other purposes, as well.
//
// gps's built-in SourceManager, SourceMgr, is intended to be generic and
// sufficient for any purpose. It provides some additional semantics around the
// methods defined here.
type SourceManager interface {
	// SourceExists checks if a repository exists, either upstream or in the
	// SourceManager's central repository cache.
	SourceExists(ProjectIdentifier) (bool, error)

	// SyncSourceFor will attempt to bring all local information about a source
	// fully up to date.
	SyncSourceFor(ProjectIdentifier) error

	// ListVersions retrieves a list of the available versions for a given
	// repository name.
	ListVersions(ProjectIdentifier) ([]Version, error)

	// RevisionPresentIn indicates whether the provided Version is present in
	// the given repository.
	RevisionPresentIn(ProjectIdentifier, Revision) (bool, error)

	// ListPackages parses the tree of the Go packages at or below root of the
	// provided ProjectIdentifier, at the provided version.
	ListPackages(ProjectIdentifier, Version) (pkgtree.PackageTree, error)

	// GetManifestAndLock returns manifest and lock information for the provided
	// root import path.
	//
	// gps currently requires that projects be rooted at their repository root,
	// necessitating that the ProjectIdentifier's ProjectRoot must also be a
	// repository root.
	GetManifestAndLock(ProjectIdentifier, Version) (Manifest, Lock, error)

	// ExportProject writes out the tree of the provided import path, at the
	// provided version, to the provided directory.
	ExportProject(ProjectIdentifier, Version, string) error

	// AnalyzerInfo reports the name and version of the logic used to service
	// GetManifestAndLock().
	AnalyzerInfo() (name string, version int)

	// DeduceRootProject takes an import path and deduces the corresponding
	// project/source root.
	DeduceProjectRoot(ip string) (ProjectRoot, error)
}

// A ProjectAnalyzer is responsible for analyzing a given path for Manifest and
// Lock information. Tools relying on gps must implement one.
type ProjectAnalyzer interface {
	// Perform analysis of the filesystem tree rooted at path, with the
	// root import path importRoot, to determine the project's constraints, as
	// indicated by a Manifest and Lock.
	DeriveManifestAndLock(path string, importRoot ProjectRoot) (Manifest, Lock, error)

	// Report the name and version of this ProjectAnalyzer.
	Info() (name string, version int)
}

// SourceMgr is the default SourceManager for gps.
//
// There's no (planned) reason why it would need to be reimplemented by other
// tools; control via dependency injection is intended to be sufficient.
type SourceMgr struct {
	cachedir    string                // path to root of cache dir
	lf          *os.File              // handle for the sm lock file on disk
	callMgr     *callManager          // subsystem that coordinates running calls/io
	deduceCoord *deductionCoordinator // subsystem that manages import path deduction
	srcCoord    *sourceCoordinator    // subsystem that manages sources
	an          ProjectAnalyzer       // analyzer injected by the caller
	qch         chan struct{}         // quit chan for signal handler
	sigmut      sync.Mutex            // mutex protecting signal handling setup/teardown
	glock       sync.RWMutex          // global lock for all ops, sm validity
	opcount     int32                 // number of ops in flight
	relonce     sync.Once             // once-er to ensure we only release once
	releasing   int32                 // flag indicating release of sm has begun
}

type smIsReleased struct{}

func (smIsReleased) Error() string {
	return "this SourceMgr has been released, its methods can no longer be called"
}

var _ SourceManager = &SourceMgr{}

// NewSourceManager produces an instance of gps's built-in SourceManager. It
// takes a cache directory (where local instances of upstream repositories are
// stored), and a ProjectAnalyzer that is used to extract manifest and lock
// information from source trees.
//
// The returned SourceManager aggressively caches information wherever possible.
// If tools need to do preliminary work involving upstream repository analysis
// prior to invoking a solve run, it is recommended that they create this
// SourceManager as early as possible and use it to their ends. That way, the
// solver can benefit from any caches that may have already been warmed.
//
// gps's SourceManager is intended to be threadsafe (if it's not, please file a
// bug!). It should be safe to reuse across concurrent solving runs, even on
// unrelated projects.
func NewSourceManager(an ProjectAnalyzer, cachedir string) (*SourceMgr, error) {
	if an == nil {
		return nil, fmt.Errorf("a ProjectAnalyzer must be provided to the SourceManager")
	}

	err := os.MkdirAll(filepath.Join(cachedir, "sources"), 0777)
	if err != nil {
		return nil, err
	}

	glpath := filepath.Join(cachedir, "sm.lock")
	_, err = os.Stat(glpath)
	if err == nil {
		return nil, CouldNotCreateLockError{
			Path: glpath,
			Err:  fmt.Errorf("cache lock file %s exists - another process crashed or is still running?", glpath),
		}
	}

	fi, err := os.OpenFile(glpath, os.O_CREATE|os.O_EXCL, 0600) // is 0600 sane for this purpose?
	if err != nil {
		return nil, CouldNotCreateLockError{
			Path: glpath,
			Err:  fmt.Errorf("err on attempting to create global cache lock: %s", err),
		}
	}

	cm := newCallManager(context.TODO())
	deducer := newDeductionCoordinator(cm)

	sm := &SourceMgr{
		cachedir:    cachedir,
		lf:          fi,
		callMgr:     cm,
		deduceCoord: deducer,
		srcCoord:    newSourceCoordinator(cm, deducer, cachedir),
		an:          an,
		qch:         make(chan struct{}),
	}

	return sm, nil
}

// UseDefaultSignalHandling sets up typical os.Interrupt signal handling for a
// SourceMgr.
func (sm *SourceMgr) UseDefaultSignalHandling() {
	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, os.Interrupt)
	sm.HandleSignals(sigch)
}

// HandleSignals sets up logic to handle incoming signals with the goal of
// shutting down the SourceMgr safely.
//
// Calling code must provide the signal channel, and is responsible for calling
// signal.Notify() on that channel.
//
// Successive calls to HandleSignals() will deregister the previous handler and
// set up a new one. It is not recommended that the same channel be passed
// multiple times to this method.
//
// SetUpSigHandling() will set up a handler that is appropriate for most
// use cases.
func (sm *SourceMgr) HandleSignals(sigch chan os.Signal) {
	sm.sigmut.Lock()
	// always start by closing the qch, which will lead to any existing signal
	// handler terminating, and deregistering its sigch.
	if sm.qch != nil {
		close(sm.qch)
	}
	sm.qch = make(chan struct{})

	// Run a new goroutine with the input sigch and the fresh qch
	go func(sch chan os.Signal, qch <-chan struct{}) {
		defer signal.Stop(sch)
		for {
			select {
			case <-sch:
				// Set up a timer to uninstall the signal handler after three
				// seconds, so that the user can easily force termination with a
				// second ctrl-c
				go func(c <-chan time.Time) {
					<-c
					signal.Stop(sch)
				}(time.After(3 * time.Second))

				if !atomic.CompareAndSwapInt32(&sm.releasing, 0, 1) {
					// Something's already called Release() on this sm, so we
					// don't have to do anything, as we'd just be redoing
					// that work. Instead, deregister and return.
					return
				}

				opc := atomic.LoadInt32(&sm.opcount)
				if opc > 0 {
					fmt.Printf("Signal received: waiting for %v ops to complete...\n", opc)
				}

				// Mutex interaction in a signal handler is, as a general rule,
				// unsafe. I'm not clear on whether the guarantees Go provides
				// around signal handling, or having passed this through a
				// channel in general, obviate those concerns, but it's a lot
				// easier to just rely on the mutex contained in the Once right
				// now, so do that until it proves problematic or someone
				// provides a clear explanation.
				sm.relonce.Do(func() { sm.doRelease() })
				return
			case <-qch:
				// quit channel triggered - deregister our sigch and return
				return
			}
		}
	}(sigch, sm.qch)
	// Try to ensure handler is blocked in for-select before releasing the mutex
	runtime.Gosched()

	sm.sigmut.Unlock()
}

// StopSignalHandling deregisters any signal handler running on this SourceMgr.
//
// It's normally not necessary to call this directly; it will be called as
// needed by Release().
func (sm *SourceMgr) StopSignalHandling() {
	sm.sigmut.Lock()
	if sm.qch != nil {
		close(sm.qch)
		sm.qch = nil
		runtime.Gosched()
	}
	sm.sigmut.Unlock()
}

// CouldNotCreateLockError describe failure modes in which creating a SourceMgr
// did not succeed because there was an error while attempting to create the
// on-disk lock file.
type CouldNotCreateLockError struct {
	Path string
	Err  error
}

func (e CouldNotCreateLockError) Error() string {
	return e.Err.Error()
}

// Release lets go of any locks held by the SourceManager. Once called, it is no
// longer safe to call methods against it; all method calls will immediately
// result in errors.
func (sm *SourceMgr) Release() {
	// Set sm.releasing before entering the Once func to guarantee that no
	// _more_ method calls will stack up if/while waiting.
	atomic.CompareAndSwapInt32(&sm.releasing, 0, 1)

	// Whether 'releasing' is set or not, we don't want this function to return
	// until after the doRelease process is done, as doing so could cause the
	// process to terminate before a signal-driven doRelease() call has a chance
	// to finish its cleanup.
	sm.relonce.Do(func() { sm.doRelease() })
}

// doRelease actually releases physical resources (files on disk, etc.).
//
// This must be called only and exactly once. Calls to it should be wrapped in
// the sm.relonce sync.Once instance.
func (sm *SourceMgr) doRelease() {
	// Grab the global sm lock so that we only release once we're sure all other
	// calls have completed
	//
	// (This could deadlock, ofc)
	sm.glock.Lock()

	// Close the file handle for the lock file
	sm.lf.Close()
	// Remove the lock file from disk
	os.Remove(filepath.Join(sm.cachedir, "sm.lock"))
	// Close the qch, if non-nil, so the signal handlers run out. This will
	// also deregister the sig channel, if any has been set up.
	if sm.qch != nil {
		close(sm.qch)
	}
	sm.glock.Unlock()
}

// AnalyzerInfo reports the name and version of the injected ProjectAnalyzer.
func (sm *SourceMgr) AnalyzerInfo() (name string, version int) {
	return sm.an.Info()
}

// GetManifestAndLock returns manifest and lock information for the provided
// import path. gps currently requires that projects be rooted at their
// repository root, necessitating that the ProjectIdentifier's ProjectRoot must
// also be a repository root.
//
// The work of producing the manifest and lock is delegated to the injected
// ProjectAnalyzer's DeriveManifestAndLock() method.
func (sm *SourceMgr) GetManifestAndLock(id ProjectIdentifier, v Version) (Manifest, Lock, error) {
	if atomic.CompareAndSwapInt32(&sm.releasing, 1, 1) {
		return nil, nil, smIsReleased{}
	}
	atomic.AddInt32(&sm.opcount, 1)
	sm.glock.RLock()
	defer func() {
		sm.glock.RUnlock()
		atomic.AddInt32(&sm.opcount, -1)
	}()

	srcg, err := sm.srcCoord.getSourceGatewayFor(context.TODO(), id)
	if err != nil {
		return nil, nil, err
	}

	return srcg.getManifestAndLock(context.TODO(), id.ProjectRoot, v, sm.an)
}

// ListPackages parses the tree of the Go packages at and below the ProjectRoot
// of the given ProjectIdentifier, at the given version.
func (sm *SourceMgr) ListPackages(id ProjectIdentifier, v Version) (pkgtree.PackageTree, error) {
	if atomic.CompareAndSwapInt32(&sm.releasing, 1, 1) {
		return pkgtree.PackageTree{}, smIsReleased{}
	}
	atomic.AddInt32(&sm.opcount, 1)
	sm.glock.RLock()
	defer func() {
		sm.glock.RUnlock()
		atomic.AddInt32(&sm.opcount, -1)
	}()

	srcg, err := sm.srcCoord.getSourceGatewayFor(context.TODO(), id)
	if err != nil {
		return pkgtree.PackageTree{}, err
	}

	return srcg.listPackages(context.TODO(), id.ProjectRoot, v)
}

// ListVersions retrieves a list of the available versions for a given
// repository name.
//
// The list is not sorted; while it may be returned in the order that the
// underlying VCS reports version information, no guarantee is made. It is
// expected that the caller either not care about order, or sort the result
// themselves.
//
// This list is always retrieved from upstream on the first call. Subsequent
// calls will return a cached version of the first call's results. if upstream
// is not accessible (network outage, access issues, or the resource actually
// went away), an error will be returned.
func (sm *SourceMgr) ListVersions(id ProjectIdentifier) ([]Version, error) {
	if atomic.CompareAndSwapInt32(&sm.releasing, 1, 1) {
		return nil, smIsReleased{}
	}
	atomic.AddInt32(&sm.opcount, 1)
	sm.glock.RLock()
	defer func() {
		sm.glock.RUnlock()
		atomic.AddInt32(&sm.opcount, -1)
	}()

	srcg, err := sm.srcCoord.getSourceGatewayFor(context.TODO(), id)
	if err != nil {
		// TODO(sdboyer) More-er proper-er errors
		return nil, err
	}

	return srcg.listVersions(context.TODO())
}

// RevisionPresentIn indicates whether the provided Revision is present in the given
// repository.
func (sm *SourceMgr) RevisionPresentIn(id ProjectIdentifier, r Revision) (bool, error) {
	if atomic.CompareAndSwapInt32(&sm.releasing, 1, 1) {
		return false, smIsReleased{}
	}
	atomic.AddInt32(&sm.opcount, 1)
	sm.glock.RLock()
	defer func() {
		sm.glock.RUnlock()
		atomic.AddInt32(&sm.opcount, -1)
	}()

	srcg, err := sm.srcCoord.getSourceGatewayFor(context.TODO(), id)
	if err != nil {
		// TODO(sdboyer) More-er proper-er errors
		return false, err
	}

	return srcg.revisionPresentIn(context.TODO(), r)
}

// SourceExists checks if a repository exists, either upstream or in the cache,
// for the provided ProjectIdentifier.
func (sm *SourceMgr) SourceExists(id ProjectIdentifier) (bool, error) {
	if atomic.CompareAndSwapInt32(&sm.releasing, 1, 1) {
		return false, smIsReleased{}
	}
	atomic.AddInt32(&sm.opcount, 1)
	sm.glock.RLock()
	defer func() {
		sm.glock.RUnlock()
		atomic.AddInt32(&sm.opcount, -1)
	}()

	srcg, err := sm.srcCoord.getSourceGatewayFor(context.TODO(), id)
	if err != nil {
		return false, err
	}

	return srcg.checkExistence(context.TODO(), existsInCache) || srcg.checkExistence(context.TODO(), existsUpstream), nil
}

// SyncSourceFor will ensure that all local caches and information about a
// source are up to date with any network-acccesible information.
//
// The primary use case for this is prefetching.
func (sm *SourceMgr) SyncSourceFor(id ProjectIdentifier) error {
	if atomic.CompareAndSwapInt32(&sm.releasing, 1, 1) {
		return smIsReleased{}
	}
	atomic.AddInt32(&sm.opcount, 1)
	sm.glock.RLock()
	defer func() {
		sm.glock.RUnlock()
		atomic.AddInt32(&sm.opcount, -1)
	}()

	srcg, err := sm.srcCoord.getSourceGatewayFor(context.TODO(), id)
	if err != nil {
		return err
	}

	return srcg.syncLocal(context.TODO())
}

// ExportProject writes out the tree of the provided ProjectIdentifier's
// ProjectRoot, at the provided version, to the provided directory.
func (sm *SourceMgr) ExportProject(id ProjectIdentifier, v Version, to string) error {
	if atomic.CompareAndSwapInt32(&sm.releasing, 1, 1) {
		return smIsReleased{}
	}
	atomic.AddInt32(&sm.opcount, 1)
	sm.glock.RLock()
	defer func() {
		sm.glock.RUnlock()
		atomic.AddInt32(&sm.opcount, -1)
	}()

	srcg, err := sm.srcCoord.getSourceGatewayFor(context.TODO(), id)
	if err != nil {
		return err
	}

	return srcg.exportVersionTo(context.TODO(), v, to)
}

// DeduceProjectRoot takes an import path and deduces the corresponding
// project/source root.
//
// Note that some import paths may require network activity to correctly
// determine the root of the path, such as, but not limited to, vanity import
// paths. (A special exception is written for gopkg.in to minimize network
// activity, as its behavior is well-structured)
func (sm *SourceMgr) DeduceProjectRoot(ip string) (ProjectRoot, error) {
	if atomic.CompareAndSwapInt32(&sm.releasing, 1, 1) {
		return "", smIsReleased{}
	}
	atomic.AddInt32(&sm.opcount, 1)
	sm.glock.RLock()
	defer func() {
		sm.glock.RUnlock()
		atomic.AddInt32(&sm.opcount, -1)
	}()

	pd, err := sm.deduceCoord.deduceRootPath(ip)
	return ProjectRoot(pd.root), err
}

type timeCount struct {
	count int
	start time.Time
}

type durCount struct {
	count int
	dur   time.Duration
}

type callManager struct {
	ctx        context.Context
	cancelFunc context.CancelFunc
	mu         sync.Mutex // Guards all maps.
	running    map[callInfo]timeCount
	//running map[callInfo]time.Time
	ran map[callType]durCount
	//ran map[callType]time.Duration
}

func newCallManager(ctx context.Context) *callManager {
	ctx, cf := context.WithCancel(ctx)
	return &callManager{
		ctx:        ctx,
		cancelFunc: cf,
		running:    make(map[callInfo]timeCount),
		ran:        make(map[callType]durCount),
	}
}

// Helper function to register a call with a callManager, combine contexts, and
// create a to-be-deferred func to clean it all up.
func (cm *callManager) setUpCall(inctx context.Context, name string, typ callType) (cctx context.Context, doneFunc func(), err error) {
	ci := callInfo{
		name: name,
		typ:  typ,
	}

	octx, err := cm.run(ci)
	if err != nil {
		return nil, nil, err
	}

	cctx, cancelFunc := constext.Cons(inctx, octx)
	return cctx, func() {
		cm.done(ci)
		cancelFunc() // ensure constext cancel goroutine is cleaned up
	}, nil
}

func (cm *callManager) getLifetimeContext() context.Context {
	return cm.ctx
}

func (cm *callManager) run(ci callInfo) (context.Context, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if cm.ctx.Err() != nil {
		// We've already been canceled; error out.
		return nil, cm.ctx.Err()
	}

	if existingInfo, has := cm.running[ci]; has {
		existingInfo.count++
		cm.running[ci] = existingInfo
	} else {
		cm.running[ci] = timeCount{
			count: 1,
			start: time.Now(),
		}
	}

	return cm.ctx, nil
}

func (cm *callManager) done(ci callInfo) {
	cm.mu.Lock()

	existingInfo, has := cm.running[ci]
	if !has {
		panic(fmt.Sprintf("sourceMgr: tried to complete a call that had not registered via run()"))
	}

	if existingInfo.count > 1 {
		// If more than one is pending, don't stop the clock yet.
		existingInfo.count--
		cm.running[ci] = existingInfo
	} else {
		// Last one for this particular key; update metrics with info.
		durCnt := cm.ran[ci.typ]
		durCnt.count++
		durCnt.dur += time.Now().Sub(existingInfo.start)
		cm.ran[ci.typ] = durCnt
		delete(cm.running, ci)
	}

	cm.mu.Unlock()
}

type callType uint

const (
	ctHTTPMetadata callType = iota
	ctListVersions
	ctGetManifestAndLock
)

// callInfo provides metadata about an ongoing call.
type callInfo struct {
	name string
	typ  callType
}
