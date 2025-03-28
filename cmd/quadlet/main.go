package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/containers/podman/v4/pkg/systemd/parser"
	"github.com/containers/podman/v4/pkg/systemd/quadlet"
	"github.com/containers/podman/v4/version/rawversion"
)

// This commandline app is the systemd generator (system and user,
// decided by the name of the binary).

// Generators run at very early startup, so must work in a very
// limited environment (e.g. no /var, /home, or syslog).  See:
// https://www.freedesktop.org/software/systemd/man/systemd.generator.html#Notes%20about%20writing%20generators
// for more details.

var (
	verboseFlag bool // True if -v passed
	noKmsgFlag  bool
	isUserFlag  bool // True if run as quadlet-user-generator executable
	dryRunFlag  bool // True if -dryrun is used
	versionFlag bool // True if -version is used
)

var (
	// data saved between logToKmsg calls
	noKmsg   = false
	kmsgFile *os.File
)

var (
	void                struct{}
	supportedExtensions = map[string]struct{}{
		".container": void,
		".volume":    void,
		".kube":      void,
		".network":   void,
	}
)

// We log directly to /dev/kmsg, because that is the only way to get information out
// of the generator into the system logs.
func logToKmsg(s string) bool {
	if noKmsg {
		return false
	}

	if kmsgFile == nil {
		f, err := os.OpenFile("/dev/kmsg", os.O_WRONLY, 0644)
		if err != nil {
			noKmsg = true
			return false
		}
		kmsgFile = f
	}

	if _, err := kmsgFile.Write([]byte(s)); err != nil {
		kmsgFile.Close()
		kmsgFile = nil
		return false
	}

	return true
}

func Logf(format string, a ...interface{}) {
	s := fmt.Sprintf(format, a...)
	line := fmt.Sprintf("quadlet-generator[%d]: %s", os.Getpid(), s)

	if !logToKmsg(line) {
		// If we can't log, print to stderr
		fmt.Fprintf(os.Stderr, "%s\n", line)
		os.Stderr.Sync()
	}
}

var debugEnabled = false

func enableDebug() {
	debugEnabled = true
}

func Debugf(format string, a ...interface{}) {
	if debugEnabled {
		Logf(format, a...)
	}
}

// This returns the directories where we read quadlet .container and .volumes from
// For system generators these are in /usr/share/containers/systemd (for distro files)
// and /etc/containers/systemd (for sysadmin files).
// For user generators these can live in /etc/containers/systemd/users, /etc/containers/systemd/users/$UID, and $XDG_CONFIG_HOME/containers/systemd
func getUnitDirs(rootless bool) []string {
	// Allow overriding source dir, this is mainly for the CI tests
	unitDirsEnv := os.Getenv("QUADLET_UNIT_DIRS")
	if len(unitDirsEnv) > 0 {
		return strings.Split(unitDirsEnv, ":")
	}

	dirs := make([]string, 0)
	if rootless {
		configDir, err := os.UserConfigDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: %v", err)
			return nil
		}
		dirs = append(dirs, path.Join(configDir, "containers/systemd"))
		u, err := user.Current()
		if err == nil {
			dirs = append(dirs, filepath.Join(quadlet.UnitDirAdmin, "users", u.Uid))
		} else {
			fmt.Fprintf(os.Stderr, "Warning: %v", err)
		}
		return append(dirs, filepath.Join(quadlet.UnitDirAdmin, "users"))
	}
	dirs = append(dirs, quadlet.UnitDirAdmin)
	return append(dirs, quadlet.UnitDirDistro)
}

func isExtSupported(filename string) bool {
	ext := filepath.Ext(filename)
	_, ok := supportedExtensions[ext]
	return ok
}

func loadUnitsFromDir(sourcePath string, units map[string]*parser.UnitFile) {
	files, err := os.ReadDir(sourcePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			Logf("Can't read \"%s\": %s", sourcePath, err)
		}
		return
	}

	for _, file := range files {
		name := file.Name()
		if units[name] == nil && isExtSupported(name) {
			path := path.Join(sourcePath, name)

			Debugf("Loading source unit file %s", path)

			if f, err := parser.ParseUnitFile(path); err != nil {
				Logf("Error loading '%s', ignoring: %s", path, err)
			} else {
				units[name] = f
			}
		}
	}
}

func generateServiceFile(service *parser.UnitFile) error {
	Debugf("writing '%s'", service.Path)

	service.PrependComment("",
		fmt.Sprintf("Automatically generated by %s", os.Args[0]),
		"")

	f, err := os.Create(service.Path)
	if err != nil {
		return err
	}

	defer f.Close()

	err = service.Write(f)
	if err != nil {
		return err
	}

	err = f.Sync()
	if err != nil {
		return err
	}

	return nil
}

// This parses the `Install` group of the unit file and creates the required
// symlinks to get systemd to start the newly generated file as needed.
// In a traditional setup this is done by "systemctl enable", but that doesn't
// work for auto-generated files like these.
func enableServiceFile(outputPath string, service *parser.UnitFile) {
	symlinks := make([]string, 0)

	aliases := service.LookupAllStrv(quadlet.InstallGroup, "Alias")
	for _, alias := range aliases {
		symlinks = append(symlinks, filepath.Clean(alias))
	}

	wantedBy := service.LookupAllStrv(quadlet.InstallGroup, "WantedBy")
	for _, wantedByUnit := range wantedBy {
		// Only allow filenames, not paths
		if !strings.Contains(wantedByUnit, "/") {
			symlinks = append(symlinks, fmt.Sprintf("%s.wants/%s", wantedByUnit, service.Filename))
		}
	}

	requiredBy := service.LookupAllStrv(quadlet.InstallGroup, "RequiredBy")
	for _, requiredByUnit := range requiredBy {
		// Only allow filenames, not paths
		if !strings.Contains(requiredByUnit, "/") {
			symlinks = append(symlinks, fmt.Sprintf("%s.requires/%s", requiredByUnit, service.Filename))
		}
	}

	for _, symlinkRel := range symlinks {
		target, err := filepath.Rel(path.Dir(symlinkRel), service.Filename)
		if err != nil {
			Logf("Can't create symlink %s: %s", symlinkRel, err)
			continue
		}
		symlinkPath := path.Join(outputPath, symlinkRel)

		symlinkDir := path.Dir(symlinkPath)
		err = os.MkdirAll(symlinkDir, os.ModePerm)
		if err != nil {
			Logf("Can't create dir %s: %s", symlinkDir, err)
			continue
		}

		Debugf("Creating symlink %s -> %s", symlinkPath, target)
		_ = os.Remove(symlinkPath) // overwrite existing symlinks
		err = os.Symlink(target, symlinkPath)
		if err != nil {
			Logf("Failed creating symlink %s: %s", symlinkPath, err)
		}
	}
}

func isImageID(imageName string) bool {
	// All sha25:... names are assumed by podman to be fully specified
	if strings.HasPrefix(imageName, "sha256:") {
		return true
	}

	// However, podman also accepts image ids as pure hex strings,
	// but only those of length 64 are unambiguous image ids
	if len(imageName) != 64 {
		return false
	}

	for _, c := range imageName {
		if !unicode.Is(unicode.Hex_Digit, c) {
			return false
		}
	}

	return true
}

func isUnambiguousName(imageName string) bool {
	// Fully specified image ids are unambiguous
	if isImageID(imageName) {
		return true
	}

	// Otherwise we require a fully qualified name
	firstSlash := strings.Index(imageName, "/")
	if firstSlash == -1 {
		// No domain or path, not fully qualified
		return false
	}

	// What is before the first slash can be a domain or a path
	domain := imageName[:firstSlash]

	// If its a domain (has dot or port or is "localhost") it is considered fq
	if strings.ContainsAny(domain, ".:") || domain == "localhost" {
		return true
	}

	return false
}

// warns if input is an ambiguous name, i.e. a partial image id or a short
// name (i.e. is missing a registry)
//
// Examples:
//   - short names: "image:tag", "library/fedora"
//   - fully qualified names: "quay.io/image", "localhost/image:tag",
//     "server.org:5000/lib/image", "sha256:..."
//
// We implement a simple version of this from scratch here to avoid
// a huge dependency in the generator just for a warning.
func warnIfAmbiguousName(container *parser.UnitFile) {
	imageName, ok := container.Lookup(quadlet.ContainerGroup, quadlet.KeyImage)
	if !ok {
		return
	}
	if !isUnambiguousName(imageName) {
		Logf("Warning: %s specifies the image \"%s\" which not a fully qualified image name. This is not ideal for performance and security reasons. See the podman-pull manpage discussion of short-name-aliases.conf for details.", container.Filename, imageName)
	}
}

func main() {
	exitCode := 0
	prgname := path.Base(os.Args[0])
	isUserFlag = strings.Contains(prgname, "user")

	flag.Parse()

	if versionFlag {
		fmt.Printf("%s\n", rawversion.RawVersion)
		return
	}

	if verboseFlag || dryRunFlag {
		enableDebug()
	}

	if noKmsgFlag || dryRunFlag {
		noKmsg = true
	}

	if !dryRunFlag && flag.NArg() < 1 {
		Logf("Missing output directory argument")
		os.Exit(1)
	}

	var outputPath string

	if !dryRunFlag {
		outputPath = flag.Arg(0)

		Debugf("Starting quadlet-generator, output to: %s", outputPath)
	}

	sourcePaths := getUnitDirs(isUserFlag)

	units := make(map[string]*parser.UnitFile)
	for _, d := range sourcePaths {
		loadUnitsFromDir(d, units)
	}

	if len(units) == 0 {
		// containers/podman/issues/17374: exit cleanly but log that we
		// had nothing to do
		Debugf("No files to parse from %s", sourcePaths)
		os.Exit(0)
	}

	if !dryRunFlag {
		err := os.MkdirAll(outputPath, os.ModePerm)
		if err != nil {
			Logf("Can't create dir %s: %s", outputPath, err)
			os.Exit(1)
		}
	}

	for name, unit := range units {
		var service *parser.UnitFile
		var err error

		switch {
		case strings.HasSuffix(name, ".container"):
			warnIfAmbiguousName(unit)
			service, err = quadlet.ConvertContainer(unit, isUserFlag)
		case strings.HasSuffix(name, ".volume"):
			service, err = quadlet.ConvertVolume(unit, name)
		case strings.HasSuffix(name, ".kube"):
			service, err = quadlet.ConvertKube(unit, isUserFlag)
		case strings.HasSuffix(name, ".network"):
			service, err = quadlet.ConvertNetwork(unit, name)
		default:
			Logf("Unsupported file type '%s'", name)
			continue
		}

		if err != nil {
			Logf("Error converting '%s', ignoring: %s", name, err)
		} else {
			service.Path = path.Join(outputPath, service.Filename)

			if dryRunFlag {
				data, err := service.ToString()
				if err != nil {
					Debugf("Error parsing %s\n---\n", service.Path)
					exitCode = 1
				} else {
					fmt.Printf("---%s---\n%s\n", service.Path, data)
				}
			} else {
				if err := generateServiceFile(service); err != nil {
					Logf("Error writing '%s'o: %s", service.Path, err)
				}
				enableServiceFile(outputPath, service)
			}
		}
	}

	os.Exit(exitCode)
}

func init() {
	flag.BoolVar(&verboseFlag, "v", false, "Print debug information")
	flag.BoolVar(&noKmsgFlag, "no-kmsg-log", false, "Don't log to kmsg")
	flag.BoolVar(&isUserFlag, "user", false, "Run as systemd user")
	flag.BoolVar(&dryRunFlag, "dryrun", false, "Run in dryrun mode printing debug information")
	flag.BoolVar(&versionFlag, "version", false, "Print version information and exit")
}
