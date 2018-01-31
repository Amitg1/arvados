// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"git.curoverse.com/arvados.git/lib/crunchstat"
	"git.curoverse.com/arvados.git/sdk/go/arvados"
	"git.curoverse.com/arvados.git/sdk/go/arvadosclient"
	"git.curoverse.com/arvados.git/sdk/go/keepclient"
	"git.curoverse.com/arvados.git/sdk/go/manifest"
	"golang.org/x/net/context"

	dockertypes "github.com/docker/docker/api/types"
	dockercontainer "github.com/docker/docker/api/types/container"
	dockernetwork "github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
)

var version = "dev"

// IArvadosClient is the minimal Arvados API methods used by crunch-run.
type IArvadosClient interface {
	Create(resourceType string, parameters arvadosclient.Dict, output interface{}) error
	Get(resourceType string, uuid string, parameters arvadosclient.Dict, output interface{}) error
	Update(resourceType string, uuid string, parameters arvadosclient.Dict, output interface{}) error
	Call(method, resourceType, uuid, action string, parameters arvadosclient.Dict, output interface{}) error
	CallRaw(method string, resourceType string, uuid string, action string, parameters arvadosclient.Dict) (reader io.ReadCloser, err error)
	Discovery(key string) (interface{}, error)
}

// ErrCancelled is the error returned when the container is cancelled.
var ErrCancelled = errors.New("Cancelled")

// IKeepClient is the minimal Keep API methods used by crunch-run.
type IKeepClient interface {
	PutHB(hash string, buf []byte) (string, int, error)
	ManifestFileReader(m manifest.Manifest, filename string) (arvados.File, error)
	ClearBlockCache()
}

// NewLogWriter is a factory function to create a new log writer.
type NewLogWriter func(name string) io.WriteCloser

type RunArvMount func(args []string, tok string) (*exec.Cmd, error)

type MkTempDir func(string, string) (string, error)

// ThinDockerClient is the minimal Docker client interface used by crunch-run.
type ThinDockerClient interface {
	ContainerAttach(ctx context.Context, container string, options dockertypes.ContainerAttachOptions) (dockertypes.HijackedResponse, error)
	ContainerCreate(ctx context.Context, config *dockercontainer.Config, hostConfig *dockercontainer.HostConfig,
		networkingConfig *dockernetwork.NetworkingConfig, containerName string) (dockercontainer.ContainerCreateCreatedBody, error)
	ContainerStart(ctx context.Context, container string, options dockertypes.ContainerStartOptions) error
	ContainerRemove(ctx context.Context, container string, options dockertypes.ContainerRemoveOptions) error
	ContainerWait(ctx context.Context, container string, condition dockercontainer.WaitCondition) (<-chan dockercontainer.ContainerWaitOKBody, <-chan error)
	ImageInspectWithRaw(ctx context.Context, image string) (dockertypes.ImageInspect, []byte, error)
	ImageLoad(ctx context.Context, input io.Reader, quiet bool) (dockertypes.ImageLoadResponse, error)
	ImageRemove(ctx context.Context, image string, options dockertypes.ImageRemoveOptions) ([]dockertypes.ImageDeleteResponseItem, error)
}

// ContainerRunner is the main stateful struct used for a single execution of a
// container.
type ContainerRunner struct {
	Docker    ThinDockerClient
	ArvClient IArvadosClient
	Kc        IKeepClient
	arvados.Container
	ContainerConfig dockercontainer.Config
	dockercontainer.HostConfig
	token       string
	ContainerID string
	ExitCode    *int
	NewLogWriter
	loggingDone   chan bool
	CrunchLog     *ThrottledLogger
	Stdout        io.WriteCloser
	Stderr        io.WriteCloser
	LogCollection *CollectionWriter
	LogsPDH       *string
	RunArvMount
	MkTempDir
	ArvMount      *exec.Cmd
	ArvMountPoint string
	HostOutputDir string
	Binds         []string
	Volumes       map[string]struct{}
	OutputPDH     *string
	SigChan       chan os.Signal
	ArvMountExit  chan error
	finalState    string
	parentTemp    string

	statLogger       io.WriteCloser
	statReporter     *crunchstat.Reporter
	hoststatLogger   io.WriteCloser
	hoststatReporter *crunchstat.Reporter
	statInterval     time.Duration
	cgroupRoot       string
	// What we expect the container's cgroup parent to be.
	expectCgroupParent string
	// What we tell docker to use as the container's cgroup
	// parent. Note: Ideally we would use the same field for both
	// expectCgroupParent and setCgroupParent, and just make it
	// default to "docker". However, when using docker < 1.10 with
	// systemd, specifying a non-empty cgroup parent (even the
	// default value "docker") hits a docker bug
	// (https://github.com/docker/docker/issues/17126). Using two
	// separate fields makes it possible to use the "expect cgroup
	// parent to be X" feature even on sites where the "specify
	// cgroup parent" feature breaks.
	setCgroupParent string

	cStateLock sync.Mutex
	cCancelled bool // StopContainer() invoked

	enableNetwork string // one of "default" or "always"
	networkMode   string // passed through to HostConfig.NetworkMode
	arvMountLog   *ThrottledLogger
}

// setupSignals sets up signal handling to gracefully terminate the underlying
// Docker container and update state when receiving a TERM, INT or QUIT signal.
func (runner *ContainerRunner) setupSignals() {
	runner.SigChan = make(chan os.Signal, 1)
	signal.Notify(runner.SigChan, syscall.SIGTERM)
	signal.Notify(runner.SigChan, syscall.SIGINT)
	signal.Notify(runner.SigChan, syscall.SIGQUIT)

	go func(sig chan os.Signal) {
		for s := range sig {
			runner.CrunchLog.Printf("caught signal: %v", s)
			runner.stop()
		}
	}(runner.SigChan)
}

// stop the underlying Docker container.
func (runner *ContainerRunner) stop() {
	runner.cStateLock.Lock()
	defer runner.cStateLock.Unlock()
	if runner.ContainerID == "" {
		return
	}
	runner.cCancelled = true
	runner.CrunchLog.Printf("removing container")
	err := runner.Docker.ContainerRemove(context.TODO(), runner.ContainerID, dockertypes.ContainerRemoveOptions{Force: true})
	if err != nil {
		runner.CrunchLog.Printf("error removing container: %s", err)
	}
}

func (runner *ContainerRunner) stopSignals() {
	if runner.SigChan != nil {
		signal.Stop(runner.SigChan)
	}
}

var errorBlacklist = []string{
	"(?ms).*[Cc]annot connect to the Docker daemon.*",
	"(?ms).*oci runtime error.*starting container process.*container init.*mounting.*to rootfs.*no such file or directory.*",
}
var brokenNodeHook *string = flag.String("broken-node-hook", "", "Script to run if node is detected to be broken (for example, Docker daemon is not running)")

func (runner *ContainerRunner) checkBrokenNode(goterr error) bool {
	for _, d := range errorBlacklist {
		if m, e := regexp.MatchString(d, goterr.Error()); m && e == nil {
			runner.CrunchLog.Printf("Error suggests node is unable to run containers: %v", goterr)
			if *brokenNodeHook == "" {
				runner.CrunchLog.Printf("No broken node hook provided, cannot mark node as broken.")
			} else {
				runner.CrunchLog.Printf("Running broken node hook %q", *brokenNodeHook)
				// run killme script
				c := exec.Command(*brokenNodeHook)
				c.Stdout = runner.CrunchLog
				c.Stderr = runner.CrunchLog
				err := c.Run()
				if err != nil {
					runner.CrunchLog.Printf("Error running broken node hook: %v", err)
				}
			}
			return true
		}
	}
	return false
}

// LoadImage determines the docker image id from the container record and
// checks if it is available in the local Docker image store.  If not, it loads
// the image from Keep.
func (runner *ContainerRunner) LoadImage() (err error) {

	runner.CrunchLog.Printf("Fetching Docker image from collection '%s'", runner.Container.ContainerImage)

	var collection arvados.Collection
	err = runner.ArvClient.Get("collections", runner.Container.ContainerImage, nil, &collection)
	if err != nil {
		return fmt.Errorf("While getting container image collection: %v", err)
	}
	manifest := manifest.Manifest{Text: collection.ManifestText}
	var img, imageID string
	for ms := range manifest.StreamIter() {
		img = ms.FileStreamSegments[0].Name
		if !strings.HasSuffix(img, ".tar") {
			return fmt.Errorf("First file in the container image collection does not end in .tar")
		}
		imageID = img[:len(img)-4]
	}

	runner.CrunchLog.Printf("Using Docker image id '%s'", imageID)

	_, _, err = runner.Docker.ImageInspectWithRaw(context.TODO(), imageID)
	if err != nil {
		runner.CrunchLog.Print("Loading Docker image from keep")

		var readCloser io.ReadCloser
		readCloser, err = runner.Kc.ManifestFileReader(manifest, img)
		if err != nil {
			return fmt.Errorf("While creating ManifestFileReader for container image: %v", err)
		}

		response, err := runner.Docker.ImageLoad(context.TODO(), readCloser, true)
		if err != nil {
			return fmt.Errorf("While loading container image into Docker: %v", err)
		}

		defer response.Body.Close()
		rbody, err := ioutil.ReadAll(response.Body)
		if err != nil {
			return fmt.Errorf("Reading response to image load: %v", err)
		}
		runner.CrunchLog.Printf("Docker response: %s", rbody)
	} else {
		runner.CrunchLog.Print("Docker image is available")
	}

	runner.ContainerConfig.Image = imageID

	runner.Kc.ClearBlockCache()

	return nil
}

func (runner *ContainerRunner) ArvMountCmd(arvMountCmd []string, token string) (c *exec.Cmd, err error) {
	c = exec.Command("arv-mount", arvMountCmd...)

	// Copy our environment, but override ARVADOS_API_TOKEN with
	// the container auth token.
	c.Env = nil
	for _, s := range os.Environ() {
		if !strings.HasPrefix(s, "ARVADOS_API_TOKEN=") {
			c.Env = append(c.Env, s)
		}
	}
	c.Env = append(c.Env, "ARVADOS_API_TOKEN="+token)

	runner.arvMountLog = NewThrottledLogger(runner.NewLogWriter("arv-mount"))
	c.Stdout = runner.arvMountLog
	c.Stderr = runner.arvMountLog

	runner.CrunchLog.Printf("Running %v", c.Args)

	err = c.Start()
	if err != nil {
		return nil, err
	}

	statReadme := make(chan bool)
	runner.ArvMountExit = make(chan error)

	keepStatting := true
	go func() {
		for keepStatting {
			time.Sleep(100 * time.Millisecond)
			_, err = os.Stat(fmt.Sprintf("%s/by_id/README", runner.ArvMountPoint))
			if err == nil {
				keepStatting = false
				statReadme <- true
			}
		}
		close(statReadme)
	}()

	go func() {
		mnterr := c.Wait()
		if mnterr != nil {
			runner.CrunchLog.Printf("Arv-mount exit error: %v", mnterr)
		}
		runner.ArvMountExit <- mnterr
		close(runner.ArvMountExit)
	}()

	select {
	case <-statReadme:
		break
	case err := <-runner.ArvMountExit:
		runner.ArvMount = nil
		keepStatting = false
		return nil, err
	}

	return c, nil
}

func (runner *ContainerRunner) SetupArvMountPoint(prefix string) (err error) {
	if runner.ArvMountPoint == "" {
		runner.ArvMountPoint, err = runner.MkTempDir(runner.parentTemp, prefix)
	}
	return
}

func copyfile(src string, dst string) (err error) {
	srcfile, err := os.Open(src)
	if err != nil {
		return
	}

	os.MkdirAll(path.Dir(dst), 0777)

	dstfile, err := os.Create(dst)
	if err != nil {
		return
	}
	_, err = io.Copy(dstfile, srcfile)
	if err != nil {
		return
	}

	err = srcfile.Close()
	err2 := dstfile.Close()

	if err != nil {
		return
	}

	if err2 != nil {
		return err2
	}

	return nil
}

func (runner *ContainerRunner) SetupMounts() (err error) {
	err = runner.SetupArvMountPoint("keep")
	if err != nil {
		return fmt.Errorf("While creating keep mount temp dir: %v", err)
	}

	token, err := runner.ContainerToken()
	if err != nil {
		return fmt.Errorf("could not get container token: %s", err)
	}

	pdhOnly := true
	tmpcount := 0
	arvMountCmd := []string{
		"--foreground",
		"--allow-other",
		"--read-write",
		fmt.Sprintf("--crunchstat-interval=%v", runner.statInterval.Seconds())}

	if runner.Container.RuntimeConstraints.KeepCacheRAM > 0 {
		arvMountCmd = append(arvMountCmd, "--file-cache", fmt.Sprintf("%d", runner.Container.RuntimeConstraints.KeepCacheRAM))
	}

	collectionPaths := []string{}
	runner.Binds = nil
	runner.Volumes = make(map[string]struct{})
	needCertMount := true
	type copyFile struct {
		src  string
		bind string
	}
	var copyFiles []copyFile

	var binds []string
	for bind := range runner.Container.Mounts {
		binds = append(binds, bind)
	}
	sort.Strings(binds)

	for _, bind := range binds {
		mnt := runner.Container.Mounts[bind]
		if bind == "stdout" || bind == "stderr" {
			// Is it a "file" mount kind?
			if mnt.Kind != "file" {
				return fmt.Errorf("Unsupported mount kind '%s' for %s. Only 'file' is supported.", mnt.Kind, bind)
			}

			// Does path start with OutputPath?
			prefix := runner.Container.OutputPath
			if !strings.HasSuffix(prefix, "/") {
				prefix += "/"
			}
			if !strings.HasPrefix(mnt.Path, prefix) {
				return fmt.Errorf("%s path does not start with OutputPath: %s, %s", strings.Title(bind), mnt.Path, prefix)
			}
		}

		if bind == "stdin" {
			// Is it a "collection" mount kind?
			if mnt.Kind != "collection" && mnt.Kind != "json" {
				return fmt.Errorf("Unsupported mount kind '%s' for stdin. Only 'collection' or 'json' are supported.", mnt.Kind)
			}
		}

		if bind == "/etc/arvados/ca-certificates.crt" {
			needCertMount = false
		}

		if strings.HasPrefix(bind, runner.Container.OutputPath+"/") && bind != runner.Container.OutputPath+"/" {
			if mnt.Kind != "collection" {
				return fmt.Errorf("Only mount points of kind 'collection' are supported underneath the output_path: %v", bind)
			}
		}

		switch {
		case mnt.Kind == "collection" && bind != "stdin":
			var src string
			if mnt.UUID != "" && mnt.PortableDataHash != "" {
				return fmt.Errorf("Cannot specify both 'uuid' and 'portable_data_hash' for a collection mount")
			}
			if mnt.UUID != "" {
				if mnt.Writable {
					return fmt.Errorf("Writing to existing collections currently not permitted.")
				}
				pdhOnly = false
				src = fmt.Sprintf("%s/by_id/%s", runner.ArvMountPoint, mnt.UUID)
			} else if mnt.PortableDataHash != "" {
				if mnt.Writable && !strings.HasPrefix(bind, runner.Container.OutputPath+"/") {
					return fmt.Errorf("Can never write to a collection specified by portable data hash")
				}
				idx := strings.Index(mnt.PortableDataHash, "/")
				if idx > 0 {
					mnt.Path = path.Clean(mnt.PortableDataHash[idx:])
					mnt.PortableDataHash = mnt.PortableDataHash[0:idx]
					runner.Container.Mounts[bind] = mnt
				}
				src = fmt.Sprintf("%s/by_id/%s", runner.ArvMountPoint, mnt.PortableDataHash)
				if mnt.Path != "" && mnt.Path != "." {
					if strings.HasPrefix(mnt.Path, "./") {
						mnt.Path = mnt.Path[2:]
					} else if strings.HasPrefix(mnt.Path, "/") {
						mnt.Path = mnt.Path[1:]
					}
					src += "/" + mnt.Path
				}
			} else {
				src = fmt.Sprintf("%s/tmp%d", runner.ArvMountPoint, tmpcount)
				arvMountCmd = append(arvMountCmd, "--mount-tmp")
				arvMountCmd = append(arvMountCmd, fmt.Sprintf("tmp%d", tmpcount))
				tmpcount += 1
			}
			if mnt.Writable {
				if bind == runner.Container.OutputPath {
					runner.HostOutputDir = src
					runner.Binds = append(runner.Binds, fmt.Sprintf("%s:%s", src, bind))
				} else if strings.HasPrefix(bind, runner.Container.OutputPath+"/") {
					copyFiles = append(copyFiles, copyFile{src, runner.HostOutputDir + bind[len(runner.Container.OutputPath):]})
				} else {
					runner.Binds = append(runner.Binds, fmt.Sprintf("%s:%s", src, bind))
				}
			} else {
				runner.Binds = append(runner.Binds, fmt.Sprintf("%s:%s:ro", src, bind))
			}
			collectionPaths = append(collectionPaths, src)

		case mnt.Kind == "tmp":
			var tmpdir string
			tmpdir, err = runner.MkTempDir(runner.parentTemp, "tmp")
			if err != nil {
				return fmt.Errorf("While creating mount temp dir: %v", err)
			}
			st, staterr := os.Stat(tmpdir)
			if staterr != nil {
				return fmt.Errorf("While Stat on temp dir: %v", staterr)
			}
			err = os.Chmod(tmpdir, st.Mode()|os.ModeSetgid|0777)
			if staterr != nil {
				return fmt.Errorf("While Chmod temp dir: %v", err)
			}
			runner.Binds = append(runner.Binds, fmt.Sprintf("%s:%s", tmpdir, bind))
			if bind == runner.Container.OutputPath {
				runner.HostOutputDir = tmpdir
			}

		case mnt.Kind == "json":
			jsondata, err := json.Marshal(mnt.Content)
			if err != nil {
				return fmt.Errorf("encoding json data: %v", err)
			}
			// Create a tempdir with a single file
			// (instead of just a tempfile): this way we
			// can ensure the file is world-readable
			// inside the container, without having to
			// make it world-readable on the docker host.
			tmpdir, err := runner.MkTempDir(runner.parentTemp, "json")
			if err != nil {
				return fmt.Errorf("creating temp dir: %v", err)
			}
			tmpfn := filepath.Join(tmpdir, "mountdata.json")
			err = ioutil.WriteFile(tmpfn, jsondata, 0644)
			if err != nil {
				return fmt.Errorf("writing temp file: %v", err)
			}
			runner.Binds = append(runner.Binds, fmt.Sprintf("%s:%s:ro", tmpfn, bind))

		case mnt.Kind == "git_tree":
			tmpdir, err := runner.MkTempDir(runner.parentTemp, "git_tree")
			if err != nil {
				return fmt.Errorf("creating temp dir: %v", err)
			}
			err = gitMount(mnt).extractTree(runner.ArvClient, tmpdir, token)
			if err != nil {
				return err
			}
			runner.Binds = append(runner.Binds, tmpdir+":"+bind+":ro")
		}
	}

	if runner.HostOutputDir == "" {
		return fmt.Errorf("Output path does not correspond to a writable mount point")
	}

	if wantAPI := runner.Container.RuntimeConstraints.API; needCertMount && wantAPI != nil && *wantAPI {
		for _, certfile := range arvadosclient.CertFiles {
			_, err := os.Stat(certfile)
			if err == nil {
				runner.Binds = append(runner.Binds, fmt.Sprintf("%s:/etc/arvados/ca-certificates.crt:ro", certfile))
				break
			}
		}
	}

	if pdhOnly {
		arvMountCmd = append(arvMountCmd, "--mount-by-pdh", "by_id")
	} else {
		arvMountCmd = append(arvMountCmd, "--mount-by-id", "by_id")
	}
	arvMountCmd = append(arvMountCmd, runner.ArvMountPoint)

	runner.ArvMount, err = runner.RunArvMount(arvMountCmd, token)
	if err != nil {
		return fmt.Errorf("While trying to start arv-mount: %v", err)
	}

	for _, p := range collectionPaths {
		_, err = os.Stat(p)
		if err != nil {
			return fmt.Errorf("While checking that input files exist: %v", err)
		}
	}

	for _, cp := range copyFiles {
		dir, err := os.Stat(cp.src)
		if err != nil {
			return fmt.Errorf("While staging writable file from %q to %q: %v", cp.src, cp.bind, err)
		}
		if dir.IsDir() {
			err = filepath.Walk(cp.src, func(walkpath string, walkinfo os.FileInfo, walkerr error) error {
				if walkerr != nil {
					return walkerr
				}
				if walkinfo.Mode().IsRegular() {
					return copyfile(walkpath, path.Join(cp.bind, walkpath[len(cp.src):]))
				} else if walkinfo.Mode().IsDir() {
					return os.MkdirAll(path.Join(cp.bind, walkpath[len(cp.src):]), 0777)
				} else {
					return fmt.Errorf("Source %q is not a regular file or directory", cp.src)
				}
			})
		} else {
			err = copyfile(cp.src, cp.bind)
		}
		if err != nil {
			return fmt.Errorf("While staging writable file from %q to %q: %v", cp.src, cp.bind, err)
		}
	}

	return nil
}

func (runner *ContainerRunner) ProcessDockerAttach(containerReader io.Reader) {
	// Handle docker log protocol
	// https://docs.docker.com/engine/reference/api/docker_remote_api_v1.15/#attach-to-a-container
	defer close(runner.loggingDone)

	header := make([]byte, 8)
	var err error
	for err == nil {
		_, err = io.ReadAtLeast(containerReader, header, 8)
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			break
		}
		readsize := int64(header[7]) | (int64(header[6]) << 8) | (int64(header[5]) << 16) | (int64(header[4]) << 24)
		if header[0] == 1 {
			// stdout
			_, err = io.CopyN(runner.Stdout, containerReader, readsize)
		} else {
			// stderr
			_, err = io.CopyN(runner.Stderr, containerReader, readsize)
		}
	}

	if err != nil {
		runner.CrunchLog.Printf("error reading docker logs: %v", err)
	}

	err = runner.Stdout.Close()
	if err != nil {
		runner.CrunchLog.Printf("error closing stdout logs: %v", err)
	}

	err = runner.Stderr.Close()
	if err != nil {
		runner.CrunchLog.Printf("error closing stderr logs: %v", err)
	}

	if runner.statReporter != nil {
		runner.statReporter.Stop()
		err = runner.statLogger.Close()
		if err != nil {
			runner.CrunchLog.Printf("error closing crunchstat logs: %v", err)
		}
	}
}

func (runner *ContainerRunner) stopHoststat() error {
	if runner.hoststatReporter == nil {
		return nil
	}
	runner.hoststatReporter.Stop()
	err := runner.hoststatLogger.Close()
	if err != nil {
		return fmt.Errorf("error closing hoststat logs: %v", err)
	}
	return nil
}

func (runner *ContainerRunner) startHoststat() {
	runner.hoststatLogger = NewThrottledLogger(runner.NewLogWriter("hoststat"))
	runner.hoststatReporter = &crunchstat.Reporter{
		Logger:     log.New(runner.hoststatLogger, "", 0),
		CgroupRoot: runner.cgroupRoot,
		PollPeriod: runner.statInterval,
	}
	runner.hoststatReporter.Start()
}

func (runner *ContainerRunner) startCrunchstat() {
	runner.statLogger = NewThrottledLogger(runner.NewLogWriter("crunchstat"))
	runner.statReporter = &crunchstat.Reporter{
		CID:          runner.ContainerID,
		Logger:       log.New(runner.statLogger, "", 0),
		CgroupParent: runner.expectCgroupParent,
		CgroupRoot:   runner.cgroupRoot,
		PollPeriod:   runner.statInterval,
	}
	runner.statReporter.Start()
}

type infoCommand struct {
	label string
	cmd   []string
}

// LogHostInfo logs info about the current host, for debugging and
// accounting purposes. Although it's logged as "node-info", this is
// about the environment where crunch-run is actually running, which
// might differ from what's described in the node record (see
// LogNodeRecord).
func (runner *ContainerRunner) LogHostInfo() (err error) {
	w := runner.NewLogWriter("node-info")

	commands := []infoCommand{
		{
			label: "Host Information",
			cmd:   []string{"uname", "-a"},
		},
		{
			label: "CPU Information",
			cmd:   []string{"cat", "/proc/cpuinfo"},
		},
		{
			label: "Memory Information",
			cmd:   []string{"cat", "/proc/meminfo"},
		},
		{
			label: "Disk Space",
			cmd:   []string{"df", "-m", "/", os.TempDir()},
		},
		{
			label: "Disk INodes",
			cmd:   []string{"df", "-i", "/", os.TempDir()},
		},
	}

	// Run commands with informational output to be logged.
	for _, command := range commands {
		fmt.Fprintln(w, command.label)
		cmd := exec.Command(command.cmd[0], command.cmd[1:]...)
		cmd.Stdout = w
		cmd.Stderr = w
		if err := cmd.Run(); err != nil {
			err = fmt.Errorf("While running command %q: %v", command.cmd, err)
			fmt.Fprintln(w, err)
			return err
		}
		fmt.Fprintln(w, "")
	}

	err = w.Close()
	if err != nil {
		return fmt.Errorf("While closing node-info logs: %v", err)
	}
	return nil
}

// LogContainerRecord gets and saves the raw JSON container record from the API server
func (runner *ContainerRunner) LogContainerRecord() error {
	logged, err := runner.logAPIResponse("container", "containers", map[string]interface{}{"filters": [][]string{{"uuid", "=", runner.Container.UUID}}}, nil)
	if !logged && err == nil {
		err = fmt.Errorf("error: no container record found for %s", runner.Container.UUID)
	}
	return err
}

// LogNodeRecord logs arvados#node record corresponding to the current host.
func (runner *ContainerRunner) LogNodeRecord() error {
	hostname := os.Getenv("SLURMD_NODENAME")
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	_, err := runner.logAPIResponse("node", "nodes", map[string]interface{}{"filters": [][]string{{"hostname", "=", hostname}}}, func(resp interface{}) {
		// The "info" field has admin-only info when obtained
		// with a privileged token, and should not be logged.
		node, ok := resp.(map[string]interface{})
		if ok {
			delete(node, "info")
		}
	})
	return err
}

func (runner *ContainerRunner) logAPIResponse(label, path string, params map[string]interface{}, munge func(interface{})) (logged bool, err error) {
	w := &ArvLogWriter{
		ArvClient:     runner.ArvClient,
		UUID:          runner.Container.UUID,
		loggingStream: label,
		writeCloser:   runner.LogCollection.Open(label + ".json"),
	}

	reader, err := runner.ArvClient.CallRaw("GET", path, "", "", arvadosclient.Dict(params))
	if err != nil {
		return false, fmt.Errorf("error getting %s record: %v", label, err)
	}
	defer reader.Close()

	dec := json.NewDecoder(reader)
	dec.UseNumber()
	var resp map[string]interface{}
	if err = dec.Decode(&resp); err != nil {
		return false, fmt.Errorf("error decoding %s list response: %v", label, err)
	}
	items, ok := resp["items"].([]interface{})
	if !ok {
		return false, fmt.Errorf("error decoding %s list response: no \"items\" key in API list response", label)
	} else if len(items) < 1 {
		return false, nil
	}
	if munge != nil {
		munge(items[0])
	}
	// Re-encode it using indentation to improve readability
	enc := json.NewEncoder(w)
	enc.SetIndent("", "    ")
	if err = enc.Encode(items[0]); err != nil {
		return false, fmt.Errorf("error logging %s record: %v", label, err)
	}
	err = w.Close()
	if err != nil {
		return false, fmt.Errorf("error closing %s.json in log collection: %v", label, err)
	}
	return true, nil
}

// AttachStreams connects the docker container stdin, stdout and stderr logs
// to the Arvados logger which logs to Keep and the API server logs table.
func (runner *ContainerRunner) AttachStreams() (err error) {

	runner.CrunchLog.Print("Attaching container streams")

	// If stdin mount is provided, attach it to the docker container
	var stdinRdr arvados.File
	var stdinJson []byte
	if stdinMnt, ok := runner.Container.Mounts["stdin"]; ok {
		if stdinMnt.Kind == "collection" {
			var stdinColl arvados.Collection
			collId := stdinMnt.UUID
			if collId == "" {
				collId = stdinMnt.PortableDataHash
			}
			err = runner.ArvClient.Get("collections", collId, nil, &stdinColl)
			if err != nil {
				return fmt.Errorf("While getting stding collection: %v", err)
			}

			stdinRdr, err = runner.Kc.ManifestFileReader(manifest.Manifest{Text: stdinColl.ManifestText}, stdinMnt.Path)
			if os.IsNotExist(err) {
				return fmt.Errorf("stdin collection path not found: %v", stdinMnt.Path)
			} else if err != nil {
				return fmt.Errorf("While getting stdin collection path %v: %v", stdinMnt.Path, err)
			}
		} else if stdinMnt.Kind == "json" {
			stdinJson, err = json.Marshal(stdinMnt.Content)
			if err != nil {
				return fmt.Errorf("While encoding stdin json data: %v", err)
			}
		}
	}

	stdinUsed := stdinRdr != nil || len(stdinJson) != 0
	response, err := runner.Docker.ContainerAttach(context.TODO(), runner.ContainerID,
		dockertypes.ContainerAttachOptions{Stream: true, Stdin: stdinUsed, Stdout: true, Stderr: true})
	if err != nil {
		return fmt.Errorf("While attaching container stdout/stderr streams: %v", err)
	}

	runner.loggingDone = make(chan bool)

	if stdoutMnt, ok := runner.Container.Mounts["stdout"]; ok {
		stdoutFile, err := runner.getStdoutFile(stdoutMnt.Path)
		if err != nil {
			return err
		}
		runner.Stdout = stdoutFile
	} else {
		runner.Stdout = NewThrottledLogger(runner.NewLogWriter("stdout"))
	}

	if stderrMnt, ok := runner.Container.Mounts["stderr"]; ok {
		stderrFile, err := runner.getStdoutFile(stderrMnt.Path)
		if err != nil {
			return err
		}
		runner.Stderr = stderrFile
	} else {
		runner.Stderr = NewThrottledLogger(runner.NewLogWriter("stderr"))
	}

	if stdinRdr != nil {
		go func() {
			_, err := io.Copy(response.Conn, stdinRdr)
			if err != nil {
				runner.CrunchLog.Print("While writing stdin collection to docker container %q", err)
				runner.stop()
			}
			stdinRdr.Close()
			response.CloseWrite()
		}()
	} else if len(stdinJson) != 0 {
		go func() {
			_, err := io.Copy(response.Conn, bytes.NewReader(stdinJson))
			if err != nil {
				runner.CrunchLog.Print("While writing stdin json to docker container %q", err)
				runner.stop()
			}
			response.CloseWrite()
		}()
	}

	go runner.ProcessDockerAttach(response.Reader)

	return nil
}

func (runner *ContainerRunner) getStdoutFile(mntPath string) (*os.File, error) {
	stdoutPath := mntPath[len(runner.Container.OutputPath):]
	index := strings.LastIndex(stdoutPath, "/")
	if index > 0 {
		subdirs := stdoutPath[:index]
		if subdirs != "" {
			st, err := os.Stat(runner.HostOutputDir)
			if err != nil {
				return nil, fmt.Errorf("While Stat on temp dir: %v", err)
			}
			stdoutPath := filepath.Join(runner.HostOutputDir, subdirs)
			err = os.MkdirAll(stdoutPath, st.Mode()|os.ModeSetgid|0777)
			if err != nil {
				return nil, fmt.Errorf("While MkdirAll %q: %v", stdoutPath, err)
			}
		}
	}
	stdoutFile, err := os.Create(filepath.Join(runner.HostOutputDir, stdoutPath))
	if err != nil {
		return nil, fmt.Errorf("While creating file %q: %v", stdoutPath, err)
	}

	return stdoutFile, nil
}

// CreateContainer creates the docker container.
func (runner *ContainerRunner) CreateContainer() error {
	runner.CrunchLog.Print("Creating Docker container")

	runner.ContainerConfig.Cmd = runner.Container.Command
	if runner.Container.Cwd != "." {
		runner.ContainerConfig.WorkingDir = runner.Container.Cwd
	}

	for k, v := range runner.Container.Environment {
		runner.ContainerConfig.Env = append(runner.ContainerConfig.Env, k+"="+v)
	}

	runner.ContainerConfig.Volumes = runner.Volumes

	runner.HostConfig = dockercontainer.HostConfig{
		Binds: runner.Binds,
		LogConfig: dockercontainer.LogConfig{
			Type: "none",
		},
		Resources: dockercontainer.Resources{
			CgroupParent: runner.setCgroupParent,
		},
	}

	if wantAPI := runner.Container.RuntimeConstraints.API; wantAPI != nil && *wantAPI {
		tok, err := runner.ContainerToken()
		if err != nil {
			return err
		}
		runner.ContainerConfig.Env = append(runner.ContainerConfig.Env,
			"ARVADOS_API_TOKEN="+tok,
			"ARVADOS_API_HOST="+os.Getenv("ARVADOS_API_HOST"),
			"ARVADOS_API_HOST_INSECURE="+os.Getenv("ARVADOS_API_HOST_INSECURE"),
		)
		runner.HostConfig.NetworkMode = dockercontainer.NetworkMode(runner.networkMode)
	} else {
		if runner.enableNetwork == "always" {
			runner.HostConfig.NetworkMode = dockercontainer.NetworkMode(runner.networkMode)
		} else {
			runner.HostConfig.NetworkMode = dockercontainer.NetworkMode("none")
		}
	}

	_, stdinUsed := runner.Container.Mounts["stdin"]
	runner.ContainerConfig.OpenStdin = stdinUsed
	runner.ContainerConfig.StdinOnce = stdinUsed
	runner.ContainerConfig.AttachStdin = stdinUsed
	runner.ContainerConfig.AttachStdout = true
	runner.ContainerConfig.AttachStderr = true

	createdBody, err := runner.Docker.ContainerCreate(context.TODO(), &runner.ContainerConfig, &runner.HostConfig, nil, runner.Container.UUID)
	if err != nil {
		return fmt.Errorf("While creating container: %v", err)
	}

	runner.ContainerID = createdBody.ID

	return runner.AttachStreams()
}

// StartContainer starts the docker container created by CreateContainer.
func (runner *ContainerRunner) StartContainer() error {
	runner.CrunchLog.Printf("Starting Docker container id '%s'", runner.ContainerID)
	runner.cStateLock.Lock()
	defer runner.cStateLock.Unlock()
	if runner.cCancelled {
		return ErrCancelled
	}
	err := runner.Docker.ContainerStart(context.TODO(), runner.ContainerID,
		dockertypes.ContainerStartOptions{})
	if err != nil {
		var advice string
		if m, e := regexp.MatchString("(?ms).*(exec|System error).*(no such file or directory|file not found).*", err.Error()); m && e == nil {
			advice = fmt.Sprintf("\nPossible causes: command %q is missing, the interpreter given in #! is missing, or script has Windows line endings.", runner.Container.Command[0])
		}
		return fmt.Errorf("could not start container: %v%s", err, advice)
	}
	return nil
}

// WaitFinish waits for the container to terminate, capture the exit code, and
// close the stdout/stderr logging.
func (runner *ContainerRunner) WaitFinish() error {
	runner.CrunchLog.Print("Waiting for container to finish")

	waitOk, waitErr := runner.Docker.ContainerWait(context.TODO(), runner.ContainerID, dockercontainer.WaitConditionNotRunning)
	arvMountExit := runner.ArvMountExit
	for {
		select {
		case waitBody := <-waitOk:
			runner.CrunchLog.Printf("Container exited with code: %v", waitBody.StatusCode)
			code := int(waitBody.StatusCode)
			runner.ExitCode = &code

			// wait for stdout/stderr to complete
			<-runner.loggingDone
			return nil

		case err := <-waitErr:
			return fmt.Errorf("container wait: %v", err)

		case <-arvMountExit:
			runner.CrunchLog.Printf("arv-mount exited while container is still running.  Stopping container.")
			runner.stop()
			// arvMountExit will always be ready now that
			// it's closed, but that doesn't interest us.
			arvMountExit = nil
		}
	}
}

var ErrNotInOutputDir = fmt.Errorf("Must point to path within the output directory")

func (runner *ContainerRunner) derefOutputSymlink(path string, startinfo os.FileInfo) (tgt string, readlinktgt string, info os.FileInfo, err error) {
	// Follow symlinks if necessary
	info = startinfo
	tgt = path
	readlinktgt = ""
	nextlink := path
	for followed := 0; info.Mode()&os.ModeSymlink != 0; followed++ {
		if followed >= limitFollowSymlinks {
			// Got stuck in a loop or just a pathological number of links, give up.
			err = fmt.Errorf("Followed more than %v symlinks from path %q", limitFollowSymlinks, path)
			return
		}

		readlinktgt, err = os.Readlink(nextlink)
		if err != nil {
			return
		}

		tgt = readlinktgt
		if !strings.HasPrefix(tgt, "/") {
			// Relative symlink, resolve it to host path
			tgt = filepath.Join(filepath.Dir(path), tgt)
		}
		if strings.HasPrefix(tgt, runner.Container.OutputPath+"/") && !strings.HasPrefix(tgt, runner.HostOutputDir+"/") {
			// Absolute symlink to container output path, adjust it to host output path.
			tgt = filepath.Join(runner.HostOutputDir, tgt[len(runner.Container.OutputPath):])
		}
		if !strings.HasPrefix(tgt, runner.HostOutputDir+"/") {
			// After dereferencing, symlink target must either be
			// within output directory, or must point to a
			// collection mount.
			err = ErrNotInOutputDir
			return
		}

		info, err = os.Lstat(tgt)
		if err != nil {
			// tgt
			err = fmt.Errorf("Symlink in output %q points to invalid location %q: %v",
				path[len(runner.HostOutputDir):], readlinktgt, err)
			return
		}

		nextlink = tgt
	}

	return
}

var limitFollowSymlinks = 10

// UploadFile uploads files within the output directory, with special handling
// for symlinks. If the symlink leads to a keep mount, copy the manifest text
// from the keep mount into the output manifestText.  Ensure that whether
// symlinks are relative or absolute, every symlink target (even targets that
// are symlinks themselves) must point to a path in either the output directory
// or a collection mount.
//
// Assumes initial value of "path" is absolute, and located within runner.HostOutputDir.
func (runner *ContainerRunner) UploadOutputFile(
	path string,
	info os.FileInfo,
	infoerr error,
	binds []string,
	walkUpload *WalkUpload,
	relocateFrom string,
	relocateTo string,
	followed int) (manifestText string, err error) {

	if infoerr != nil {
		return "", infoerr
	}

	if info.Mode().IsDir() {
		// if empty, need to create a .keep file
		dir, direrr := os.Open(path)
		if direrr != nil {
			return "", direrr
		}
		defer dir.Close()
		names, eof := dir.Readdirnames(1)
		if len(names) == 0 && eof == io.EOF && path != runner.HostOutputDir {
			containerPath := runner.OutputPath + path[len(runner.HostOutputDir):]
			for _, bind := range binds {
				mnt := runner.Container.Mounts[bind]
				// Check if there is a bind for this
				// directory, in which case assume we don't need .keep
				if (containerPath == bind || strings.HasPrefix(containerPath, bind+"/")) && mnt.PortableDataHash != "d41d8cd98f00b204e9800998ecf8427e+0" {
					return
				}
			}
			outputSuffix := path[len(runner.HostOutputDir)+1:]
			return fmt.Sprintf("./%v d41d8cd98f00b204e9800998ecf8427e+0 0:0:.keep\n", outputSuffix), nil
		}
		return
	}

	if followed >= limitFollowSymlinks {
		// Got stuck in a loop or just a pathological number of
		// directory links, give up.
		err = fmt.Errorf("Followed more than %v symlinks from path %q", limitFollowSymlinks, path)
		return
	}

	// "path" is the actual path we are visiting
	// "tgt" is the target of "path" (a non-symlink) after following symlinks
	// "relocated" is the path in the output manifest where the file should be placed,
	// but has HostOutputDir as a prefix.

	// The destination path in the output manifest may need to be
	// logically relocated to some other path in order to appear
	// in the correct location as a result of following a symlink.
	// Remove the relocateFrom prefix and replace it with
	// relocateTo.
	relocated := relocateTo + path[len(relocateFrom):]

	tgt, readlinktgt, info, derefErr := runner.derefOutputSymlink(path, info)
	if derefErr != nil && derefErr != ErrNotInOutputDir {
		return "", derefErr
	}

	// go through mounts and try reverse map to collection reference
	for _, bind := range binds {
		mnt := runner.Container.Mounts[bind]
		if (tgt == bind || strings.HasPrefix(tgt, bind+"/")) && !mnt.Writable {
			// get path relative to bind
			targetSuffix := tgt[len(bind):]

			// Copy mount and adjust the path to add path relative to the bind
			adjustedMount := mnt
			adjustedMount.Path = filepath.Join(adjustedMount.Path, targetSuffix)

			// Terminates in this keep mount, so add the
			// manifest text at appropriate location.
			outputSuffix := relocated[len(runner.HostOutputDir):]
			manifestText, err = runner.getCollectionManifestForPath(adjustedMount, outputSuffix)
			return
		}
	}

	// If target is not a collection mount, it must be located within the
	// output directory, otherwise it is an error.
	if derefErr == ErrNotInOutputDir {
		err = fmt.Errorf("Symlink in output %q points to invalid location %q, must point to path within the output directory.",
			path[len(runner.HostOutputDir):], readlinktgt)
		return
	}

	if info.Mode().IsRegular() {
		return "", walkUpload.UploadFile(relocated, tgt)
	}

	if info.Mode().IsDir() {
		// Symlink leads to directory.  Walk() doesn't follow
		// directory symlinks, so we walk the target directory
		// instead.  Within the walk, file paths are relocated
		// so they appear under the original symlink path.
		err = filepath.Walk(tgt, func(walkpath string, walkinfo os.FileInfo, walkerr error) error {
			var m string
			m, walkerr = runner.UploadOutputFile(walkpath, walkinfo, walkerr,
				binds, walkUpload, tgt, relocated, followed+1)
			if walkerr == nil {
				manifestText = manifestText + m
			}
			return walkerr
		})
		return
	}

	return
}

// HandleOutput sets the output, unmounts the FUSE mount, and deletes temporary directories
func (runner *ContainerRunner) CaptureOutput() error {
	if wantAPI := runner.Container.RuntimeConstraints.API; wantAPI != nil && *wantAPI {
		// Output may have been set directly by the container, so
		// refresh the container record to check.
		err := runner.ArvClient.Get("containers", runner.Container.UUID,
			nil, &runner.Container)
		if err != nil {
			return err
		}
		if runner.Container.Output != "" {
			// Container output is already set.
			runner.OutputPDH = &runner.Container.Output
			return nil
		}
	}

	if runner.HostOutputDir == "" {
		return nil
	}

	_, err := os.Stat(runner.HostOutputDir)
	if err != nil {
		return fmt.Errorf("While checking host output path: %v", err)
	}

	// Pre-populate output from the configured mount points
	var binds []string
	for bind, mnt := range runner.Container.Mounts {
		if mnt.Kind == "collection" {
			binds = append(binds, bind)
		}
	}
	sort.Strings(binds)

	var manifestText string

	collectionMetafile := fmt.Sprintf("%s/.arvados#collection", runner.HostOutputDir)
	_, err = os.Stat(collectionMetafile)
	if err != nil {
		// Regular directory

		cw := CollectionWriter{0, runner.Kc, nil, nil, sync.Mutex{}}
		walkUpload := cw.BeginUpload(runner.HostOutputDir, runner.CrunchLog.Logger)

		var m string
		err = filepath.Walk(runner.HostOutputDir, func(path string, info os.FileInfo, err error) error {
			m, err = runner.UploadOutputFile(path, info, err, binds, walkUpload, "", "", 0)
			if err == nil {
				manifestText = manifestText + m
			}
			return err
		})

		cw.EndUpload(walkUpload)

		if err != nil {
			return fmt.Errorf("While uploading output files: %v", err)
		}

		m, err = cw.ManifestText()
		manifestText = manifestText + m
		if err != nil {
			return fmt.Errorf("While uploading output files: %v", err)
		}
	} else {
		// FUSE mount directory
		file, openerr := os.Open(collectionMetafile)
		if openerr != nil {
			return fmt.Errorf("While opening FUSE metafile: %v", err)
		}
		defer file.Close()

		var rec arvados.Collection
		err = json.NewDecoder(file).Decode(&rec)
		if err != nil {
			return fmt.Errorf("While reading FUSE metafile: %v", err)
		}
		manifestText = rec.ManifestText
	}

	for _, bind := range binds {
		mnt := runner.Container.Mounts[bind]

		bindSuffix := strings.TrimPrefix(bind, runner.Container.OutputPath)

		if bindSuffix == bind || len(bindSuffix) <= 0 {
			// either does not start with OutputPath or is OutputPath itself
			continue
		}

		if mnt.ExcludeFromOutput == true || mnt.Writable {
			continue
		}

		// append to manifest_text
		m, err := runner.getCollectionManifestForPath(mnt, bindSuffix)
		if err != nil {
			return err
		}

		manifestText = manifestText + m
	}

	// Save output
	var response arvados.Collection
	manifest := manifest.Manifest{Text: manifestText}
	manifestText = manifest.Extract(".", ".").Text
	err = runner.ArvClient.Create("collections",
		arvadosclient.Dict{
			"ensure_unique_name": true,
			"collection": arvadosclient.Dict{
				"is_trashed":    true,
				"name":          "output for " + runner.Container.UUID,
				"manifest_text": manifestText}},
		&response)
	if err != nil {
		return fmt.Errorf("While creating output collection: %v", err)
	}
	runner.OutputPDH = &response.PortableDataHash
	return nil
}

var outputCollections = make(map[string]arvados.Collection)

// Fetch the collection for the mnt.PortableDataHash
// Return the manifest_text fragment corresponding to the specified mnt.Path
//  after making any required updates.
//  Ex:
//    If mnt.Path is not specified,
//      return the entire manifest_text after replacing any "." with bindSuffix
//    If mnt.Path corresponds to one stream,
//      return the manifest_text for that stream after replacing that stream name with bindSuffix
//    Otherwise, check if a filename in any one stream is being sought. Return the manifest_text
//      for that stream after replacing stream name with bindSuffix minus the last word
//      and the file name with last word of the bindSuffix
//  Allowed path examples:
//    "path":"/"
//    "path":"/subdir1"
//    "path":"/subdir1/subdir2"
//    "path":"/subdir/filename" etc
func (runner *ContainerRunner) getCollectionManifestForPath(mnt arvados.Mount, bindSuffix string) (string, error) {
	collection := outputCollections[mnt.PortableDataHash]
	if collection.PortableDataHash == "" {
		err := runner.ArvClient.Get("collections", mnt.PortableDataHash, nil, &collection)
		if err != nil {
			return "", fmt.Errorf("While getting collection for %v: %v", mnt.PortableDataHash, err)
		}
		outputCollections[mnt.PortableDataHash] = collection
	}

	if collection.ManifestText == "" {
		runner.CrunchLog.Printf("No manifest text for collection %v", collection.PortableDataHash)
		return "", nil
	}

	mft := manifest.Manifest{Text: collection.ManifestText}
	extracted := mft.Extract(mnt.Path, bindSuffix)
	if extracted.Err != nil {
		return "", fmt.Errorf("Error parsing manifest for %v: %v", mnt.PortableDataHash, extracted.Err.Error())
	}
	return extracted.Text, nil
}

func (runner *ContainerRunner) CleanupDirs() {
	if runner.ArvMount != nil {
		var delay int64 = 8
		umount := exec.Command("arv-mount", fmt.Sprintf("--unmount-timeout=%d", delay), "--unmount", runner.ArvMountPoint)
		umount.Stdout = runner.CrunchLog
		umount.Stderr = runner.CrunchLog
		runner.CrunchLog.Printf("Running %v", umount.Args)
		umnterr := umount.Start()

		if umnterr != nil {
			runner.CrunchLog.Printf("Error unmounting: %v", umnterr)
		} else {
			// If arv-mount --unmount gets stuck for any reason, we
			// don't want to wait for it forever.  Do Wait() in a goroutine
			// so it doesn't block crunch-run.
			umountExit := make(chan error)
			go func() {
				mnterr := umount.Wait()
				if mnterr != nil {
					runner.CrunchLog.Printf("Error unmounting: %v", mnterr)
				}
				umountExit <- mnterr
			}()

			for again := true; again; {
				again = false
				select {
				case <-umountExit:
					umount = nil
					again = true
				case <-runner.ArvMountExit:
					break
				case <-time.After(time.Duration((delay + 1) * int64(time.Second))):
					runner.CrunchLog.Printf("Timed out waiting for unmount")
					if umount != nil {
						umount.Process.Kill()
					}
					runner.ArvMount.Process.Kill()
				}
			}
		}
	}

	if runner.ArvMountPoint != "" {
		if rmerr := os.Remove(runner.ArvMountPoint); rmerr != nil {
			runner.CrunchLog.Printf("While cleaning up arv-mount directory %s: %v", runner.ArvMountPoint, rmerr)
		}
	}

	if rmerr := os.RemoveAll(runner.parentTemp); rmerr != nil {
		runner.CrunchLog.Printf("While cleaning up temporary directory %s: %v", runner.parentTemp, rmerr)
	}
}

// CommitLogs posts the collection containing the final container logs.
func (runner *ContainerRunner) CommitLogs() error {
	runner.CrunchLog.Print(runner.finalState)

	if runner.arvMountLog != nil {
		runner.arvMountLog.Close()
	}
	runner.CrunchLog.Close()

	// Closing CrunchLog above allows them to be committed to Keep at this
	// point, but re-open crunch log with ArvClient in case there are any
	// other further errors (such as failing to write the log to Keep!)
	// while shutting down
	runner.CrunchLog = NewThrottledLogger(&ArvLogWriter{ArvClient: runner.ArvClient,
		UUID: runner.Container.UUID, loggingStream: "crunch-run", writeCloser: nil})
	runner.CrunchLog.Immediate = log.New(os.Stderr, runner.Container.UUID+" ", 0)

	if runner.LogsPDH != nil {
		// If we have already assigned something to LogsPDH,
		// we must be closing the re-opened log, which won't
		// end up getting attached to the container record and
		// therefore doesn't need to be saved as a collection
		// -- it exists only to send logs to other channels.
		return nil
	}

	mt, err := runner.LogCollection.ManifestText()
	if err != nil {
		return fmt.Errorf("While creating log manifest: %v", err)
	}

	var response arvados.Collection
	err = runner.ArvClient.Create("collections",
		arvadosclient.Dict{
			"ensure_unique_name": true,
			"collection": arvadosclient.Dict{
				"is_trashed":    true,
				"name":          "logs for " + runner.Container.UUID,
				"manifest_text": mt}},
		&response)
	if err != nil {
		return fmt.Errorf("While creating log collection: %v", err)
	}
	runner.LogsPDH = &response.PortableDataHash
	return nil
}

// UpdateContainerRunning updates the container state to "Running"
func (runner *ContainerRunner) UpdateContainerRunning() error {
	runner.cStateLock.Lock()
	defer runner.cStateLock.Unlock()
	if runner.cCancelled {
		return ErrCancelled
	}
	return runner.ArvClient.Update("containers", runner.Container.UUID,
		arvadosclient.Dict{"container": arvadosclient.Dict{"state": "Running"}}, nil)
}

// ContainerToken returns the api_token the container (and any
// arv-mount processes) are allowed to use.
func (runner *ContainerRunner) ContainerToken() (string, error) {
	if runner.token != "" {
		return runner.token, nil
	}

	var auth arvados.APIClientAuthorization
	err := runner.ArvClient.Call("GET", "containers", runner.Container.UUID, "auth", nil, &auth)
	if err != nil {
		return "", err
	}
	runner.token = auth.APIToken
	return runner.token, nil
}

// UpdateContainerComplete updates the container record state on API
// server to "Complete" or "Cancelled"
func (runner *ContainerRunner) UpdateContainerFinal() error {
	update := arvadosclient.Dict{}
	update["state"] = runner.finalState
	if runner.LogsPDH != nil {
		update["log"] = *runner.LogsPDH
	}
	if runner.finalState == "Complete" {
		if runner.ExitCode != nil {
			update["exit_code"] = *runner.ExitCode
		}
		if runner.OutputPDH != nil {
			update["output"] = *runner.OutputPDH
		}
	}
	return runner.ArvClient.Update("containers", runner.Container.UUID, arvadosclient.Dict{"container": update}, nil)
}

// IsCancelled returns the value of Cancelled, with goroutine safety.
func (runner *ContainerRunner) IsCancelled() bool {
	runner.cStateLock.Lock()
	defer runner.cStateLock.Unlock()
	return runner.cCancelled
}

// NewArvLogWriter creates an ArvLogWriter
func (runner *ContainerRunner) NewArvLogWriter(name string) io.WriteCloser {
	return &ArvLogWriter{
		ArvClient:     runner.ArvClient,
		UUID:          runner.Container.UUID,
		loggingStream: name,
		writeCloser:   runner.LogCollection.Open(name + ".txt")}
}

// Run the full container lifecycle.
func (runner *ContainerRunner) Run() (err error) {
	runner.CrunchLog.Printf("crunch-run %s started", version)
	runner.CrunchLog.Printf("Executing container '%s'", runner.Container.UUID)

	hostname, hosterr := os.Hostname()
	if hosterr != nil {
		runner.CrunchLog.Printf("Error getting hostname '%v'", hosterr)
	} else {
		runner.CrunchLog.Printf("Executing on host '%s'", hostname)
	}

	runner.finalState = "Queued"

	defer func() {
		runner.stopSignals()
		runner.CleanupDirs()

		runner.CrunchLog.Printf("crunch-run finished")
		runner.CrunchLog.Close()
	}()

	defer func() {
		// checkErr prints e (unless it's nil) and sets err to
		// e (unless err is already non-nil). Thus, if err
		// hasn't already been assigned when Run() returns,
		// this cleanup func will cause Run() to return the
		// first non-nil error that is passed to checkErr().
		checkErr := func(e error) {
			if e == nil {
				return
			}
			runner.CrunchLog.Print(e)
			if err == nil {
				err = e
			}
			if runner.finalState == "Complete" {
				// There was an error in the finalization.
				runner.finalState = "Cancelled"
			}
		}

		// Log the error encountered in Run(), if any
		checkErr(err)

		if runner.finalState == "Queued" {
			runner.UpdateContainerFinal()
			return
		}

		if runner.IsCancelled() {
			runner.finalState = "Cancelled"
			// but don't return yet -- we still want to
			// capture partial output and write logs
		}

		checkErr(runner.CaptureOutput())
		checkErr(runner.stopHoststat())
		checkErr(runner.CommitLogs())
		checkErr(runner.UpdateContainerFinal())
	}()

	err = runner.fetchContainerRecord()
	if err != nil {
		return
	}
	runner.setupSignals()
	runner.startHoststat()

	// check for and/or load image
	err = runner.LoadImage()
	if err != nil {
		if !runner.checkBrokenNode(err) {
			// Failed to load image but not due to a "broken node"
			// condition, probably user error.
			runner.finalState = "Cancelled"
		}
		err = fmt.Errorf("While loading container image: %v", err)
		return
	}

	// set up FUSE mount and binds
	err = runner.SetupMounts()
	if err != nil {
		runner.finalState = "Cancelled"
		err = fmt.Errorf("While setting up mounts: %v", err)
		return
	}

	err = runner.CreateContainer()
	if err != nil {
		return
	}
	err = runner.LogHostInfo()
	if err != nil {
		return
	}
	err = runner.LogNodeRecord()
	if err != nil {
		return
	}
	err = runner.LogContainerRecord()
	if err != nil {
		return
	}

	if runner.IsCancelled() {
		return
	}

	err = runner.UpdateContainerRunning()
	if err != nil {
		return
	}
	runner.finalState = "Cancelled"

	runner.startCrunchstat()

	err = runner.StartContainer()
	if err != nil {
		runner.checkBrokenNode(err)
		return
	}

	err = runner.WaitFinish()
	if err == nil && !runner.IsCancelled() {
		runner.finalState = "Complete"
	}
	return
}

// Fetch the current container record (uuid = runner.Container.UUID)
// into runner.Container.
func (runner *ContainerRunner) fetchContainerRecord() error {
	reader, err := runner.ArvClient.CallRaw("GET", "containers", runner.Container.UUID, "", nil)
	if err != nil {
		return fmt.Errorf("error fetching container record: %v", err)
	}
	defer reader.Close()

	dec := json.NewDecoder(reader)
	dec.UseNumber()
	err = dec.Decode(&runner.Container)
	if err != nil {
		return fmt.Errorf("error decoding container record: %v", err)
	}
	return nil
}

// NewContainerRunner creates a new container runner.
func NewContainerRunner(api IArvadosClient,
	kc IKeepClient,
	docker ThinDockerClient,
	containerUUID string) *ContainerRunner {

	cr := &ContainerRunner{ArvClient: api, Kc: kc, Docker: docker}
	cr.NewLogWriter = cr.NewArvLogWriter
	cr.RunArvMount = cr.ArvMountCmd
	cr.MkTempDir = ioutil.TempDir
	cr.LogCollection = &CollectionWriter{0, kc, nil, nil, sync.Mutex{}}
	cr.Container.UUID = containerUUID
	cr.CrunchLog = NewThrottledLogger(cr.NewLogWriter("crunch-run"))
	cr.CrunchLog.Immediate = log.New(os.Stderr, containerUUID+" ", 0)

	loadLogThrottleParams(api)

	return cr
}

func main() {
	statInterval := flag.Duration("crunchstat-interval", 10*time.Second, "sampling period for periodic resource usage reporting")
	cgroupRoot := flag.String("cgroup-root", "/sys/fs/cgroup", "path to sysfs cgroup tree")
	cgroupParent := flag.String("cgroup-parent", "docker", "name of container's parent cgroup (ignored if -cgroup-parent-subsystem is used)")
	cgroupParentSubsystem := flag.String("cgroup-parent-subsystem", "", "use current cgroup for given subsystem as parent cgroup for container")
	caCertsPath := flag.String("ca-certs", "", "Path to TLS root certificates")
	enableNetwork := flag.String("container-enable-networking", "default",
		`Specify if networking should be enabled for container.  One of 'default', 'always':
    	default: only enable networking if container requests it.
    	always:  containers always have networking enabled
    	`)
	networkMode := flag.String("container-network-mode", "default",
		`Set networking mode for container.  Corresponds to Docker network mode (--net).
    	`)
	memprofile := flag.String("memprofile", "", "write memory profile to `file` after running container")
	getVersion := flag.Bool("version", false, "Print version information and exit.")
	flag.Parse()

	// Print version information if requested
	if *getVersion {
		fmt.Printf("crunch-run %s\n", version)
		return
	}

	log.Printf("crunch-run %s started", version)

	containerId := flag.Arg(0)

	if *caCertsPath != "" {
		arvadosclient.CertFiles = []string{*caCertsPath}
	}

	api, err := arvadosclient.MakeArvadosClient()
	if err != nil {
		log.Fatalf("%s: %v", containerId, err)
	}
	api.Retries = 8

	kc, kcerr := keepclient.MakeKeepClient(api)
	if kcerr != nil {
		log.Fatalf("%s: %v", containerId, kcerr)
	}
	kc.BlockCache = &keepclient.BlockCache{MaxBlocks: 2}
	kc.Retries = 4

	// API version 1.21 corresponds to Docker 1.9, which is currently the
	// minimum version we want to support.
	docker, dockererr := dockerclient.NewClient(dockerclient.DefaultDockerHost, "1.21", nil, nil)

	cr := NewContainerRunner(api, kc, docker, containerId)
	if dockererr != nil {
		cr.CrunchLog.Printf("%s: %v", containerId, dockererr)
		cr.checkBrokenNode(dockererr)
		cr.CrunchLog.Close()
		os.Exit(1)
	}

	parentTemp, tmperr := cr.MkTempDir("", "crunch-run."+containerId+".")
	if tmperr != nil {
		log.Fatalf("%s: %v", containerId, tmperr)
	}

	cr.parentTemp = parentTemp
	cr.statInterval = *statInterval
	cr.cgroupRoot = *cgroupRoot
	cr.expectCgroupParent = *cgroupParent
	cr.enableNetwork = *enableNetwork
	cr.networkMode = *networkMode
	if *cgroupParentSubsystem != "" {
		p := findCgroup(*cgroupParentSubsystem)
		cr.setCgroupParent = p
		cr.expectCgroupParent = p
	}

	runerr := cr.Run()

	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Printf("could not create memory profile: ", err)
		}
		runtime.GC() // get up-to-date statistics
		if err := pprof.WriteHeapProfile(f); err != nil {
			log.Printf("could not write memory profile: ", err)
		}
		closeerr := f.Close()
		if closeerr != nil {
			log.Printf("closing memprofile file: ", err)
		}
	}

	if runerr != nil {
		log.Fatalf("%s: %v", containerId, runerr)
	}
}
