package integration

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containers/common/pkg/cgroups"
	"github.com/containers/podman/v4/libpod/define"
	"github.com/containers/podman/v4/pkg/inspect"
	"github.com/containers/podman/v4/pkg/util"
	. "github.com/containers/podman/v4/test/utils"
	"github.com/containers/storage/pkg/lockfile"
	"github.com/containers/storage/pkg/reexec"
	"github.com/containers/storage/pkg/stringid"
	jsoniter "github.com/json-iterator/go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/onsi/gomega/gexec"
	"github.com/sirupsen/logrus"
)

var (
	//lint:ignore ST1003
	PODMAN_BINARY      string                              //nolint:revive,stylecheck
	INTEGRATION_ROOT   string                              //nolint:revive,stylecheck
	CGROUP_MANAGER     = "systemd"                         //nolint:revive,stylecheck
	RESTORE_IMAGES     = []string{ALPINE, BB, NGINX_IMAGE} //nolint:revive,stylecheck
	defaultWaitTimeout = 90
	CGROUPSV2, _       = cgroups.IsCgroup2UnifiedMode()
)

// PodmanTestIntegration struct for command line options
type PodmanTestIntegration struct {
	PodmanTest
	ConmonBinary        string
	QuadletBinary       string
	Root                string
	NetworkConfigDir    string
	OCIRuntime          string
	RunRoot             string
	StorageOptions      string
	SignaturePolicyPath string
	CgroupManager       string
	Host                HostOS
	Timings             []string
	TmpDir              string
	RemoteStartErr      error
}

var LockTmpDir string

// PodmanSessionIntegration struct for command line session
type PodmanSessionIntegration struct {
	*PodmanSession
}

type testResult struct {
	name   string
	length float64
}

type testResultsSorted []testResult

func (a testResultsSorted) Len() int      { return len(a) }
func (a testResultsSorted) Swap(i, j int) { a[i], a[j] = a[j], a[i] }

type testResultsSortedLength struct{ testResultsSorted }

func (a testResultsSorted) Less(i, j int) bool { return a[i].length < a[j].length }

var testResults []testResult
var testResultsMutex sync.Mutex

func TestMain(m *testing.M) {
	if reexec.Init() {
		return
	}
	os.Exit(m.Run())
}

// TestLibpod ginkgo master function
func TestLibpod(t *testing.T) {
	if os.Getenv("NOCACHE") == "1" {
		CACHE_IMAGES = []string{}
		RESTORE_IMAGES = []string{}
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "Libpod Suite")
}

var (
	tempdir    string
	err        error
	podmanTest *PodmanTestIntegration

	_ = BeforeEach(func() {
		tempdir, err = CreateTempDirInTempDir()
		Expect(err).ToNot(HaveOccurred())
		podmanTest = PodmanTestCreate(tempdir)
		podmanTest.Setup()
	})

	_ = AfterEach(func() {
		// First unset CONTAINERS_CONF before doing Cleanup() to prevent
		// invalid containers.conf files to fail the cleanup.
		os.Unsetenv("CONTAINERS_CONF")
		os.Unsetenv("CONTAINERS_CONF_OVERRIDE")
		podmanTest.Cleanup()
		f := CurrentSpecReport()
		processTestResult(f)
	})
)

var _ = SynchronizedBeforeSuite(func() []byte {
	// make cache dir
	ImageCacheDir = filepath.Join(os.TempDir(), "imagecachedir")
	if err := os.MkdirAll(ImageCacheDir, 0700); err != nil {
		GinkgoWriter.Printf("%q\n", err)
		os.Exit(1)
	}

	// Cache images
	cwd, _ := os.Getwd()
	INTEGRATION_ROOT = filepath.Join(cwd, "../../")
	podman := PodmanTestSetup(os.TempDir())

	// Pull cirros but don't put it into the cache
	pullImages := []string{CIRROS_IMAGE, fedoraToolbox, volumeTest}
	pullImages = append(pullImages, CACHE_IMAGES...)
	for _, image := range pullImages {
		podman.createArtifact(image)
	}

	if err := os.MkdirAll(filepath.Join(ImageCacheDir, podman.ImageCacheFS+"-images"), 0777); err != nil {
		GinkgoWriter.Printf("%q\n", err)
		os.Exit(1)
	}
	podman.Root = ImageCacheDir
	// If running localized tests, the cache dir is created and populated. if the
	// tests are remote, this is a no-op
	populateCache(podman)

	host := GetHostDistributionInfo()
	if host.Distribution == "rhel" && strings.HasPrefix(host.Version, "7") {
		f, err := os.OpenFile("/proc/sys/user/max_user_namespaces", os.O_WRONLY, 0644)
		if err != nil {
			GinkgoWriter.Println("Unable to enable userspace on RHEL 7")
			os.Exit(1)
		}
		_, err = f.WriteString("15000")
		if err != nil {
			GinkgoWriter.Println("Unable to enable userspace on RHEL 7")
			os.Exit(1)
		}
		f.Close()
	}
	path, err := os.MkdirTemp("", "libpodlock")
	if err != nil {
		GinkgoWriter.Println(err)
		os.Exit(1)
	}

	// If running remote, we need to stop the associated podman system service
	if podman.RemoteTest {
		podman.StopRemoteService()
	}

	return []byte(path)
}, func(data []byte) {
	cwd, _ := os.Getwd()
	INTEGRATION_ROOT = filepath.Join(cwd, "../../")
	LockTmpDir = string(data)
})

func (p *PodmanTestIntegration) Setup() {
	cwd, _ := os.Getwd()
	INTEGRATION_ROOT = filepath.Join(cwd, "../../")
}

var _ = SynchronizedAfterSuite(func() {
	f, err := os.Create(fmt.Sprintf("%s/timings-%d", LockTmpDir, GinkgoParallelProcess()))
	Expect(err).ToNot(HaveOccurred())
	defer f.Close()
	for _, result := range testResults {
		_, err := f.WriteString(fmt.Sprintf("%s\t\t%f\n", result.name, result.length))
		Expect(err).ToNot(HaveOccurred(), "write timings")
	}
},
	func() {
		testTimings := make(testResultsSorted, 0, 2000)
		for i := 1; i <= GinkgoT().ParallelTotal(); i++ {
			f, err := os.Open(fmt.Sprintf("%s/timings-%d", LockTmpDir, i))
			Expect(err).ToNot(HaveOccurred())
			defer f.Close()
			scanner := bufio.NewScanner(f)
			for scanner.Scan() {
				text := scanner.Text()
				timing := strings.SplitN(text, "\t\t", 2)
				if len(timing) != 2 {
					Fail(fmt.Sprintf("incorrect timing line: %q", text))
				}
				name := timing[0]
				duration, err := strconv.ParseFloat(timing[1], 64)
				Expect(err).ToNot(HaveOccurred(), "failed to parse float from timings file")
				testTimings = append(testTimings, testResult{name: name, length: duration})
			}
			if err := scanner.Err(); err != nil {
				Expect(err).ToNot(HaveOccurred(), "read timings %d", i)
			}
		}
		sort.Sort(testResultsSortedLength{testTimings})
		GinkgoWriter.Println("integration timing results")
		for _, result := range testTimings {
			GinkgoWriter.Printf("%s\t\t%f\n", result.name, result.length)
		}

		// previous runroot
		tempdir, err := CreateTempDirInTempDir()
		if err != nil {
			os.Exit(1)
		}
		podmanTest := PodmanTestCreate(tempdir)
		defer os.RemoveAll(tempdir)

		if err := os.RemoveAll(podmanTest.Root); err != nil {
			GinkgoWriter.Printf("%q\n", err)
		}

		// If running remote, we need to stop the associated podman system service
		if podmanTest.RemoteTest {
			podmanTest.StopRemoteService()
		}
		// for localized tests, this removes the image cache dir and for remote tests
		// this is a no-op
		podmanTest.removeCache(podmanTest.ImageCacheDir)

		// LockTmpDir can already be removed
		os.RemoveAll(LockTmpDir)
	})

// PodmanTestCreate creates a PodmanTestIntegration instance for the tests
func PodmanTestCreateUtil(tempDir string, remote bool) *PodmanTestIntegration {
	var podmanRemoteBinary string

	host := GetHostDistributionInfo()
	cwd, _ := os.Getwd()

	root := filepath.Join(tempDir, "root")
	podmanBinary := filepath.Join(cwd, "../../bin/podman")
	if os.Getenv("PODMAN_BINARY") != "" {
		podmanBinary = os.Getenv("PODMAN_BINARY")
	}

	podmanRemoteBinary = filepath.Join(cwd, "../../bin/podman-remote")
	if os.Getenv("PODMAN_REMOTE_BINARY") != "" {
		podmanRemoteBinary = os.Getenv("PODMAN_REMOTE_BINARY")
	}

	quadletBinary := filepath.Join(cwd, "../../bin/quadlet")
	if os.Getenv("QUADLET_BINARY") != "" {
		quadletBinary = os.Getenv("QUADLET_BINARY")
	}

	conmonBinary := "/usr/libexec/podman/conmon"
	altConmonBinary := "/usr/bin/conmon"
	if _, err := os.Stat(conmonBinary); os.IsNotExist(err) {
		conmonBinary = altConmonBinary
	}
	if os.Getenv("CONMON_BINARY") != "" {
		conmonBinary = os.Getenv("CONMON_BINARY")
	}
	storageOptions := STORAGE_OPTIONS
	if os.Getenv("STORAGE_OPTIONS") != "" {
		storageOptions = os.Getenv("STORAGE_OPTIONS")
	}

	cgroupManager := CGROUP_MANAGER
	if isRootless() {
		cgroupManager = "cgroupfs"
	}
	if os.Getenv("CGROUP_MANAGER") != "" {
		cgroupManager = os.Getenv("CGROUP_MANAGER")
	}

	ociRuntime := os.Getenv("OCI_RUNTIME")
	if ociRuntime == "" {
		ociRuntime = "crun"
	}
	os.Setenv("DISABLE_HC_SYSTEMD", "true")

	dbBackend := "boltdb"
	if os.Getenv("PODMAN_DB") == "sqlite" {
		dbBackend = "sqlite"
	}

	networkBackend := CNI
	networkConfigDir := "/etc/cni/net.d"
	if isRootless() {
		networkConfigDir = filepath.Join(os.Getenv("HOME"), ".config/cni/net.d")
	}

	if strings.ToLower(os.Getenv("NETWORK_BACKEND")) == "netavark" {
		networkBackend = Netavark
		networkConfigDir = "/etc/containers/networks"
		if isRootless() {
			networkConfigDir = filepath.Join(root, "etc", "networks")
		}
	}

	if err := os.MkdirAll(root, 0755); err != nil {
		panic(err)
	}

	if err := os.MkdirAll(networkConfigDir, 0755); err != nil {
		panic(err)
	}

	storageFs := STORAGE_FS
	if isRootless() {
		storageFs = ROOTLESS_STORAGE_FS
	}
	if os.Getenv("STORAGE_FS") != "" {
		storageFs = os.Getenv("STORAGE_FS")
		storageOptions = "--storage-driver " + storageFs
	}

	ImageCacheDir = filepath.Join(os.TempDir(), "imagecachedir")

	p := &PodmanTestIntegration{
		PodmanTest: PodmanTest{
			PodmanBinary:       podmanBinary,
			RemotePodmanBinary: podmanRemoteBinary,
			TempDir:            tempDir,
			RemoteTest:         remote,
			ImageCacheFS:       storageFs,
			ImageCacheDir:      ImageCacheDir,
			NetworkBackend:     networkBackend,
			DatabaseBackend:    dbBackend,
		},
		ConmonBinary:        conmonBinary,
		QuadletBinary:       quadletBinary,
		Root:                root,
		TmpDir:              tempDir,
		NetworkConfigDir:    networkConfigDir,
		OCIRuntime:          ociRuntime,
		RunRoot:             filepath.Join(tempDir, "runroot"),
		StorageOptions:      storageOptions,
		SignaturePolicyPath: filepath.Join(INTEGRATION_ROOT, "test/policy.json"),
		CgroupManager:       cgroupManager,
		Host:                host,
	}

	if remote {
		var pathPrefix string
		if !isRootless() {
			pathPrefix = "/run/podman/podman"
		} else {
			runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
			pathPrefix = filepath.Join(runtimeDir, "podman")
		}
		// We want to avoid collisions in socket paths, but using the
		// socket directly for a collision check doesn’t work; bind(2) on AF_UNIX
		// creates the file, and we need to pass a unique path now before the bind(2)
		// happens. So, use a podman-%s.sock-lock empty file as a marker.
		tries := 0
		for {
			uuid := stringid.GenerateRandomID()
			lockPath := fmt.Sprintf("%s-%s.sock-lock", pathPrefix, uuid)
			lockFile, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
			if err == nil {
				lockFile.Close()
				p.RemoteSocketLock = lockPath
				p.RemoteSocket = fmt.Sprintf("unix:%s-%s.sock", pathPrefix, uuid)
				break
			}
			tries++
			if tries >= 1000 {
				panic("Too many RemoteSocket collisions")
			}
		}
	}

	// Set up registries.conf ENV variable
	p.setDefaultRegistriesConfigEnv()
	// Rewrite the PodmanAsUser function
	p.PodmanMakeOptions = p.makeOptions
	return p
}

func (p PodmanTestIntegration) AddImageToRWStore(image string) {
	if err := p.RestoreArtifact(image); err != nil {
		logrus.Errorf("Unable to restore %s to RW store", image)
	}
}

func imageTarPath(image string) string {
	cacheDir := os.Getenv("PODMAN_TEST_IMAGE_CACHE_DIR")
	if cacheDir == "" {
		cacheDir = os.Getenv("TMPDIR")
		if cacheDir == "" {
			cacheDir = "/tmp"
		}
	}

	// e.g., registry.com/fubar:latest -> registry.com-fubar-latest.tar
	imageCacheName := strings.ReplaceAll(strings.ReplaceAll(image, ":", "-"), "/", "-") + ".tar"

	return filepath.Join(cacheDir, imageCacheName)
}

// createArtifact creates a cached image tarball in a local directory
func (p *PodmanTestIntegration) createArtifact(image string) {
	if os.Getenv("NO_TEST_CACHE") != "" {
		return
	}
	destName := imageTarPath(image)
	if _, err := os.Stat(destName); os.IsNotExist(err) {
		GinkgoWriter.Printf("Caching %s at %s...\n", image, destName)
		pull := p.PodmanNoCache([]string{"pull", image})
		pull.Wait(440)
		Expect(pull).Should(Exit(0))

		save := p.PodmanNoCache([]string{"save", "-o", destName, image})
		save.Wait(90)
		Expect(save).Should(Exit(0))
		GinkgoWriter.Printf("\n")
	} else {
		GinkgoWriter.Printf("[image already cached: %s]\n", destName)
	}
}

// InspectImageJSON takes the session output of an inspect
// image and returns json
func (s *PodmanSessionIntegration) InspectImageJSON() []inspect.ImageData {
	var i []inspect.ImageData
	err := jsoniter.Unmarshal(s.Out.Contents(), &i)
	Expect(err).ToNot(HaveOccurred())
	return i
}

// InspectContainer returns a container's inspect data in JSON format
func (p *PodmanTestIntegration) InspectContainer(name string) []define.InspectContainerData {
	cmd := []string{"inspect", name}
	session := p.Podman(cmd)
	session.WaitWithDefaultTimeout()
	Expect(session).Should(Exit(0))
	return session.InspectContainerToJSON()
}

func processTestResult(r SpecReport) {
	tr := testResult{length: r.RunTime.Seconds(), name: r.FullText()}
	testResultsMutex.Lock()
	testResults = append(testResults, tr)
	testResultsMutex.Unlock()
}

func GetPortLock(port string) *lockfile.LockFile {
	lockFile := filepath.Join(LockTmpDir, port)
	lock, err := lockfile.GetLockFile(lockFile)
	if err != nil {
		GinkgoWriter.Println(err)
		os.Exit(1)
	}
	lock.Lock()
	return lock
}

// GetRandomIPAddress returns a random IP address to avoid IP
// collisions during parallel tests
func GetRandomIPAddress() string {
	// To avoid IP collisions of initialize random seed for random IP addresses
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	nProcs := GinkgoT().ParallelTotal()
	myProc := GinkgoT().ParallelProcess() - 1

	// 10.88.255 is unlikely to be used by CNI or netavark.
	// Last octet .0 - .254, and will be unique to this ginkgo process.
	ip4 := strconv.Itoa(rng.Intn((255-nProcs)/nProcs)*nProcs + myProc)
	return "10.88.255." + ip4
}

// RunTopContainer runs a simple container in the background that
// runs top.  If the name passed != "", it will have a name
func (p *PodmanTestIntegration) RunTopContainer(name string) *PodmanSessionIntegration {
	return p.RunTopContainerWithArgs(name, nil)
}

// RunTopContainerWithArgs runs a simple container in the background that
// runs top.  If the name passed != "", it will have a name, command args can also be passed in
func (p *PodmanTestIntegration) RunTopContainerWithArgs(name string, args []string) *PodmanSessionIntegration {
	// In proxy environment, some tests need to the --http-proxy=false option (#16684)
	var podmanArgs = []string{"run", "--http-proxy=false"}
	if name != "" {
		podmanArgs = append(podmanArgs, "--name", name)
	}
	podmanArgs = append(podmanArgs, args...)
	podmanArgs = append(podmanArgs, "-d", ALPINE, "top")
	return p.Podman(podmanArgs)
}

// RunLsContainer runs a simple container in the background that
// simply runs ls. If the name passed != "", it will have a name
func (p *PodmanTestIntegration) RunLsContainer(name string) (*PodmanSessionIntegration, int, string) {
	var podmanArgs = []string{"run"}
	if name != "" {
		podmanArgs = append(podmanArgs, "--name", name)
	}
	podmanArgs = append(podmanArgs, "-d", ALPINE, "ls")
	session := p.Podman(podmanArgs)
	session.WaitWithDefaultTimeout()
	if session.ExitCode() != 0 {
		return session, session.ExitCode(), session.OutputToString()
	}
	cid := session.OutputToString()

	wsession := p.Podman([]string{"wait", cid})
	wsession.WaitWithDefaultTimeout()
	return session, wsession.ExitCode(), cid
}

// RunNginxWithHealthCheck runs the alpine nginx container with an optional name and adds a healthcheck into it
func (p *PodmanTestIntegration) RunNginxWithHealthCheck(name string) (*PodmanSessionIntegration, string) {
	var podmanArgs = []string{"run"}
	if name != "" {
		podmanArgs = append(podmanArgs, "--name", name)
	}
	// curl without -f exits 0 even if http code >= 400!
	podmanArgs = append(podmanArgs, "-dt", "-P", "--health-cmd", "curl -f http://localhost/", NGINX_IMAGE)
	session := p.Podman(podmanArgs)
	session.WaitWithDefaultTimeout()
	return session, session.OutputToString()
}

// RunContainerWithNetworkTest runs the fedoraMinimal curl with the specified network mode.
func (p *PodmanTestIntegration) RunContainerWithNetworkTest(mode string) *PodmanSessionIntegration {
	var podmanArgs = []string{"run"}
	if mode != "" {
		podmanArgs = append(podmanArgs, "--network", mode)
	}
	podmanArgs = append(podmanArgs, fedoraMinimal, "curl", "-k", "-o", "/dev/null", "http://www.redhat.com:80")
	session := p.Podman(podmanArgs)
	return session
}

func (p *PodmanTestIntegration) RunLsContainerInPod(name, pod string) (*PodmanSessionIntegration, int, string) {
	var podmanArgs = []string{"run", "--pod", pod}
	if name != "" {
		podmanArgs = append(podmanArgs, "--name", name)
	}
	podmanArgs = append(podmanArgs, "-d", ALPINE, "ls")
	session := p.Podman(podmanArgs)
	session.WaitWithDefaultTimeout()
	if session.ExitCode() != 0 {
		return session, session.ExitCode(), session.OutputToString()
	}
	cid := session.OutputToString()

	wsession := p.Podman([]string{"wait", cid})
	wsession.WaitWithDefaultTimeout()
	return session, wsession.ExitCode(), cid
}

// BuildImage uses podman build and buildah to build an image
// called imageName based on a string dockerfile
func (p *PodmanTestIntegration) BuildImage(dockerfile, imageName string, layers string) string {
	return p.buildImage(dockerfile, imageName, layers, "")
}

// BuildImageWithLabel uses podman build and buildah to build an image
// called imageName based on a string dockerfile, adds desired label to paramset
func (p *PodmanTestIntegration) BuildImageWithLabel(dockerfile, imageName string, layers string, label string) string {
	return p.buildImage(dockerfile, imageName, layers, label)
}

// PodmanPID execs podman and returns its PID
func (p *PodmanTestIntegration) PodmanPID(args []string) (*PodmanSessionIntegration, int) {
	podmanOptions := p.MakeOptions(args, false, false)
	GinkgoWriter.Printf("Running: %s %s\n", p.PodmanBinary, strings.Join(podmanOptions, " "))

	command := exec.Command(p.PodmanBinary, podmanOptions...)
	session, err := Start(command, GinkgoWriter, GinkgoWriter)
	if err != nil {
		Fail("unable to run podman command: " + strings.Join(podmanOptions, " "))
	}
	podmanSession := &PodmanSession{Session: session}
	return &PodmanSessionIntegration{podmanSession}, command.Process.Pid
}

func (p *PodmanTestIntegration) Quadlet(args []string, sourceDir string) *PodmanSessionIntegration {
	GinkgoWriter.Printf("Running: %s %s with QUADLET_UNIT_DIRS=%s\n", p.QuadletBinary, strings.Join(args, " "), sourceDir)

	// quadlet uses PODMAN env to get a stable podman path
	podmanPath, found := os.LookupEnv("PODMAN")
	if !found {
		podmanPath = p.PodmanBinary
	}

	command := exec.Command(p.QuadletBinary, args...)
	command.Env = []string{
		fmt.Sprintf("QUADLET_UNIT_DIRS=%s", sourceDir),
		fmt.Sprintf("PODMAN=%s", podmanPath),
	}
	session, err := Start(command, GinkgoWriter, GinkgoWriter)
	if err != nil {
		Fail("unable to run quadlet command: " + strings.Join(args, " "))
	}
	quadletSession := &PodmanSession{Session: session}
	return &PodmanSessionIntegration{quadletSession}
}

// Cleanup cleans up the temporary store
func (p *PodmanTestIntegration) Cleanup() {
	// ginkgo v2 still goes into AfterEach() when Skip() was called,
	// some tests call skip before the podman test is initialized.
	if p == nil {
		return
	}

	// first stop everything, rm -fa is unreliable
	// https://github.com/containers/podman/issues/18180
	stop := p.Podman([]string{"stop", "--all", "-t", "0"})
	stop.WaitWithDefaultTimeout()

	// Remove all pods...
	podrm := p.Podman([]string{"pod", "rm", "-fa", "-t", "0"})
	podrm.WaitWithDefaultTimeout()

	// ...and containers
	rmall := p.Podman([]string{"rm", "-fa", "-t", "0"})
	rmall.WaitWithDefaultTimeout()

	p.StopRemoteService()
	// Nuke tempdir
	p.removeCache(p.TempDir)

	// Clean up the registries configuration file ENV variable set in Create
	resetRegistriesConfigEnv()

	// Make sure to only check exit codes after all cleanup is done.
	// An error would cause it to stop and return early otherwise.
	Expect(stop).To(Exit(0), "command: %v\nstdout: %s\nstderr: %s", stop.Command.Args, stop.OutputToString(), stop.ErrorToString())
	Expect(podrm).To(Exit(0), "command: %v\nstdout: %s\nstderr: %s", podrm.Command.Args, podrm.OutputToString(), podrm.ErrorToString())
	Expect(rmall).To(Exit(0), "command: %v\nstdout: %s\nstderr: %s", rmall.Command.Args, rmall.OutputToString(), rmall.ErrorToString())
}

// CleanupVolume cleans up the volumes and containers.
// This already calls Cleanup() internally.
func (p *PodmanTestIntegration) CleanupVolume() {
	// Remove all containers
	session := p.Podman([]string{"volume", "rm", "-fa"})
	session.WaitWithDefaultTimeout()
	Expect(session).To(Exit(0), "command: %v\nstdout: %s\nstderr: %s", session.Command.Args, session.OutputToString(), session.ErrorToString())
}

// CleanupSecret cleans up the secrets and containers.
// This already calls Cleanup() internally.
func (p *PodmanTestIntegration) CleanupSecrets() {
	// Remove all containers
	session := p.Podman([]string{"secret", "rm", "-a"})
	session.Wait(90)
	Expect(session).To(Exit(0), "command: %v\nstdout: %s\nstderr: %s", session.Command.Args, session.OutputToString(), session.ErrorToString())
}

// InspectContainerToJSON takes the session output of an inspect
// container and returns json
func (s *PodmanSessionIntegration) InspectContainerToJSON() []define.InspectContainerData {
	var i []define.InspectContainerData
	err := jsoniter.Unmarshal(s.Out.Contents(), &i)
	Expect(err).ToNot(HaveOccurred())
	return i
}

// InspectPodToJSON takes the sessions output from a pod inspect and returns json
func (s *PodmanSessionIntegration) InspectPodToJSON() define.InspectPodData {
	var i define.InspectPodData
	err := jsoniter.Unmarshal(s.Out.Contents(), &i)
	Expect(err).ToNot(HaveOccurred())
	return i
}

// InspectPodToJSON takes the sessions output from an inspect and returns json
func (s *PodmanSessionIntegration) InspectPodArrToJSON() []define.InspectPodData {
	var i []define.InspectPodData
	err := jsoniter.Unmarshal(s.Out.Contents(), &i)
	Expect(err).ToNot(HaveOccurred())
	return i
}

// CreatePod creates a pod with no infra container
// it optionally takes a pod name
func (p *PodmanTestIntegration) CreatePod(options map[string][]string) (*PodmanSessionIntegration, int, string) {
	var args = []string{"pod", "create", "--infra=false", "--share", ""}
	for k, values := range options {
		for _, v := range values {
			args = append(args, k+"="+v)
		}
	}

	session := p.Podman(args)
	session.WaitWithDefaultTimeout()
	return session, session.ExitCode(), session.OutputToString()
}

func (p *PodmanTestIntegration) CreateVolume(options map[string][]string) (*PodmanSessionIntegration, int, string) {
	var args = []string{"volume", "create"}
	for k, values := range options {
		for _, v := range values {
			args = append(args, k+"="+v)
		}
	}

	session := p.Podman(args)
	session.WaitWithDefaultTimeout()
	return session, session.ExitCode(), session.OutputToString()
}

func (p *PodmanTestIntegration) RunTopContainerInPod(name, pod string) *PodmanSessionIntegration {
	return p.RunTopContainerWithArgs(name, []string{"--pod", pod})
}

func (p *PodmanTestIntegration) RunHealthCheck(cid string) error {
	for i := 0; i < 10; i++ {
		hc := p.Podman([]string{"healthcheck", "run", cid})
		hc.WaitWithDefaultTimeout()
		if hc.ExitCode() == 0 {
			return nil
		}
		// Restart container if it's not running
		ps := p.Podman([]string{"ps", "--no-trunc", "--quiet", "--filter", fmt.Sprintf("id=%s", cid)})
		ps.WaitWithDefaultTimeout()
		if ps.ExitCode() == 0 {
			if !strings.Contains(ps.OutputToString(), cid) {
				GinkgoWriter.Printf("Container %s is not running, restarting", cid)
				restart := p.Podman([]string{"restart", cid})
				restart.WaitWithDefaultTimeout()
				if restart.ExitCode() != 0 {
					return fmt.Errorf("unable to restart %s", cid)
				}
			}
		}
		GinkgoWriter.Printf("Waiting for %s to pass healthcheck\n", cid)
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("unable to detect %s as running", cid)
}

func (p *PodmanTestIntegration) CreateSeccompJSON(in []byte) (string, error) {
	jsonFile := filepath.Join(p.TempDir, "seccomp.json")
	err := WriteJSONFile(in, jsonFile)
	if err != nil {
		return "", err
	}
	return jsonFile, nil
}

func checkReason(reason string) {
	if len(reason) < 5 {
		panic("Test must specify a reason to skip")
	}
}

func SkipIfRunc(p *PodmanTestIntegration, reason string) {
	checkReason(reason)
	if p.OCIRuntime == "runc" {
		Skip("[runc]: " + reason)
	}
}

func SkipIfRootlessCgroupsV1(reason string) {
	checkReason(reason)
	if isRootless() && !CGROUPSV2 {
		Skip("[rootless]: " + reason)
	}
}

func SkipIfRootless(reason string) {
	checkReason(reason)
	if isRootless() {
		Skip("[rootless]: " + reason)
	}
}

func SkipIfNotRootless(reason string) {
	checkReason(reason)
	if !isRootless() {
		Skip("[notRootless]: " + reason)
	}
}

func SkipIfSystemdNotRunning(reason string) {
	checkReason(reason)

	cmd := exec.Command("systemctl", "list-units")
	err := cmd.Run()
	if err != nil {
		if _, ok := err.(*exec.Error); ok {
			Skip("[notSystemd]: not running " + reason)
		}
		Expect(err).ToNot(HaveOccurred())
	}
}

func SkipIfNotSystemd(manager, reason string) {
	checkReason(reason)
	if manager != "systemd" {
		Skip("[notSystemd]: " + reason)
	}
}

func SkipOnOSVersion(os, version string) {
	info := GetHostDistributionInfo()
	if info.Distribution == os && info.Version == version {
		Skip(fmt.Sprintf("Test doesn't work on %s %s", os, version))
	}
}

func SkipIfNotFedora() {
	info := GetHostDistributionInfo()
	if info.Distribution != "fedora" {
		Skip("Test can only run on Fedora")
	}
}

type journaldTests struct {
	journaldSkip bool
	journaldOnce sync.Once
}

var journald journaldTests

func SkipIfJournaldUnavailable() {
	f := func() {
		journald.journaldSkip = false

		// Check if journalctl is unavailable
		cmd := exec.Command("journalctl", "-n", "1")
		if err := cmd.Run(); err != nil {
			journald.journaldSkip = true
		}
	}
	journald.journaldOnce.Do(f)

	// In container, journalctl does not return an error even if
	// journald is unavailable
	SkipIfInContainer("[journald]: journalctl inside a container doesn't work correctly")
	if journald.journaldSkip {
		Skip("[journald]: journald is unavailable")
	}
}

// Use isRootless() instead of rootless.IsRootless()
// This function can detect to join the user namespace by mistake
func isRootless() bool {
	return os.Geteuid() != 0
}

func isCgroupsV1() bool {
	return !CGROUPSV2
}

func SkipIfCgroupV1(reason string) {
	checkReason(reason)
	if isCgroupsV1() {
		Skip(reason)
	}
}

func SkipIfCgroupV2(reason string) {
	checkReason(reason)
	if CGROUPSV2 {
		Skip(reason)
	}
}

func isContainerized() bool {
	// This is set to "podman" by podman automatically
	return os.Getenv("container") != ""
}

func SkipIfContainerized(reason string) {
	checkReason(reason)
	if isContainerized() {
		Skip(reason)
	}
}

func SkipIfRemote(reason string) {
	checkReason(reason)
	if !IsRemote() {
		return
	}
	Skip("[remote]: " + reason)
}

func SkipIfNotRemote(reason string) {
	checkReason(reason)
	if IsRemote() {
		return
	}
	Skip("[local]: " + reason)
}

// SkipIfInContainer skips a test if the test is run inside a container
func SkipIfInContainer(reason string) {
	checkReason(reason)
	if os.Getenv("TEST_ENVIRON") == "container" {
		Skip("[container]: " + reason)
	}
}

// SkipIfNotActive skips a test if the given systemd unit is not active
func SkipIfNotActive(unit string, reason string) {
	checkReason(reason)

	cmd := exec.Command("systemctl", "is-active", unit)
	cmd.Stdout = GinkgoWriter
	cmd.Stderr = GinkgoWriter
	err := cmd.Run()
	if cmd.ProcessState.ExitCode() == 0 {
		return
	}
	Skip(fmt.Sprintf("[systemd]: unit %s is not active (%v): %s", unit, err, reason))
}

func SkipIfCNI(p *PodmanTestIntegration) {
	if p.NetworkBackend == CNI {
		Skip("this test is not compatible with the CNI network backend")
	}
}

func SkipIfNetavark(p *PodmanTestIntegration) {
	if p.NetworkBackend == Netavark {
		Skip("This test is not compatible with the netavark network backend")
	}
}

// PodmanAsUser is the exec call to podman on the filesystem with the specified uid/gid and environment
func (p *PodmanTestIntegration) PodmanAsUser(args []string, uid, gid uint32, cwd string, env []string) *PodmanSessionIntegration {
	podmanSession := p.PodmanAsUserBase(args, uid, gid, cwd, env, false, false, nil, nil)
	return &PodmanSessionIntegration{podmanSession}
}

// RestartRemoteService stop and start API Server, usually to change config
func (p *PodmanTestIntegration) RestartRemoteService() {
	p.StopRemoteService()
	p.StartRemoteService()
}

// RestoreArtifactToCache populates the imagecache from tarballs that were cached earlier
func (p *PodmanTestIntegration) RestoreArtifactToCache(image string) error {
	tarball := imageTarPath(image)
	if _, err := os.Stat(tarball); err == nil {
		GinkgoWriter.Printf("Restoring %s...\n", image)
		p.Root = p.ImageCacheDir
		restore := p.PodmanNoEvents([]string{"load", "-q", "-i", tarball})
		restore.WaitWithDefaultTimeout()
	}
	return nil
}

func populateCache(podman *PodmanTestIntegration) {
	for _, image := range CACHE_IMAGES {
		err := podman.RestoreArtifactToCache(image)
		Expect(err).ToNot(HaveOccurred())
	}
	// logformatter uses this to recognize the first test
	GinkgoWriter.Printf("-----------------------------\n")
}

func (p *PodmanTestIntegration) removeCache(path string) {
	// Remove cache dirs
	if isRootless() {
		// If rootless, os.RemoveAll() is failed due to permission denied
		cmd := exec.Command(p.PodmanBinary, "unshare", "rm", "-rf", path)
		cmd.Stdout = GinkgoWriter
		cmd.Stderr = GinkgoWriter
		if err := cmd.Run(); err != nil {
			GinkgoWriter.Printf("%v\n", err)
		}
	} else {
		if err := os.RemoveAll(path); err != nil {
			GinkgoWriter.Printf("%q\n", err)
		}
	}
}

// PodmanNoCache calls the podman command with no configured imagecache
func (p *PodmanTestIntegration) PodmanNoCache(args []string) *PodmanSessionIntegration {
	podmanSession := p.PodmanBase(args, false, true)
	return &PodmanSessionIntegration{podmanSession}
}

func PodmanTestSetup(tempDir string) *PodmanTestIntegration {
	return PodmanTestCreateUtil(tempDir, false)
}

// PodmanNoEvents calls the Podman command without an imagecache and without an
// events backend. It is used mostly for caching and uncaching images.
func (p *PodmanTestIntegration) PodmanNoEvents(args []string) *PodmanSessionIntegration {
	podmanSession := p.PodmanBase(args, true, true)
	return &PodmanSessionIntegration{podmanSession}
}

// MakeOptions assembles all the podman main options
func (p *PodmanTestIntegration) makeOptions(args []string, noEvents, noCache bool) []string {
	if p.RemoteTest {
		if !util.StringInSlice("--remote", args) {
			return append([]string{"--remote", "--url", p.RemoteSocket}, args...)
		}
		return args
	}

	var debug string
	if _, ok := os.LookupEnv("E2E_DEBUG"); ok {
		debug = "--log-level=debug --syslog=true "
	}

	eventsType := "file"
	if noEvents {
		eventsType = "none"
	}

	podmanOptions := strings.Split(fmt.Sprintf("%s--root %s --runroot %s --runtime %s --conmon %s --network-config-dir %s --network-backend %s --cgroup-manager %s --tmpdir %s --events-backend %s --db-backend %s",
		debug, p.Root, p.RunRoot, p.OCIRuntime, p.ConmonBinary, p.NetworkConfigDir, p.NetworkBackend.ToString(), p.CgroupManager, p.TmpDir, eventsType, p.DatabaseBackend), " ")

	podmanOptions = append(podmanOptions, strings.Split(p.StorageOptions, " ")...)
	if !noCache {
		cacheOptions := []string{"--storage-opt",
			fmt.Sprintf("%s.imagestore=%s", p.PodmanTest.ImageCacheFS, p.PodmanTest.ImageCacheDir)}
		podmanOptions = append(cacheOptions, podmanOptions...)
	}
	podmanOptions = append(podmanOptions, args...)
	return podmanOptions
}

func writeConf(conf []byte, confPath string) {
	if _, err := os.Stat(filepath.Dir(confPath)); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(confPath), 0o777); err != nil {
			GinkgoWriter.Println(err)
		}
	}
	if err := os.WriteFile(confPath, conf, 0o777); err != nil {
		GinkgoWriter.Println(err)
	}
}

func removeConf(confPath string) {
	if err := os.Remove(confPath); err != nil {
		GinkgoWriter.Println(err)
	}
}

// generateNetworkConfig generates a CNI or Netavark config with a random name
// it returns the network name and the filepath
func generateNetworkConfig(p *PodmanTestIntegration) (string, string) {
	var (
		path string
		conf string
	)
	// generate a random name to prevent conflicts with other tests
	name := "net" + stringid.GenerateRandomID()
	if p.NetworkBackend != Netavark {
		path = filepath.Join(p.NetworkConfigDir, fmt.Sprintf("%s.conflist", name))
		conf = fmt.Sprintf(`{
		"cniVersion": "0.3.0",
		"name": "%s",
		"plugins": [
		  {
			"type": "bridge",
			"bridge": "cni1",
			"isGateway": true,
			"ipMasq": true,
			"ipam": {
				"type": "host-local",
				"subnet": "10.99.0.0/16",
				"routes": [
					{ "dst": "0.0.0.0/0" }
				]
			}
		  },
		  {
			"type": "portmap",
			"capabilities": {
			  "portMappings": true
			}
		  }
		]
	}`, name)
	} else {
		path = filepath.Join(p.NetworkConfigDir, fmt.Sprintf("%s.json", name))
		conf = fmt.Sprintf(`
{
     "name": "%s",
     "id": "e1ef2749024b88f5663ca693a9118e036d6bfc48bcfe460faf45e9614a513e5c",
     "driver": "bridge",
     "network_interface": "netavark1",
     "created": "2022-01-05T14:15:10.975493521-06:00",
     "subnets": [
          {
               "subnet": "10.100.0.0/16",
               "gateway": "10.100.0.1"
          }
     ],
     "ipv6_enabled": false,
     "internal": false,
     "dns_enabled": true,
     "ipam_options": {
          "driver": "host-local"
     }
}
`, name)
	}
	writeConf([]byte(conf), path)
	return name, path
}

func (p *PodmanTestIntegration) removeNetwork(name string) {
	session := p.Podman([]string{"network", "rm", "-f", name})
	session.WaitWithDefaultTimeout()
	Expect(session.ExitCode()).To(BeNumerically("<=", 1), "Exit code must be 0 or 1")
}

// generatePolicyFile generates a signature verification policy file.
// it returns the policy file path.
func generatePolicyFile(tempDir string) string {
	keyPath := filepath.Join(tempDir, "key.gpg")
	policyPath := filepath.Join(tempDir, "policy.json")
	conf := fmt.Sprintf(`
{
    "default": [
        {
            "type": "insecureAcceptAnything"
        }
    ],
    "transports": {
        "docker": {
            "localhost:5000": [
                {
                    "type": "signedBy",
                    "keyType": "GPGKeys",
                    "keyPath": "%s"
                }
            ],
            "localhost:5000/sigstore-signed": [
                {
                    "type": "sigstoreSigned",
                    "keyPath": "testdata/sigstore-key.pub"
                }
            ],
            "localhost:5000/sigstore-signed-params": [
                {
                    "type": "sigstoreSigned",
                    "keyPath": "testdata/sigstore-key.pub"
                }
            ]
        }
    }
}
`, keyPath)
	writeConf([]byte(conf), policyPath)
	return policyPath
}

func (s *PodmanSessionIntegration) jq(jqCommand string) (string, error) {
	var out bytes.Buffer
	cmd := exec.Command("jq", jqCommand)
	cmd.Stdin = strings.NewReader(s.OutputToString())
	cmd.Stdout = &out
	err := cmd.Run()
	return strings.TrimRight(out.String(), "\n"), err
}

func (p *PodmanTestIntegration) buildImage(dockerfile, imageName string, layers string, label string) string {
	dockerfilePath := filepath.Join(p.TempDir, "Dockerfile-"+stringid.GenerateRandomID())
	err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0755)
	Expect(err).ToNot(HaveOccurred())
	cmd := []string{"build", "--pull-never", "--layers=" + layers, "--file", dockerfilePath}
	if label != "" {
		cmd = append(cmd, "--label="+label)
	}
	if len(imageName) > 0 {
		cmd = append(cmd, []string{"-t", imageName}...)
	}
	cmd = append(cmd, p.TempDir)
	session := p.Podman(cmd)
	session.Wait(240)
	Expect(session).Should(Exit(0), fmt.Sprintf("BuildImage session output: %q", session.OutputToString()))
	output := session.OutputToStringArray()
	return output[len(output)-1]
}

func writeYaml(content string, fileName string) error {
	f, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(content)
	if err != nil {
		return err
	}

	return nil
}

// GetPort finds an unused TCP/IP port on the system, in the range 5000-5999
func GetPort() int {
	portMin := 5000
	portMax := 5999
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	// Avoid dup-allocation races between parallel ginkgo processes
	nProcs := GinkgoT().ParallelTotal()
	myProc := GinkgoT().ParallelProcess() - 1

	for i := 0; i < 50; i++ {
		// Random port within that range
		port := portMin + rng.Intn((portMax-portMin)/nProcs)*nProcs + myProc

		used, err := net.Listen("tcp", "localhost:"+strconv.Itoa(port))
		if err == nil {
			// it's open. Return it.
			err = used.Close()
			Expect(err).ToNot(HaveOccurred(), "closing random port")
			return port
		}
	}

	Fail(fmt.Sprintf("unable to get free port: %v", err))
	return 0 // notreached
}

func ncz(port int) bool {
	timeout := 500 * time.Millisecond
	for i := 0; i < 5; i++ {
		ncCmd := []string{"-z", "localhost", fmt.Sprintf("%d", port)}
		GinkgoWriter.Printf("Running: nc %s\n", strings.Join(ncCmd, " "))
		check := SystemExec("nc", ncCmd)
		if check.ExitCode() == 0 {
			return true
		}
		time.Sleep(timeout)
		timeout++
	}
	return false
}

func createNetworkName(name string) string {
	return name + stringid.GenerateRandomID()[:10]
}

var IPRegex = `(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)(\.(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)){3}`

// digShort execs into the given container and does a dig lookup with a timeout
// backoff.  If it gets a response, it ensures that the output is in the correct
// format and iterates a string array for match
func digShort(container, lookupName, expectedIP string, p *PodmanTestIntegration) {
	digInterval := time.Millisecond * 250
	for i := 0; i < 6; i++ {
		time.Sleep(digInterval * time.Duration(i))
		dig := p.Podman([]string{"exec", container, "dig", "+short", lookupName})
		dig.WaitWithDefaultTimeout()
		output := dig.OutputToString()
		if dig.ExitCode() == 0 && output != "" {
			Expect(output).To(Equal(expectedIP))
			// success
			return
		}
	}
	Fail("dns is not responding")
}

// WaitForFile to be created in defaultWaitTimeout seconds, returns false if file not created
func WaitForFile(path string) (err error) {
	until := time.Now().Add(time.Duration(defaultWaitTimeout) * time.Second)
	for time.Now().Before(until) {
		_, err = os.Stat(path)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, os.ErrNotExist):
			time.Sleep(10 * time.Millisecond)
		default:
			return err
		}
	}
	return err
}

// WaitForService blocks for defaultWaitTimeout seconds, waiting for some service listening on given host:port
func WaitForService(address url.URL) {
	// Wait for podman to be ready
	var err error
	until := time.Now().Add(time.Duration(defaultWaitTimeout) * time.Second)
	for time.Now().Before(until) {
		var conn net.Conn
		conn, err = net.Dial("tcp", address.Host)
		if err == nil {
			conn.Close()
			break
		}

		// Podman not available yet...
		time.Sleep(10 * time.Millisecond)
	}
	Expect(err).ShouldNot(HaveOccurred())
}

// useCustomNetworkDir makes sure this test uses a custom network dir.
// This needs to be called for all test they may remove networks from other tests,
// so netwokr prune, system prune, or system reset.
// see https://github.com/containers/podman/issues/17946
func useCustomNetworkDir(podmanTest *PodmanTestIntegration, tempdir string) {
	// set custom network directory to prevent flakes since the dir is shared with all tests by default
	podmanTest.NetworkConfigDir = tempdir
	if IsRemote() {
		podmanTest.RestartRemoteService()
	}
}
